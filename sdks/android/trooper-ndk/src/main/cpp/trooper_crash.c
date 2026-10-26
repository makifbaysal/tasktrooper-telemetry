/*
 * TaskTrooper native crash handler. See trooper_crash.h and sdks/android/README.md ("Native crashes").
 *
 * Report format (text, one record per line, parsed by NativeCrashReport.kt):
 *
 *   TTNDK 1
 *   pid <n>
 *   tid <n>
 *   thread <name>
 *   signal <n>
 *   code <n>
 *   fault_addr 0x<hex>
 *   ts_ms <unix ms>
 *   abi <arm64-v8a|armeabi-v7a|x86_64|x86|host>
 *   unwinder <unwind|fp>
 *   ctx <context line>
 *   frame 0x<abs pc> 0x<pc - load bias> <module index | -1>
 *   module <index> <build-id hex | -> <path>
 *   end
 *
 * Async-signal-safety: the handler only uses write/read/getpid/gettid/prctl/clock_gettime/sigaction,
 * static memory and the module table captured outside the handler. `_Unwind_Backtrace` is not
 * formally async-signal-safe (it reads .eh_frame and may take the dl lock on first use of
 * dl_iterate_phdr inside libunwind); this is the same trade-off made by most crash reporters and we
 * warm it up at install time. If it fails to reach the faulting frame we fall back to a
 * frame-pointer walk from the signal ucontext with every memory read validated through a pipe.
 */
#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif

#include "trooper_crash.h"

#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#if defined(__APPLE__)
#include <sys/ucontext.h>
#else
#include <ucontext.h>
#endif
#include <unistd.h>
#include <unwind.h>

#if defined(__APPLE__)
#include <mach-o/dyld.h>
#include <mach-o/loader.h>
#else
#include <link.h>
#include <sys/prctl.h>
#include <sys/syscall.h>
#endif

#if defined(__aarch64__)
#define TT_ABI "arm64-v8a"
#elif defined(__arm__)
#define TT_ABI "armeabi-v7a"
#elif defined(__x86_64__)
#define TT_ABI "x86_64"
#elif defined(__i386__)
#define TT_ABI "x86"
#else
#define TT_ABI "unknown"
#endif

#if defined(__ANDROID__)
#define TT_ABI_NAME TT_ABI
#else
#define TT_ABI_NAME "host-" TT_ABI
#endif

/* ------------------------------------------------------------------ module table */

#define TT_PATH_POOL (128 * 1024)
#define TT_BUILD_ID_HEX 65

typedef struct {
    uintptr_t start;
    uintptr_t end;
    uintptr_t bias;
    uint32_t path_off;
    char build_id[TT_BUILD_ID_HEX];
} tt_module;

typedef struct {
    int count;
    uint32_t pool_used;
    tt_module mods[TT_NDK_MAX_MODULES];
    char pool[TT_PATH_POOL];
} tt_module_table;

static tt_module_table *g_tables[2];
static _Atomic int g_active_table = -1;
static pthread_mutex_t g_refresh_lock = PTHREAD_MUTEX_INITIALIZER;

static const int g_signals[] = {SIGSEGV, SIGABRT, SIGBUS, SIGILL, SIGFPE, SIGTRAP};
#define TT_NSIG ((int)(sizeof(g_signals) / sizeof(g_signals[0])))
static struct sigaction g_old[TT_NSIG];
static int g_installed;
static int g_fd = -1;
static int g_pipe[2] = {-1, -1};
static _Atomic int g_handling;
static void *g_altstack;

static char g_ctx[2][TT_NDK_CONTEXT_MAX];
static _Atomic int g_ctx_idx;
static pthread_mutex_t g_ctx_lock = PTHREAD_MUTEX_INITIALIZER;

static void tt_hex_bytes(const uint8_t *b, size_t n, char *out, size_t cap) {
    static const char hex[] = "0123456789abcdef";
    size_t j = 0;
    for (size_t i = 0; i < n && j + 2 < cap; i++) {
        out[j++] = hex[b[i] >> 4];
        out[j++] = hex[b[i] & 0xf];
    }
    out[j] = 0;
}

static int tt_table_add(tt_module_table *t, uintptr_t start, uintptr_t end, uintptr_t bias, const char *path,
                        const char *build_id) {
    if (t->count >= TT_NDK_MAX_MODULES) return 0;
    size_t len = path ? strlen(path) : 0;
    if (t->pool_used + len + 1 > TT_PATH_POOL) len = 0;
    tt_module *m = &t->mods[t->count++];
    m->start = start;
    m->end = end;
    m->bias = bias;
    m->path_off = t->pool_used;
    if (len) memcpy(t->pool + t->pool_used, path, len);
    t->pool[t->pool_used + len] = 0;
    t->pool_used += (uint32_t)len + 1;
    strncpy(m->build_id, build_id ? build_id : "", TT_BUILD_ID_HEX - 1);
    m->build_id[TT_BUILD_ID_HEX - 1] = 0;
    return 1;
}

#if defined(__APPLE__)
static void tt_capture_modules(tt_module_table *t) {
    uint32_t n = _dyld_image_count();
    for (uint32_t i = 0; i < n; i++) {
        const struct mach_header_64 *h = (const struct mach_header_64 *)_dyld_get_image_header(i);
        if (!h || h->magic != MH_MAGIC_64) continue;
        uintptr_t slide = (uintptr_t)_dyld_get_image_vmaddr_slide(i);
        uintptr_t lo = UINTPTR_MAX, hi = 0;
        char id[TT_BUILD_ID_HEX] = {0};
        const uint8_t *p = (const uint8_t *)(h + 1);
        for (uint32_t c = 0; c < h->ncmds; c++) {
            const struct load_command *lc = (const struct load_command *)p;
            if (lc->cmd == LC_SEGMENT_64) {
                const struct segment_command_64 *s = (const struct segment_command_64 *)lc;
                if (strcmp(s->segname, "__PAGEZERO") != 0 && strcmp(s->segname, "__LINKEDIT") != 0 && s->vmsize) {
                    if (s->vmaddr + slide < lo) lo = s->vmaddr + slide;
                    if (s->vmaddr + slide + s->vmsize > hi) hi = s->vmaddr + slide + s->vmsize;
                }
            } else if (lc->cmd == LC_UUID) {
                tt_hex_bytes(((const struct uuid_command *)lc)->uuid, 16, id, sizeof id);
            }
            p += lc->cmdsize;
        }
        if (hi > lo) tt_table_add(t, lo, hi, slide, _dyld_get_image_name(i), id);
    }
}
#else
static void tt_build_id(const struct dl_phdr_info *info, char *out) {
    out[0] = 0;
    for (int i = 0; i < info->dlpi_phnum; i++) {
        const ElfW(Phdr) *ph = &info->dlpi_phdr[i];
        if (ph->p_type != PT_NOTE) continue;
        const uint8_t *p = (const uint8_t *)(info->dlpi_addr + ph->p_vaddr);
        const uint8_t *end = p + ph->p_memsz;
        while (p + sizeof(ElfW(Nhdr)) <= end) {
            const ElfW(Nhdr) *n = (const ElfW(Nhdr) *)p;
            size_t name = (n->n_namesz + 3) & ~3u, desc = (n->n_descsz + 3) & ~3u;
            const uint8_t *nm = p + sizeof(ElfW(Nhdr));
            if (nm + name + desc > end) break;
            if (n->n_type == NT_GNU_BUILD_ID && n->n_namesz == 4 && memcmp(nm, "GNU", 4) == 0) {
                size_t len = n->n_descsz > 32 ? 32 : n->n_descsz;
                tt_hex_bytes(nm + name, len, out, TT_BUILD_ID_HEX);
                return;
            }
            p = nm + name + desc;
        }
    }
}

static int tt_phdr_cb(struct dl_phdr_info *info, size_t size, void *arg) {
    (void)size;
    tt_module_table *t = (tt_module_table *)arg;
    uintptr_t lo = UINTPTR_MAX, hi = 0;
    for (int i = 0; i < info->dlpi_phnum; i++) {
        const ElfW(Phdr) *ph = &info->dlpi_phdr[i];
        if (ph->p_type != PT_LOAD) continue;
        uintptr_t s = info->dlpi_addr + ph->p_vaddr;
        if (s < lo) lo = s;
        if (s + ph->p_memsz > hi) hi = s + ph->p_memsz;
    }
    if (hi <= lo) return 0;
    char id[TT_BUILD_ID_HEX];
    tt_build_id(info, id);
    tt_table_add(t, lo, hi, info->dlpi_addr, info->dlpi_name, id);
    return 0;
}

static void tt_capture_modules(tt_module_table *t) { dl_iterate_phdr(tt_phdr_cb, t); }
#endif

int tt_ndk_refresh_modules(void) {
    if (!g_tables[0]) return -1;
    pthread_mutex_lock(&g_refresh_lock);
    int next = atomic_load(&g_active_table) == 0 ? 1 : 0;
    tt_module_table *t = g_tables[next];
    t->count = 0;
    t->pool_used = 0;
    tt_capture_modules(t);
    atomic_store(&g_active_table, next);
    int n = t->count;
    pthread_mutex_unlock(&g_refresh_lock);
    return n;
}

/* ------------------------------------------------------------------ async-signal-safe writer */

typedef struct {
    int fd;
    size_t len;
    char buf[1024];
} tt_writer;

static void tt_flush(tt_writer *w) {
    size_t off = 0;
    while (off < w->len) {
        ssize_t r = write(w->fd, w->buf + off, w->len - off);
        if (r < 0 && errno == EINTR) continue;
        if (r <= 0) break;
        off += (size_t)r;
    }
    w->len = 0;
}

static void tt_put(tt_writer *w, const char *s) {
    while (s && *s) {
        if (w->len == sizeof(w->buf)) tt_flush(w);
        w->buf[w->len++] = *s++;
    }
}

/* Free text (thread names, context, paths): newlines would break the line format. */
static void tt_put_text(tt_writer *w, const char *s) {
    while (s && *s) {
        if (w->len == sizeof(w->buf)) tt_flush(w);
        char c = *s++;
        w->buf[w->len++] = (c == '\n' || c == '\r') ? ' ' : c;
    }
}

static void tt_putc(tt_writer *w, char c) {
    if (w->len == sizeof(w->buf)) tt_flush(w);
    w->buf[w->len++] = c;
}

static void tt_put_dec(tt_writer *w, long long v) {
    char tmp[24];
    int i = 0;
    unsigned long long u = v < 0 ? (unsigned long long)(-v) : (unsigned long long)v;
    do { tmp[i++] = (char)('0' + u % 10); u /= 10; } while (u);
    if (v < 0) tt_putc(w, '-');
    while (i) tt_putc(w, tmp[--i]);
}

static void tt_put_hex(tt_writer *w, uintptr_t v) {
    static const char hex[] = "0123456789abcdef";
    char tmp[2 * sizeof(uintptr_t)];
    int i = 0;
    do { tmp[i++] = hex[v & 0xf]; v >>= 4; } while (v);
    tt_put(w, "0x");
    while (i) tt_putc(w, tmp[--i]);
}

/* ------------------------------------------------------------------ unwinding */

typedef struct {
    uintptr_t *pcs;
    int n;
    int max;
} tt_unwind_state;

static _Unwind_Reason_Code tt_unwind_cb(struct _Unwind_Context *ctx, void *arg) {
    tt_unwind_state *st = (tt_unwind_state *)arg;
    uintptr_t pc = (uintptr_t)_Unwind_GetIP(ctx);
    if (pc == 0) return _URC_END_OF_STACK;
    if (st->n >= st->max) return _URC_END_OF_STACK;
    st->pcs[st->n++] = pc;
    return _URC_NO_REASON;
}

/* Reads one word without faulting: the kernel validates the source address of write(2). */
__attribute__((unused)) static int tt_safe_read(uintptr_t addr, uintptr_t *out) {
    if (g_pipe[0] < 0 || (addr & (sizeof(uintptr_t) - 1))) return 0;
    if (write(g_pipe[1], (const void *)addr, sizeof(uintptr_t)) != (ssize_t)sizeof(uintptr_t)) return 0;
    return read(g_pipe[0], out, sizeof(uintptr_t)) == (ssize_t)sizeof(uintptr_t);
}

static void tt_context_regs(void *uc_v, uintptr_t *pc, uintptr_t *fp, uintptr_t *lr) {
    *pc = *fp = *lr = 0;
    if (!uc_v) return;
    ucontext_t *uc = (ucontext_t *)uc_v;
#if defined(__APPLE__) && defined(__aarch64__)
    *pc = (uintptr_t)uc->uc_mcontext->__ss.__pc;
    *fp = (uintptr_t)uc->uc_mcontext->__ss.__fp;
    *lr = (uintptr_t)uc->uc_mcontext->__ss.__lr;
#elif defined(__APPLE__) && defined(__x86_64__)
    *pc = (uintptr_t)uc->uc_mcontext->__ss.__rip;
    *fp = (uintptr_t)uc->uc_mcontext->__ss.__rbp;
#elif defined(__aarch64__)
    *pc = (uintptr_t)uc->uc_mcontext.pc;
    *fp = (uintptr_t)uc->uc_mcontext.regs[29];
    *lr = (uintptr_t)uc->uc_mcontext.regs[30];
#elif defined(__arm__)
    *pc = (uintptr_t)uc->uc_mcontext.arm_pc;
    *fp = (uintptr_t)uc->uc_mcontext.arm_fp;
    *lr = (uintptr_t)uc->uc_mcontext.arm_lr;
#elif defined(__x86_64__)
    *pc = (uintptr_t)uc->uc_mcontext.gregs[REG_RIP];
    *fp = (uintptr_t)uc->uc_mcontext.gregs[REG_RBP];
#elif defined(__i386__)
    *pc = (uintptr_t)uc->uc_mcontext.gregs[REG_EIP];
    *fp = (uintptr_t)uc->uc_mcontext.gregs[REG_EBP];
#endif
}

static int tt_fp_walk(uintptr_t pc, uintptr_t fp, uintptr_t lr, uintptr_t *pcs, int max) {
    int n = 0;
    if (pc) pcs[n++] = pc;
    if (lr && n < max) pcs[n++] = lr;
#if !defined(__arm__)
    while (fp && n < max) {
        uintptr_t next = 0, ret = 0;
        if (!tt_safe_read(fp, &next) || !tt_safe_read(fp + sizeof(uintptr_t), &ret)) break;
        if (ret == 0) break;
#if defined(__aarch64__) && !defined(__APPLE__)
        ret &= 0x00ffffffffffffffULL; /* strip pointer-auth / tag bits */
#elif defined(__aarch64__)
        ret &= 0x0000000fffffffffULL;
#endif
        if (!(n == 2 && lr && ret == lr)) pcs[n++] = ret;
        if (next <= fp) break; /* stacks grow down: frame records must move up */
        fp = next;
    }
#else
    (void)fp;
#endif
    return n;
}

/* Collects the crashing thread's frames, starting at the faulting pc. Returns the count. */
static int tt_collect(void *uc, uintptr_t *pcs, int max, const char **unwinder) {
    uintptr_t pc, fp, lr;
    tt_context_regs(uc, &pc, &fp, &lr);
#if defined(__arm__)
    pc &= ~(uintptr_t)1;
#endif
    uintptr_t raw[TT_NDK_MAX_FRAMES + 16];
    tt_unwind_state st = {raw, 0, (int)(sizeof raw / sizeof raw[0])};
    _Unwind_Backtrace(tt_unwind_cb, &st);
    if (!uc) { /* test reports: no signal frame, keep the caller's stack */
        int n = 0;
        for (int i = 0; i < st.n && n < max; i++) pcs[n++] = raw[i];
        *unwinder = "unwind";
        return n;
    }
    int start = -1;
    for (int i = 0; i < st.n; i++) {
        uintptr_t v = raw[i];
#if defined(__arm__)
        v &= ~(uintptr_t)1;
#endif
        if (pc && v == pc) { start = i; break; }
    }
    if (start >= 0 && st.n - start >= 2) {
        int n = 0;
        for (int i = start; i < st.n && n < max; i++) pcs[n++] = raw[i];
        *unwinder = "unwind";
        return n;
    }
    *unwinder = "fp";
    return tt_fp_walk(pc, fp, lr, pcs, max);
}

static const tt_module *tt_find_module(const tt_module_table *t, uintptr_t pc, int *index) {
    if (!t) return NULL;
    for (int i = 0; i < t->count; i++) {
        if (pc >= t->mods[i].start && pc < t->mods[i].end) {
            *index = i;
            return &t->mods[i];
        }
    }
    return NULL;
}

/* ------------------------------------------------------------------ report */

static long long tt_gettid(void) {
#if defined(__APPLE__)
    uint64_t tid = 0;
    pthread_threadid_np(NULL, &tid);
    return (long long)tid;
#else
    return (long long)syscall(SYS_gettid);
#endif
}

static void tt_thread_name(char *out, size_t cap) {
    out[0] = 0;
#if defined(__APPLE__)
    pthread_getname_np(pthread_self(), out, cap);
#else
    char tmp[17] = {0};
    if (prctl(PR_GET_NAME, (unsigned long)tmp, 0, 0, 0) == 0) {
        strncpy(out, tmp, cap - 1);
        out[cap - 1] = 0;
    }
#endif
}

static int tt_write_report(int sig, siginfo_t *info, void *uc) {
    if (g_fd < 0) return -1;
    tt_writer w;
    w.fd = g_fd;
    w.len = 0;

    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    char tname[64];
    tt_thread_name(tname, sizeof tname);

    tt_put(&w, "TTNDK 1\npid ");
    tt_put_dec(&w, (long long)getpid());
    tt_put(&w, "\ntid ");
    tt_put_dec(&w, tt_gettid());
    tt_put(&w, "\nthread ");
    tt_put_text(&w, tname);
    tt_put(&w, "\nsignal ");
    tt_put_dec(&w, sig);
    tt_put(&w, "\ncode ");
    tt_put_dec(&w, info ? info->si_code : 0);
    tt_put(&w, "\nfault_addr ");
    tt_put_hex(&w, info ? (uintptr_t)info->si_addr : 0);
    tt_put(&w, "\nts_ms ");
    tt_put_dec(&w, (long long)ts.tv_sec * 1000 + ts.tv_nsec / 1000000);
    tt_put(&w, "\nabi " TT_ABI_NAME "\n");

    uintptr_t pcs[TT_NDK_MAX_FRAMES];
    const char *unwinder = "fp";
    int n = tt_collect(uc, pcs, TT_NDK_MAX_FRAMES, &unwinder);
    tt_put(&w, "unwinder ");
    tt_put(&w, unwinder);
    tt_put(&w, "\nctx ");
    tt_put_text(&w, g_ctx[atomic_load(&g_ctx_idx)]);
    tt_putc(&w, '\n');

    int ti = atomic_load(&g_active_table);
    const tt_module_table *t = ti >= 0 ? g_tables[ti] : NULL;
    int used[TT_NDK_MAX_FRAMES];
    int nused = 0;
    for (int i = 0; i < n; i++) {
        int mi = -1;
        const tt_module *m = tt_find_module(t, pcs[i], &mi);
        tt_put(&w, "frame ");
        tt_put_hex(&w, pcs[i]);
        tt_putc(&w, ' ');
        tt_put_hex(&w, m ? pcs[i] - m->bias : pcs[i]);
        tt_putc(&w, ' ');
        tt_put_dec(&w, mi);
        tt_putc(&w, '\n');
        if (mi >= 0) {
            int seen = 0;
            for (int j = 0; j < nused; j++) seen |= used[j] == mi;
            if (!seen) used[nused++] = mi;
        }
    }
    for (int j = 0; j < nused; j++) {
        const tt_module *m = &t->mods[used[j]];
        tt_put(&w, "module ");
        tt_put_dec(&w, used[j]);
        tt_putc(&w, ' ');
        tt_put(&w, m->build_id[0] ? m->build_id : "-");
        tt_putc(&w, ' ');
        tt_put_text(&w, t->pool + m->path_off);
        tt_putc(&w, '\n');
    }
    tt_put(&w, "end\n");
    tt_flush(&w);
    return 0;
}

/* ------------------------------------------------------------------ handlers */

static int tt_sig_index(int sig) {
    for (int i = 0; i < TT_NSIG; i++)
        if (g_signals[i] == sig) return i;
    return -1;
}

static void tt_reraise(int sig) {
#if defined(__APPLE__)
    pthread_kill(pthread_self(), sig);
#else
    syscall(SYS_tgkill, getpid(), (pid_t)syscall(SYS_gettid), sig);
#endif
}

static void tt_chain(int sig, siginfo_t *info, void *uc) {
    int i = tt_sig_index(sig);
    if (i < 0) return;
    struct sigaction *old = &g_old[i];
    /* Put the previous handler back so a re-fault (or the re-raise below) reaches it. */
    sigaction(sig, old, NULL);
    if ((old->sa_flags & SA_SIGINFO) && old->sa_sigaction) {
        old->sa_sigaction(sig, info, uc);
        return;
    }
    if (!(old->sa_flags & SA_SIGINFO) && old->sa_handler != SIG_DFL && old->sa_handler != SIG_IGN) {
        old->sa_handler(sig);
        return;
    }
    if (old->sa_handler == SIG_IGN) signal(sig, SIG_DFL);
    /* Synchronous faults re-execute the faulting instruction on return; sent signals must be re-raised. */
    if (info == NULL || info->si_code <= 0 || sig == SIGABRT) tt_reraise(sig);
}

static void tt_handler(int sig, siginfo_t *info, void *uc) {
    int saved_errno = errno;
    int expected = 0;
    if (atomic_compare_exchange_strong(&g_handling, &expected, 1)) {
        tt_write_report(sig, info, uc);
    } else {
        /* Another thread is writing its report: give it time to finish before we chain and die. */
        struct timespec d = {0, 200 * 1000 * 1000};
        nanosleep(&d, NULL);
    }
    tt_chain(sig, info, uc);
    errno = saved_errno;
}

static _Unwind_Reason_Code tt_warmup_cb(struct _Unwind_Context *ctx, void *arg) {
    (void)ctx;
    return ++*(int *)arg > 2 ? _URC_END_OF_STACK : _URC_NO_REASON;
}

int tt_ndk_install(const char *report_path) {
    if (g_installed) return 0;
    if (!report_path) return -1;
    if (!g_tables[0]) {
        for (int i = 0; i < 2; i++) {
            void *p = mmap(NULL, sizeof(tt_module_table), PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
            if (p == MAP_FAILED) return -1;
            g_tables[i] = (tt_module_table *)p;
        }
    }
    g_fd = open(report_path, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
    if (g_fd < 0) return -1;
    if (g_pipe[0] < 0) {
        if (pipe(g_pipe) != 0) {
            g_pipe[0] = g_pipe[1] = -1;
        } else {
            for (int i = 0; i < 2; i++) {
                fcntl(g_pipe[i], F_SETFL, fcntl(g_pipe[i], F_GETFL) | O_NONBLOCK);
                fcntl(g_pipe[i], F_SETFD, FD_CLOEXEC);
            }
        }
    }
    tt_ndk_refresh_modules();
    /* Warm up libunwind (lazy .eh_frame_hdr lookup structures) outside of a signal context. */
    int depth = 0;
    _Unwind_Backtrace(tt_warmup_cb, &depth);

    /* Alternate stack for the installing thread; bionic gives every pthread its own sigaltstack. */
    stack_t cur;
    if (sigaltstack(NULL, &cur) == 0 && (cur.ss_flags & SS_DISABLE)) {
        size_t sz = 64 * 1024;
        g_altstack = mmap(NULL, sz, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
        if (g_altstack != MAP_FAILED) {
            stack_t ss = {0};
            ss.ss_sp = g_altstack;
            ss.ss_size = sz;
            sigaltstack(&ss, NULL);
        } else {
            g_altstack = NULL;
        }
    }

    struct sigaction sa;
    memset(&sa, 0, sizeof sa);
    sigemptyset(&sa.sa_mask);
    for (int i = 0; i < TT_NSIG; i++) sigaddset(&sa.sa_mask, g_signals[i]);
    sa.sa_sigaction = tt_handler;
    sa.sa_flags = SA_SIGINFO | SA_ONSTACK;
    for (int i = 0; i < TT_NSIG; i++) {
        if (sigaction(g_signals[i], &sa, &g_old[i]) != 0) memset(&g_old[i], 0, sizeof g_old[i]);
    }
    g_installed = 1;
    return 0;
}

void tt_ndk_uninstall(void) {
    if (!g_installed) return;
    for (int i = 0; i < TT_NSIG; i++) sigaction(g_signals[i], &g_old[i], NULL);
    if (g_fd >= 0) close(g_fd);
    g_fd = -1;
    g_installed = 0;
    atomic_store(&g_handling, 0);
}

void tt_ndk_set_context(const char *context) {
    pthread_mutex_lock(&g_ctx_lock);
    int next = atomic_load(&g_ctx_idx) == 0 ? 1 : 0;
    char *dst = g_ctx[next];
    size_t i = 0;
    if (context) {
        for (; context[i] && i < TT_NDK_CONTEXT_MAX - 1; i++) {
            char c = context[i];
            dst[i] = (c == '\n' || c == '\r') ? ' ' : c;
        }
    }
    dst[i] = 0;
    atomic_store(&g_ctx_idx, next);
    pthread_mutex_unlock(&g_ctx_lock);
}

int tt_ndk_write_test_report(int sig) {
    if (g_fd < 0) return -1;
    siginfo_t info;
    memset(&info, 0, sizeof info);
    info.si_signo = sig;
    return tt_write_report(sig, &info, NULL);
}

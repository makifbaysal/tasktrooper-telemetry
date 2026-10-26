// Async-signal-safe crash report writer.
//
// Everything the signal handler touches is allocated up front: the report file descriptor is opened at
// install time, the frame buffer and binary image table are static, and output is formatted by hand
// (no printf/malloc/locks). Memory reads while walking the stack go through vm_read_overwrite so a
// corrupt frame pointer cannot fault inside the handler.
//
// All threads: the handler enumerates the task's threads with task_threads(), suspends every other
// thread, reads its registers with thread_get_state() and walks its frame-pointer chain. task_threads()
// is a Mach call that allocates the returned port array with vm_allocate (kernel side, no malloc lock)
// and is used by every production crash reporter (PLCrashReporter, KSCrash), but it is not on the POSIX
// async-signal-safe list; if it fails the report still contains the crashed thread. The array and the
// thread ports are released with vm_deallocate / mach_port_deallocate.
//
// Crash messages: Swift's fatalError / preconditionFailure and abort() annotations are stored in the
// `__DATA,__crash_info` section (crashreporter_annotations_t) of libswiftCore / libsystem_c. The section
// address of every image is computed at load time, so the handler only reads memory.

#include "TTCrash.h"

#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdlib.h>
#include <string.h>
#include <sys/sysctl.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>
#include <dlfcn.h>
#include <mach/mach.h>
#include <mach-o/dyld.h>
#include <mach-o/loader.h>
#include <zlib.h>
#include <TargetConditionals.h>

#if __has_feature(ptrauth_calls)
#include <ptrauth.h>
#endif

#include <mach-o/getsect.h>

#define TT_MAX_FRAMES 128
#define TT_MAX_IMAGES 4096
#define TT_MAX_THREADS 64
#define TT_THREAD_FRAMES 64
#define TT_CRASH_INFO_MAX 1024

typedef struct {
    uintptr_t load;
    uintptr_t size;
    const char *path;
    uint8_t uuid[16];
    uintptr_t crash_info;  // address of __DATA,__crash_info (0 if none)
} tt_image_t;

// Layout of crashreporter_annotations_t (CrashReporterClient.h); fields are 64-bit on all versions.
typedef struct {
    uint64_t version;
    uint64_t message;
    uint64_t signature_string;
    uint64_t backtrace;
    uint64_t message2;
    uint64_t thread;
    uint64_t dialog_mode;
    uint64_t abort_cause;
} tt_crash_annotations_t;

// Our own annotation slot: lets tests (and host code via tt_crash_set_annotation) exercise the
// __crash_info reader exactly as it reads libswiftCore's.
__attribute__((used, section("__DATA,__crash_info")))
static tt_crash_annotations_t g_own_annotations = {5, 0, 0, 0, 0, 0, 0, 0};
static char g_own_annotation_message[512];

static int g_fd = -1;
static volatile sig_atomic_t g_written = 0;
static int g_signals_installed = 0;
static int g_images_registered = 0;

static char g_session[128];
static char g_view_id[128];
static char g_view_name[256];
static char g_user[256];

// Uncaught NSException data (filled outside of signal context by the ObjC handler).
char tt_exc_name[256];
char tt_exc_reason[2048];
uintptr_t tt_exc_frames[TT_MAX_FRAMES];
int tt_exc_frame_count = 0;
int tt_exc_present = 0;

static uintptr_t g_frames[TT_MAX_FRAMES];
static uintptr_t g_thread_frames[TT_MAX_THREADS][TT_THREAD_FRAMES];
static int g_thread_frame_counts[TT_MAX_THREADS];
static int g_thread_is_main[TT_MAX_THREADS];
static int g_thread_count = 0;
static mach_port_t g_main_thread = MACH_PORT_NULL;
static char g_crash_info_buf[TT_CRASH_INFO_MAX];
static tt_image_t g_images[TT_MAX_IMAGES];
static volatile int g_image_count = 0;
static pthread_mutex_t g_image_lock = PTHREAD_MUTEX_INITIALIZER;

static const int g_fatal_signals[] = {SIGABRT, SIGSEGV, SIGBUS, SIGILL, SIGTRAP, SIGFPE};
#define TT_SIGNAL_COUNT (int)(sizeof(g_fatal_signals) / sizeof(g_fatal_signals[0]))
static struct sigaction g_prev_actions[TT_SIGNAL_COUNT];
static stack_t g_alt_stack;

// ---------------------------------------------------------------------------------------------
// Binary images (dyld callback; not signal context, may lock)

static void tt_add_image(const struct mach_header *header, intptr_t slide) {
    (void)slide;
#if __LP64__
    const struct mach_header_64 *mh = (const struct mach_header_64 *)header;
    if (mh->magic != MH_MAGIC_64) return;
    tt_image_t img;
    memset(&img, 0, sizeof(img));
    img.load = (uintptr_t)mh;
    const uint8_t *cmd = (const uint8_t *)(mh + 1);
    for (uint32_t i = 0; i < mh->ncmds; i++) {
        const struct load_command *lc = (const struct load_command *)cmd;
        if (lc->cmd == LC_SEGMENT_64) {
            const struct segment_command_64 *seg = (const struct segment_command_64 *)lc;
            if (strcmp(seg->segname, "__TEXT") == 0) img.size = (uintptr_t)seg->vmsize;
        } else if (lc->cmd == LC_UUID) {
            memcpy(img.uuid, ((const struct uuid_command *)lc)->uuid, 16);
        }
        cmd += lc->cmdsize;
    }
    unsigned long sect_size = 0;
    uint8_t *ci = getsectiondata(mh, "__DATA", "__crash_info", &sect_size);
    if (ci && sect_size >= 5 * sizeof(uint64_t)) img.crash_info = (uintptr_t)ci;
    Dl_info info;
    if (dladdr(mh, &info) && info.dli_fname) img.path = info.dli_fname;
    pthread_mutex_lock(&g_image_lock);
    int n = g_image_count;
    if (n < TT_MAX_IMAGES) {
        g_images[n] = img;
        __atomic_store_n(&g_image_count, n + 1, __ATOMIC_RELEASE);
    }
    pthread_mutex_unlock(&g_image_lock);
#else
    (void)header;
#endif
}

// ---------------------------------------------------------------------------------------------
// Async-signal-safe output helpers

static void w_raw(const char *s, size_t n) {
    while (n > 0) {
        ssize_t r = write(g_fd, s, n);
        if (r <= 0) {
            if (r < 0 && errno == EINTR) continue;
            return;
        }
        s += r;
        n -= (size_t)r;
    }
}

static void w_str(const char *s) {
    if (s) w_raw(s, strlen(s));
}

static void w_hex(uintptr_t v) {
    char buf[2 + 16];
    static const char digits[] = "0123456789abcdef";
    buf[0] = '0';
    buf[1] = 'x';
    for (int i = 0; i < 16; i++) buf[2 + i] = digits[(v >> ((15 - i) * 4)) & 0xF];
    w_raw(buf, sizeof(buf));
}

static void w_dec(int64_t v) {
    char buf[24];
    int i = (int)sizeof(buf);
    int neg = v < 0;
    uint64_t u = neg ? (uint64_t)(-(v + 1)) + 1 : (uint64_t)v;
    do {
        buf[--i] = (char)('0' + (u % 10));
        u /= 10;
    } while (u && i > 1);
    if (neg) buf[--i] = '-';
    w_raw(buf + i, sizeof(buf) - (size_t)i);
}

static void w_kv(const char *key, const char *value) {
    w_str(key);
    w_str(" ");
    w_str(value);
    w_str("\n");
}

static const char *tt_signal_name(int sig) {
    switch (sig) {
        case SIGABRT: return "SIGABRT";
        case SIGSEGV: return "SIGSEGV";
        case SIGBUS: return "SIGBUS";
        case SIGILL: return "SIGILL";
        case SIGTRAP: return "SIGTRAP";
        case SIGFPE: return "SIGFPE";
        default: return "SIGNAL";
    }
}

static uintptr_t tt_strip(uintptr_t v) {
#if __has_feature(ptrauth_calls)
    return (uintptr_t)ptrauth_strip((void *)v, ptrauth_key_return_address);
#else
    return v;
#endif
}

static int tt_read_mem(uintptr_t addr, void *dst, size_t len) {
    vm_size_t out = 0;
    kern_return_t kr = vm_read_overwrite(mach_task_self(), (vm_address_t)addr, (vm_size_t)len, (vm_address_t)dst, &out);
    return (kr == KERN_SUCCESS && out == len) ? 0 : -1;
}

static int tt_walk_stack(uintptr_t pc, uintptr_t fp, uintptr_t lr, uintptr_t *out, int max) {
    int n = 0;
    if (pc) out[n++] = tt_strip(pc);
    uintptr_t rec[2];
    int first = 1;
    while (fp && n < max && (fp & (sizeof(uintptr_t) - 1)) == 0) {
        if (tt_read_mem(fp, rec, sizeof(rec)) != 0) break;
        uintptr_t ret = tt_strip(rec[1]);
        if (first) {
            first = 0;
            uintptr_t l = tt_strip(lr);
            if (l && l != ret && n < max) out[n++] = l;  // leaf function without a frame record
        }
        if (ret == 0) break;
        if (n < max) out[n++] = ret;
        if (rec[0] <= fp) break;  // stack grows down: caller frames live at higher addresses
        fp = rec[0];
    }
    return n;
}


/// Registers of a suspended (or other) thread → pc/fp/lr. Returns 0 on success.
static int tt_thread_registers(thread_t thread, uintptr_t *pc, uintptr_t *fp, uintptr_t *lr) {
#if defined(__arm64__)
    arm_thread_state64_t st;
    mach_msg_type_number_t count = ARM_THREAD_STATE64_COUNT;
    if (thread_get_state(thread, ARM_THREAD_STATE64, (thread_state_t)&st, &count) != KERN_SUCCESS) return -1;
    *pc = (uintptr_t)__darwin_arm_thread_state64_get_pc(st);
    *fp = (uintptr_t)__darwin_arm_thread_state64_get_fp(st);
    *lr = (uintptr_t)__darwin_arm_thread_state64_get_lr(st);
    return 0;
#elif defined(__x86_64__)
    x86_thread_state64_t st;
    mach_msg_type_number_t count = x86_THREAD_STATE64_COUNT;
    if (thread_get_state(thread, x86_THREAD_STATE64, (thread_state_t)&st, &count) != KERN_SUCCESS) return -1;
    *pc = (uintptr_t)st.__rip;
    *fp = (uintptr_t)st.__rbp;
    *lr = 0;
    return 0;
#else
    (void)thread; (void)pc; (void)fp; (void)lr;
    return -1;
#endif
}

// Threads captured by tt_capture_other_threads(); they stay suspended until tt_resume_threads().
static thread_act_array_t g_threads = NULL;
static mach_msg_type_number_t g_threads_n = 0;

static int tt_capture_other_threads(void) {
    g_thread_count = 0;
    g_threads = NULL;
    g_threads_n = 0;
    thread_t self = mach_thread_self();
    if (task_threads(mach_task_self(), &g_threads, &g_threads_n) != KERN_SUCCESS) {
        g_threads = NULL;
        g_threads_n = 0;
        mach_port_deallocate(mach_task_self(), self);
        return 0;
    }
    for (mach_msg_type_number_t i = 0; i < g_threads_n; i++) {
        if (g_threads[i] != self) thread_suspend(g_threads[i]);
    }
    for (mach_msg_type_number_t i = 0; i < g_threads_n && g_thread_count < TT_MAX_THREADS; i++) {
        thread_t t = g_threads[i];
        if (t == self) continue;
        uintptr_t pc = 0, fp = 0, lr = 0;
        int idx = g_thread_count;
        g_thread_frame_counts[idx] = 0;
        g_thread_is_main[idx] = (t == g_main_thread);
        if (tt_thread_registers(t, &pc, &fp, &lr) == 0) {
            g_thread_frame_counts[idx] = tt_walk_stack(pc, fp, lr, g_thread_frames[idx], TT_THREAD_FRAMES);
        }
        g_thread_count++;
    }
    mach_port_deallocate(mach_task_self(), self);
    return g_thread_count;
}

static void tt_resume_threads(void) {
    if (!g_threads) return;
    thread_t self = mach_thread_self();
    for (mach_msg_type_number_t i = 0; i < g_threads_n; i++) {
        if (g_threads[i] != self) thread_resume(g_threads[i]);
        mach_port_deallocate(mach_task_self(), g_threads[i]);
    }
    mach_port_deallocate(mach_task_self(), self);
    vm_deallocate(mach_task_self(), (vm_address_t)g_threads, g_threads_n * sizeof(thread_t));
    g_threads = NULL;
    g_threads_n = 0;
}

/// Copies a C string from (possibly invalid) memory. Returns its length.
static size_t tt_read_cstring(uintptr_t addr, char *dst, size_t cap) {
    size_t n = 0;
    char chunk[64];
    while (addr && n + 1 < cap) {
        // Never cross a page boundary in one read (the next page may be unmapped).
        size_t to_page = 4096 - (addr & 4095);
        size_t len = to_page < sizeof(chunk) ? to_page : sizeof(chunk);
        if (tt_read_mem(addr, chunk, len) != 0) break;
        for (size_t i = 0; i < len; i++) {
            if (chunk[i] == 0 || n + 1 >= cap) { dst[n] = 0; return n; }
            dst[n++] = chunk[i];
        }
        addr += len;
    }
    dst[n] = 0;
    return n;
}

/// Writes "crash_info <image>: <message>" lines for every image with a non-empty annotation.
static void tt_write_crash_info(void) {
    int count = __atomic_load_n(&g_image_count, __ATOMIC_ACQUIRE);
    for (int i = 0; i < count; i++) {
        const tt_image_t *img = &g_images[i];
        if (!img->crash_info) continue;
        tt_crash_annotations_t a;
        memset(&a, 0, sizeof(a));
        if (tt_read_mem(img->crash_info, &a, 5 * sizeof(uint64_t)) != 0) continue;
        uint64_t msgs[2] = {a.message, a.message2};
        for (int m = 0; m < 2; m++) {
            if (!msgs[m]) continue;
            size_t n = tt_read_cstring((uintptr_t)msgs[m], g_crash_info_buf, sizeof(g_crash_info_buf));
            if (n == 0) continue;
            w_str("crash_info ");
            const char *name = img->path ? img->path : "?";
            const char *slash = name;
            for (const char *p = name; *p; p++) if (*p == '/') slash = p + 1;
            w_str(slash);
            w_str(": ");
            for (size_t k = 0; k < n; k++) {
                char c = g_crash_info_buf[k];
                if (c == '\n') w_raw("\\n", 2);
                else if (c != '\r') w_raw(&c, 1);
            }
            w_str("\n");
        }
    }
}

static int tt_frame_in_image(const tt_image_t *img, uintptr_t f) {
    return f >= img->load && f < img->load + img->size;
}

static int tt_image_used(const tt_image_t *img, const uintptr_t *frames, int frame_count) {
    for (int f = 0; f < frame_count; f++) if (tt_frame_in_image(img, frames[f])) return 1;
    for (int t = 0; t < g_thread_count; t++) {
        for (int f = 0; f < g_thread_frame_counts[t]; f++) if (tt_frame_in_image(img, g_thread_frames[t][f])) return 1;
    }
    return 0;
}

static void tt_write_uuid(const uint8_t *u) {
    static const char digits[] = "0123456789abcdef";
    char buf[32];
    for (int i = 0; i < 16; i++) {
        buf[i * 2] = digits[u[i] >> 4];
        buf[i * 2 + 1] = digits[u[i] & 0xF];
    }
    w_raw(buf, sizeof(buf));
}

/// Core writer. Must stay async-signal-safe.
static void tt_write_report(int sig, int64_t code, uintptr_t fault, const uintptr_t *frames, int frame_count, int with_exception) {
    if (g_fd < 0) return;
    lseek(g_fd, 0, SEEK_SET);
    w_str("TTCRASH 1\n");
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    w_str("ts ");
    w_dec((int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000);
    w_str("\nsignal ");
    w_dec(sig);
    w_str(" ");
    w_str(tt_signal_name(sig));
    w_str("\ncode ");
    w_dec(code);
    w_str("\naddr ");
    w_hex(fault);
    w_str("\n");
    w_kv("session", g_session);
    w_kv("view_id", g_view_id);
    w_kv("view_name", g_view_name);
    w_kv("user", g_user);
    if (with_exception && tt_exc_present) {
        w_kv("exception_name", tt_exc_name);
        w_kv("exception_reason", tt_exc_reason);
    }
    tt_write_crash_info();
    w_kv("thread", pthread_main_np() ? "main" : "background");
    for (int i = 0; i < frame_count; i++) {
        w_str("frame ");
        w_hex(frames[i]);
        w_str("\n");
    }
    // Other threads (captured by tt_capture_other_threads; may be empty).
    for (int t = 0; t < g_thread_count; t++) {
        w_str("thread_begin ");
        w_str(g_thread_is_main[t] ? "main" : "background");
        w_str("\n");
        for (int i = 0; i < g_thread_frame_counts[t]; i++) {
            w_str("tframe ");
            w_hex(g_thread_frames[t][i]);
            w_str("\n");
        }
    }
    int count = __atomic_load_n(&g_image_count, __ATOMIC_ACQUIRE);
    for (int i = 0; i < count; i++) {
        const tt_image_t *img = &g_images[i];
        if (!tt_image_used(img, frames, frame_count)) continue;
        w_str("image ");
        w_hex(img->load);
        w_str(" ");
        w_hex(img->size);
        w_str(" ");
        tt_write_uuid(img->uuid);
        w_str(" ");
        w_str(img->path ? img->path : "?");
        w_str("\n");
    }
    w_str("end\n");
    fsync(g_fd);
}

// Called by the ObjC exception handler (TTException.m), outside signal context.
void tt_crash_write_exception_frames(void) {
    if (g_written) return;
    g_written = 1;
    tt_capture_other_threads();
    tt_write_report(SIGABRT, 0, 0, tt_exc_frames, tt_exc_frame_count, 1);
    tt_resume_threads();
}

// ---------------------------------------------------------------------------------------------
// Signal handling

static void tt_restore_signal_handlers(void) {
    if (!g_signals_installed) return;
    for (int i = 0; i < TT_SIGNAL_COUNT; i++) sigaction(g_fatal_signals[i], &g_prev_actions[i], NULL);
    g_signals_installed = 0;
}

static void tt_signal_handler(int sig, siginfo_t *info, void *uap) {
    if (!g_written && g_fd >= 0) {
        g_written = 1;
        uintptr_t pc = 0, fp = 0, lr = 0;
        ucontext_t *uc = (ucontext_t *)uap;
        if (uc && uc->uc_mcontext) {
#if defined(__arm64__)
            pc = (uintptr_t)__darwin_arm_thread_state64_get_pc(uc->uc_mcontext->__ss);
            fp = (uintptr_t)__darwin_arm_thread_state64_get_fp(uc->uc_mcontext->__ss);
            lr = (uintptr_t)__darwin_arm_thread_state64_get_lr(uc->uc_mcontext->__ss);
#elif defined(__x86_64__)
            pc = (uintptr_t)uc->uc_mcontext->__ss.__rip;
            fp = (uintptr_t)uc->uc_mcontext->__ss.__rbp;
#endif
        }
        int n = tt_walk_stack(pc, fp, lr, g_frames, TT_MAX_FRAMES);
        tt_capture_other_threads();
        tt_write_report(sig, info ? info->si_code : 0, info ? (uintptr_t)info->si_addr : 0, g_frames, n, 1);
        tt_resume_threads();
    }
    // Chain: restore the previous dispositions. Hardware faults re-execute the faulting instruction
    // and reach the previous handler; software signals (abort/raise/kill) are re-raised.
    tt_restore_signal_handlers();
    if (sig == SIGABRT || (info && info->si_code == SI_USER)) raise(sig);
}

static void tt_install_signal_handlers(void) {
    if (g_signals_installed) return;
#if !TARGET_OS_TV
    size_t alt_size = SIGSTKSZ > 65536 ? SIGSTKSZ : 65536;
    if (g_alt_stack.ss_sp == NULL) {
        g_alt_stack.ss_sp = malloc(alt_size);
        g_alt_stack.ss_size = alt_size;
        g_alt_stack.ss_flags = 0;
    }
    if (g_alt_stack.ss_sp) sigaltstack(&g_alt_stack, NULL);  // main thread; lets us survive stack overflow there
#endif
    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_sigaction = tt_signal_handler;
    sa.sa_flags = SA_SIGINFO | SA_ONSTACK;
    sigemptyset(&sa.sa_mask);
    for (int i = 0; i < TT_SIGNAL_COUNT; i++) sigaction(g_fatal_signals[i], &sa, &g_prev_actions[i]);
    g_signals_installed = 1;
}

// Implemented in TTException.m
void tt_install_exception_handler(void);
void tt_uninstall_exception_handler(void);

// ---------------------------------------------------------------------------------------------
// Public API

/// Records the main thread's port. On the main thread this is mach_thread_self(); otherwise the first
/// thread of the task (task_threads returns threads in creation order, the main thread first).
static void tt_find_main_thread(void) {
    if (g_main_thread != MACH_PORT_NULL) return;
    if (pthread_main_np()) {
        g_main_thread = mach_thread_self();  // keep the send right for the process lifetime
        return;
    }
    thread_act_array_t threads = NULL;
    mach_msg_type_number_t n = 0;
    if (task_threads(mach_task_self(), &threads, &n) != KERN_SUCCESS || n == 0) return;
    g_main_thread = threads[0];
    for (mach_msg_type_number_t i = 1; i < n; i++) mach_port_deallocate(mach_task_self(), threads[i]);
    vm_deallocate(mach_task_self(), (vm_address_t)threads, n * sizeof(thread_t));
}

__attribute__((constructor)) static void tt_crash_constructor(void) { tt_find_main_thread(); }

void tt_images_register(void) {
    tt_find_main_thread();
    if (!g_images_registered) {
        g_images_registered = 1;
        _dyld_register_func_for_add_image(tt_add_image);
    }
}

int tt_crash_install(const char *report_path, int install_signal_handlers, int install_exception_handler) {
    if (!report_path) return -1;
    if (g_fd >= 0) close(g_fd);
    g_fd = open(report_path, O_WRONLY | O_CREAT | O_TRUNC, 0644);
    if (g_fd < 0) return -1;
    g_written = 0;
    tt_images_register();
    if (install_signal_handlers) tt_install_signal_handlers();
    if (install_exception_handler) tt_install_exception_handler();
    return 0;
}

void tt_crash_uninstall(void) {
    tt_restore_signal_handlers();
    tt_uninstall_exception_handler();
    if (g_fd >= 0) {
        close(g_fd);
        g_fd = -1;
    }
}

static void tt_copy(char *dst, size_t cap, const char *src) {
    if (!src) src = "";
    strlcpy(dst, src, cap);
    for (char *p = dst; *p; p++) {
        if (*p == '\n' || *p == '\r') *p = ' ';
    }
}

void tt_crash_set_context(const char *session_id, const char *view_id, const char *view_name, const char *user_id) {
    tt_copy(g_session, sizeof(g_session), session_id);
    tt_copy(g_view_id, sizeof(g_view_id), view_id);
    tt_copy(g_view_name, sizeof(g_view_name), view_name);
    tt_copy(g_user, sizeof(g_user), user_id);
}

int tt_crash_report_written(void) { return g_written ? 1 : 0; }

__attribute__((noinline)) int tt_crash_write_test_report(int sig, uintptr_t fault_address) {
    if (g_fd < 0) return -1;
    uintptr_t pc = (uintptr_t)__builtin_return_address(0);
    uintptr_t fp = (uintptr_t)__builtin_frame_address(1);
    int n = tt_walk_stack(pc, fp, 0, g_frames, TT_MAX_FRAMES);
    tt_capture_other_threads();
    tt_write_report(sig, 1, fault_address, g_frames, n, 0);
    tt_resume_threads();
    return 0;
}

void tt_crash_set_annotation(const char *message) {
    if (!message || !*message) {
        g_own_annotations.message = 0;
        return;
    }
    strlcpy(g_own_annotation_message, message, sizeof(g_own_annotation_message));
    g_own_annotations.message = (uint64_t)(uintptr_t)g_own_annotation_message;
}

mach_port_t tt_main_thread_port(void) {
    tt_images_register();
    return g_main_thread;
}

int tt_capture_thread_stack(mach_port_t thread, uintptr_t *out, int max) {
    if (!out || max <= 0 || thread == MACH_PORT_NULL) return 0;
    thread_t self = mach_thread_self();
    mach_port_deallocate(mach_task_self(), self);
    if (thread == self) return 0;
    if (thread_suspend(thread) != KERN_SUCCESS) return 0;
    uintptr_t pc = 0, fp = 0, lr = 0;
    int n = 0;
    if (tt_thread_registers(thread, &pc, &fp, &lr) == 0) n = tt_walk_stack(pc, fp, lr, out, max);
    thread_resume(thread);
    return n;
}

int tt_image_count(void) { return __atomic_load_n(&g_image_count, __ATOMIC_ACQUIRE); }

int tt_image_info(int index, uint64_t *load, uint64_t *size, uint8_t uuid[16], const char **path) {
    if (index < 0 || index >= tt_image_count()) return -1;
    const tt_image_t *img = &g_images[index];
    if (load) *load = img->load;
    if (size) *size = img->size;
    if (uuid) memcpy(uuid, img->uuid, 16);
    if (path) *path = img->path ? img->path : "?";
    return 0;
}

void tt_crash_reset_for_test(void) {
    g_written = 0;
    tt_exc_present = 0;
    if (g_fd >= 0) ftruncate(g_fd, 0);
}

// ---------------------------------------------------------------------------------------------
// gzip

int tt_gzip(const uint8_t *input, size_t input_len, uint8_t **output, size_t *output_len) {
    if (!output || !output_len) return -1;
    z_stream zs;
    memset(&zs, 0, sizeof(zs));
    if (deflateInit2(&zs, Z_DEFAULT_COMPRESSION, Z_DEFLATED, 15 + 16, 8, Z_DEFAULT_STRATEGY) != Z_OK) return -1;
    uLong bound = deflateBound(&zs, (uLong)input_len) + 32;
    uint8_t *buf = malloc(bound);
    if (!buf) {
        deflateEnd(&zs);
        return -1;
    }
    zs.next_in = (Bytef *)input;
    zs.avail_in = (uInt)input_len;
    zs.next_out = buf;
    zs.avail_out = (uInt)bound;
    int rc = deflate(&zs, Z_FINISH);
    size_t produced = zs.total_out;
    deflateEnd(&zs);
    if (rc != Z_STREAM_END) {
        free(buf);
        return -1;
    }
    *output = buf;
    *output_len = produced;
    return 0;
}

int tt_gunzip(const uint8_t *input, size_t input_len, uint8_t **output, size_t *output_len) {
    if (!output || !output_len) return -1;
    z_stream zs;
    memset(&zs, 0, sizeof(zs));
    if (inflateInit2(&zs, 15 + 32) != Z_OK) return -1;
    size_t cap = input_len * 4 + 1024;
    uint8_t *buf = malloc(cap);
    if (!buf) {
        inflateEnd(&zs);
        return -1;
    }
    zs.next_in = (Bytef *)input;
    zs.avail_in = (uInt)input_len;
    int rc;
    do {
        if (zs.total_out == cap) {
            size_t ncap = cap * 2;
            uint8_t *nbuf = realloc(buf, ncap);
            if (!nbuf) {
                free(buf);
                inflateEnd(&zs);
                return -1;
            }
            buf = nbuf;
            cap = ncap;
        }
        zs.next_out = buf + zs.total_out;
        zs.avail_out = (uInt)(cap - zs.total_out);
        rc = inflate(&zs, Z_NO_FLUSH);
    } while (rc == Z_OK);
    size_t produced = zs.total_out;
    inflateEnd(&zs);
    if (rc != Z_STREAM_END) {
        free(buf);
        return -1;
    }
    *output = buf;
    *output_len = produced;
    return 0;
}

// ---------------------------------------------------------------------------------------------
// Process info

static int tt_kinfo(struct kinfo_proc *kp) {
    int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, getpid()};
    size_t len = sizeof(*kp);
    memset(kp, 0, sizeof(*kp));
    return sysctl(mib, 4, kp, &len, NULL, 0);
}

int64_t tt_process_start_time_ms(void) {
    struct kinfo_proc kp;
    if (tt_kinfo(&kp) != 0) return 0;
    struct timeval tv = kp.kp_proc.p_starttime;
    return (int64_t)tv.tv_sec * 1000 + tv.tv_usec / 1000;
}

int tt_debugger_attached(void) {
    struct kinfo_proc kp;
    if (tt_kinfo(&kp) != 0) return 0;
    return (kp.kp_proc.p_flag & P_TRACED) != 0;
}

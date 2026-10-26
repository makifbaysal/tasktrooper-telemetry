/*
 * Host test for trooper_crash.c: every case forks a child that installs the handler and crashes for
 * real; the parent checks how the child died (previous handler / default action chained) and the
 * report it left behind (signal, thread, context, frames starting at the faulting function, modules
 * with build ids). Exit code 0 = all passed.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#include "trooper_crash.h"

static int failures;
#define CHECK(cond, ...)                                    \
    do {                                                    \
        if (!(cond)) {                                      \
            failures++;                                     \
            fprintf(stderr, "FAIL %s:%d: ", __FILE__, __LINE__); \
            fprintf(stderr, __VA_ARGS__);                   \
            fprintf(stderr, "\n");                          \
        }                                                   \
    } while (0)

static volatile int *volatile g_null;

__attribute__((noinline)) void crash_segv(void) {
    *g_null = 42;
    __asm__ volatile("" ::: "memory");
}
__attribute__((noinline)) void level2(void) {
    crash_segv();
    __asm__ volatile("" ::: "memory");
}
__attribute__((noinline)) void level1(void) {
    level2();
    __asm__ volatile("" ::: "memory");
}

__attribute__((noinline)) void crash_abort(void) {
    abort();
}

__attribute__((noinline)) void crash_trap(void) {
    __builtin_trap();
}

static void *thread_main(void *arg) {
    (void)arg;
#if defined(__APPLE__)
    pthread_setname_np("worker-7");
#else
    pthread_setname_np(pthread_self(), "worker-7");
#endif
    level1();
    return NULL;
}

static void crash_on_thread(void) {
    pthread_t t;
    pthread_create(&t, NULL, thread_main, NULL);
    pthread_join(t, NULL);
}

static void previous_handler(int sig) {
    (void)sig;
    _exit(42);
}

typedef struct {
    const char *name;
    void (*crash)(void);
    int install_previous;
    int expect_signal;   /* how the child must die; 0 = any of expect_alt */
    int expect_alt;
    int expect_exit;     /* when previous handler installed */
    void *fault_fn;      /* first frame must be in this function (0 = skip) */
    void *caller_fn;     /* some later frame must be in this function */
    const char *thread;
} test_case;

static char *read_file(const char *path) {
    FILE *f = fopen(path, "r");
    if (!f) return NULL;
    static char buf[64 * 1024];
    size_t n = fread(buf, 1, sizeof buf - 1, f);
    buf[n] = 0;
    fclose(f);
    return buf;
}

static int in_fn(uintptr_t pc, void *fn) {
    uintptr_t f = (uintptr_t)fn;
    return pc >= f && pc < f + 160;
}

static void run_case(const char *dir, const test_case *tc) {
    char path[512];
    snprintf(path, sizeof path, "%s/%s.ndk", dir, tc->name);
    unlink(path);
    fflush(stdout);
    fflush(stderr);
    pid_t pid = fork();
    if (pid == 0) {
        if (tc->install_previous) signal(tc->expect_signal ? tc->expect_signal : SIGSEGV, previous_handler);
        if (tt_ndk_install(path) != 0) _exit(99);
        tt_ndk_set_context("session=s-1\tview=v-9\nCheckout");
        tc->crash();
        _exit(98); /* not reached */
    }
    int status = 0;
    waitpid(pid, &status, 0);
    if (tc->install_previous) {
        CHECK(WIFEXITED(status) && WEXITSTATUS(status) == tc->expect_exit, "%s: previous handler not chained (status %d)",
              tc->name, status);
    } else if (tc->expect_signal) {
        CHECK(WIFSIGNALED(status) && WTERMSIG(status) == tc->expect_signal, "%s: expected death by %d, status %d",
              tc->name, tc->expect_signal, status);
    } else {
        CHECK(WIFSIGNALED(status) && (WTERMSIG(status) == SIGTRAP || WTERMSIG(status) == SIGILL),
              "%s: expected SIGTRAP/SIGILL, status %d", tc->name, status);
    }
    char *r = read_file(path);
    CHECK(r != NULL, "%s: no report", tc->name);
    if (!r) return;
    CHECK(strncmp(r, "TTNDK 1\n", 8) == 0, "%s: bad header", tc->name);
    CHECK(strstr(r, "\nend\n") != NULL, "%s: report not terminated", tc->name);
    char want[64];
    if (tc->expect_signal) {
        snprintf(want, sizeof want, "\nsignal %d\n", tc->expect_signal);
        CHECK(strstr(r, want) != NULL, "%s: missing '%s'", tc->name, want);
    }
    snprintf(want, sizeof want, "\npid %d\n", (int)pid);
    CHECK(strstr(r, want) != NULL, "%s: pid", tc->name);
    CHECK(strstr(r, "\nctx session=s-1\tview=v-9 Checkout\n") != NULL, "%s: context (newline must be sanitized)", tc->name);
    if (tc->thread) {
        snprintf(want, sizeof want, "\nthread %s\n", tc->thread);
        CHECK(strstr(r, want) != NULL, "%s: thread name", tc->name);
    }
    /* Frames: parse "frame 0x.. 0x.. idx". */
    int nframes = 0, first_ok = 0, caller_ok = 0, with_module = 0;
    for (char *line = strstr(r, "\nframe "); line; line = strstr(line + 1, "\nframe ")) {
        unsigned long long pc = 0, rel = 0;
        int idx = -2;
        if (sscanf(line + 7, "0x%llx 0x%llx %d", &pc, &rel, &idx) != 3) continue;
        if (nframes == 0 && tc->fault_fn) first_ok = in_fn((uintptr_t)pc, tc->fault_fn);
        if (tc->caller_fn && in_fn((uintptr_t)pc, tc->caller_fn)) caller_ok = 1;
        if (idx >= 0) with_module++;
        nframes++;
    }
    CHECK(nframes >= 3, "%s: only %d frames\n%s", tc->name, nframes, r);
    if (tc->fault_fn) CHECK(first_ok, "%s: first frame is not the faulting function\n%s", tc->name, r);
    if (tc->caller_fn) CHECK(caller_ok, "%s: caller frame missing\n%s", tc->name, r);
    CHECK(with_module > 0, "%s: no frame mapped to a module", tc->name);
    CHECK(strstr(r, "\nmodule ") != NULL, "%s: no module lines", tc->name);
    /* The harness binary itself is linked with a build id (LC_UUID on macOS). */
    char *m = strstr(r, "crash_harness");
    CHECK(m != NULL, "%s: harness module not listed", tc->name);
    if (m) {
        char *ls = m;
        while (ls > r && ls[-1] != '\n') ls--;
        int idx;
        char id[80];
        CHECK(sscanf(ls, "module %d %79s", &idx, id) == 2 && strlen(id) >= 32, "%s: harness build id missing", tc->name);
    }
    printf("ok  %-14s frames=%d\n", tc->name, nframes);
}

int main(int argc, char **argv) {
    const char *dir = argc > 1 ? argv[1] : "reports";
    mkdir(dir, 0700);
    test_case cases[] = {
        {"segv", level1, 0, SIGSEGV, 0, 0, (void *)crash_segv, (void *)level2, NULL},
        {"abort", crash_abort, 0, SIGABRT, 0, 0, NULL, NULL, NULL},
        {"trap", crash_trap, 0, 0, 0, 0, (void *)crash_trap, NULL, NULL},
        {"thread", crash_on_thread, 0, SIGSEGV, 0, 0, (void *)crash_segv, (void *)level1, "worker-7"},
        {"chained", level1, 1, SIGSEGV, 0, 42, (void *)crash_segv, (void *)level2, NULL},
    };
    for (size_t i = 0; i < sizeof cases / sizeof cases[0]; i++) run_case(dir, &cases[i]);

    /* In-process: a test report (no signal) must not disturb the process and lists modules. */
    char path[512];
    snprintf(path, sizeof path, "%s/inproc.ndk", dir);
    CHECK(tt_ndk_install(path) == 0, "install");
    CHECK(tt_ndk_refresh_modules() > 1, "module snapshot");
    CHECK(tt_ndk_write_test_report(SIGSEGV) == 0, "test report");
    tt_ndk_uninstall();
    char *r = read_file(path);
    CHECK(r && strstr(r, "\nsignal 11\n") && strstr(r, "\nend\n"), "in-process report");

    if (failures) {
        fprintf(stderr, "%d failure(s)\n", failures);
        return 1;
    }
    printf("all native crash harness checks passed\n");
    return 0;
}

/*
 * TaskTrooper native crash handler (trooper-ndk).
 *
 * Portable core used on Android (bionic) and, for tests, on Linux/macOS hosts. Everything that runs
 * inside the signal handler is async-signal-safe: no malloc, no locks, no stdio; only syscalls on a
 * pre-opened file descriptor, static buffers and a pre-captured module table.
 */
#ifndef TROOPER_CRASH_H
#define TROOPER_CRASH_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define TT_NDK_REPORT_VERSION 1
#define TT_NDK_MAX_FRAMES 64
#define TT_NDK_MAX_MODULES 1024
#define TT_NDK_CONTEXT_MAX 1024

/*
 * Opens (creates/truncates) `report_path`, snapshots loaded modules (path, address range, load bias,
 * GNU build-id) and installs handlers for SIGSEGV, SIGABRT, SIGBUS, SIGILL, SIGFPE and SIGTRAP on an
 * alternate stack. Previous handlers are saved and chained. Idempotent; returns 0 on success.
 * The caller must collect reports of previous processes BEFORE reusing their file names.
 */
int tt_ndk_install(const char *report_path);

/* Restores the previous handlers and closes the report file (tests). */
void tt_ndk_uninstall(void);

/*
 * Context line copied into the report (session/view/user ids as produced by the Kotlin side).
 * Not async-signal-safe itself, but safe to call concurrently with a crash (double buffered).
 * Newlines are replaced by spaces.
 */
void tt_ndk_set_context(const char *context);

/* Re-snapshots loaded modules (call after libraries are loaded). Returns the module count or -1. */
int tt_ndk_refresh_modules(void);

/* Writes a report for `sig` as if it had been received here, without chaining (tests). */
int tt_ndk_write_test_report(int sig);

#ifdef __cplusplus
}
#endif

#endif /* TROOPER_CRASH_H */

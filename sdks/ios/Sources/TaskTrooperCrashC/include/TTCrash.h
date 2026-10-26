#ifndef TT_CRASH_H
#define TT_CRASH_H

#include <stdint.h>
#include <stddef.h>
#include <mach/mach_types.h>

#ifdef __OBJC__
@class NSException;
#endif

#ifdef __cplusplus
extern "C" {
#endif

/// Opens (and truncates) the crash report file, starts tracking loaded binary images and optionally
/// installs POSIX signal handlers (SIGABRT, SIGSEGV, SIGBUS, SIGILL, SIGTRAP, SIGFPE) and the
/// NSUncaughtExceptionHandler. Previous handlers are chained. Returns 0 on success.
/// Read any pending report from `report_path` BEFORE calling this.
int tt_crash_install(const char *report_path, int install_signal_handlers, int install_exception_handler);

/// Restores previous signal / exception handlers and closes the report file.
void tt_crash_uninstall(void);

/// Context written into a crash report. Strings are copied into fixed-size static buffers.
void tt_crash_set_context(const char *session_id, const char *view_id, const char *view_name, const char *user_id);

/// 1 if a report has been written in this process.
int tt_crash_report_written(void);

/// Testing: write a report as if `sig` was received at the caller, without terminating.
int tt_crash_write_test_report(int sig, uintptr_t fault_address);

/// Testing: reset the "already written" flag and truncate the report file.
void tt_crash_reset_for_test(void);

#ifdef __OBJC__
/// Testing: write a report for `exception` exactly like the uncaught exception handler (no chaining).
int tt_crash_write_exception_report(NSException *exception);
#endif

/// Starts tracking loaded binary images (idempotent; also done by tt_crash_install).
void tt_images_register(void);
/// Number of tracked binary images.
int tt_image_count(void);
/// Image `index`: load address, __TEXT size, LC_UUID and path. Returns 0 on success.
int tt_image_info(int index, uint64_t *load, uint64_t *size, uint8_t uuid[16], const char **path);

/// Mach port of the main thread (valid for the process lifetime; do not deallocate).
mach_port_t tt_main_thread_port(void);
/// Suspends `thread`, reads its registers, walks its frame-pointer chain into `out` and resumes it.
/// Returns the number of frames (0 on failure or when `thread` is the caller).
int tt_capture_thread_stack(mach_port_t thread, uintptr_t *out, int max);

/// Sets the message of this library's own `__DATA,__crash_info` annotation (read into crash reports
/// like libswiftCore's fatalError message). NULL/empty clears it. Not async-signal-safe.
void tt_crash_set_annotation(const char *message);

/// gzip-compress (RFC 1952). On success returns 0 and a malloc'd buffer the caller must free().
int tt_gzip(const uint8_t *input, size_t input_len, uint8_t **output, size_t *output_len);
/// Decompress gzip or zlib data. On success returns 0 and a malloc'd buffer the caller must free().
int tt_gunzip(const uint8_t *input, size_t input_len, uint8_t **output, size_t *output_len);

/// Process start time (unix epoch ms) from sysctl kinfo_proc, 0 on failure.
int64_t tt_process_start_time_ms(void);
/// 1 if a debugger is attached (P_TRACED).
int tt_debugger_attached(void);

#ifdef __cplusplus
}
#endif

#endif /* TT_CRASH_H */

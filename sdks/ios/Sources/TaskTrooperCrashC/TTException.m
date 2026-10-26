#import <Foundation/Foundation.h>
#include <string.h>
#include "TTCrash.h"

extern char tt_exc_name[256];
extern char tt_exc_reason[2048];
extern uintptr_t tt_exc_frames[128];
extern int tt_exc_frame_count;
extern int tt_exc_present;
void tt_crash_write_exception_frames(void);

static NSUncaughtExceptionHandler *g_previous_handler = NULL;
static int g_exception_installed = 0;

static void tt_copy_escaped(char *dst, size_t cap, NSString *src) {
    const char *s = src.UTF8String ?: "";
    size_t j = 0;
    for (size_t i = 0; s[i] && j + 2 < cap; i++) {
        if (s[i] == '\n') { dst[j++] = '\\'; dst[j++] = 'n'; }
        else if (s[i] == '\r') { continue; }
        else dst[j++] = s[i];
    }
    dst[j] = 0;
}

static void tt_capture_exception(NSException *exception) {
    @try {
        tt_copy_escaped(tt_exc_name, sizeof(tt_exc_name), exception.name ?: @"NSException");
        tt_copy_escaped(tt_exc_reason, sizeof(tt_exc_reason), exception.reason ?: @"");
        NSArray<NSNumber *> *addresses = exception.callStackReturnAddresses;
        int n = 0;
        for (NSNumber *a in addresses) {
            if (n >= 128) break;
            tt_exc_frames[n++] = (uintptr_t)a.unsignedLongLongValue;
        }
        tt_exc_frame_count = n;
        tt_exc_present = 1;
    } @catch (__unused id e) {
    }
}

static void tt_uncaught_exception_handler(NSException *exception) {
    tt_capture_exception(exception);
    tt_crash_write_exception_frames();
    if (g_previous_handler) g_previous_handler(exception);
}

void tt_install_exception_handler(void) {
    if (g_exception_installed) return;
    g_previous_handler = NSGetUncaughtExceptionHandler();
    NSSetUncaughtExceptionHandler(&tt_uncaught_exception_handler);
    g_exception_installed = 1;
}

void tt_uninstall_exception_handler(void) {
    if (!g_exception_installed) return;
    if (NSGetUncaughtExceptionHandler() == &tt_uncaught_exception_handler) NSSetUncaughtExceptionHandler(g_previous_handler);
    g_exception_installed = 0;
}

int tt_crash_write_exception_report(NSException *exception) {
    tt_capture_exception(exception);
    tt_crash_write_exception_frames();
    return tt_crash_report_written() ? 0 : -1;
}

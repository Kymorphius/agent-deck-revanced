//go:build darwin && cgo

// matrix/hardened: scope native input-source changes to our terminal's GUI app.
#import <AppKit/AppKit.h>
#import <Carbon/Carbon.h>
#include <libproc.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/sysctl.h>
#include "native_darwin.h"

// matrix/hardened: proc_pidinfo(PROC_PIDTBSDINFO) denies access to a root-owned
// login process in an ordinary terminal's ancestry. sysctl exposes the parent
// identity without requesting its protected BSD process details.
int deck_parent_pid(int pid) {
    if (pid <= 0) return 0;
    int mib[] = { CTL_KERN, KERN_PROC, KERN_PROC_PID, pid };
    struct kinfo_proc info = {0};
    size_t size = sizeof(info);
    if (sysctl(mib, 4, &info, &size, NULL, 0) != 0 || size != sizeof(info)) {
        return 0;
    }
    return info.kp_eproc.e_ppid;
}

int deck_terminal_owner(void) {
    pid_t pid = getppid();
    for (int depth = 0; depth < 64 && pid > 1; depth++) {
        char path[PROC_PIDPATHINFO_MAXSIZE] = {0};
        if (proc_pidpath(pid, path, sizeof(path)) > 0 &&
            strstr(path, ".app/Contents/MacOS/")) {
            return pid;
        }
        pid_t parent = deck_parent_pid(pid);
        if (parent <= 0 || parent == pid) {
            break;
        }
        pid = parent;
    }
    return 0;
}

int deck_is_foreground(int owner) {
    @autoreleasepool {
        return owner > 0 && [NSWorkspace sharedWorkspace].frontmostApplication.processIdentifier == owner;
    }
}

char *deck_source_id(int ascii) {
    TISInputSourceRef source = ascii ? TISCopyCurrentASCIICapableKeyboardInputSource()
                                     : TISCopyCurrentKeyboardInputSource();
    if (!source) return NULL;
    CFStringRef identifier = TISGetInputSourceProperty(source, kTISPropertyInputSourceID);
    char *result = NULL;
    if (identifier) {
        CFIndex capacity = CFStringGetMaximumSizeForEncoding(CFStringGetLength(identifier),
                                                            kCFStringEncodingUTF8) + 1;
        result = malloc(capacity);
        if (result && !CFStringGetCString(identifier, result, capacity, kCFStringEncodingUTF8)) {
            free(result);
            result = NULL;
        }
    }
    CFRelease(source);
    return result;
}

int deck_select_source(const char *identifier, int owner) {
    if (!identifier || !deck_is_foreground(owner)) return 0;
    CFStringRef key = CFStringCreateWithCString(NULL, identifier, kCFStringEncodingUTF8);
    if (!key) return 0;
    const void *keys[] = { kTISPropertyInputSourceID, kTISPropertyInputSourceIsEnabled };
    const void *values[] = { key, kCFBooleanTrue };
    CFDictionaryRef filter = CFDictionaryCreate(NULL, keys, values, 2,
                                               &kCFTypeDictionaryKeyCallBacks,
                                               &kCFTypeDictionaryValueCallBacks);
    CFArrayRef sources = TISCreateInputSourceList(filter, false);
    int success = 0;
    if (sources && CFArrayGetCount(sources) > 0 && deck_is_foreground(owner)) {
        success = TISSelectInputSource((TISInputSourceRef)CFArrayGetValueAtIndex(sources, 0)) == noErr;
    }
    if (sources) CFRelease(sources);
    CFRelease(filter);
    CFRelease(key);
    return success;
}

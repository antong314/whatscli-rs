//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>

static id whatscliActivityToken = nil;

// Take a process-wide activity assertion so macOS App Nap never throttles
// us. Without it the OS eventually stalls this (windowless, "idle") process's
// timers — whatsmeow's keepalive and auto-reconnect stop firing, the
// WhatsApp socket dies server-side, and the backend becomes a zombie that
// still accepts gRPC but never receives another message.
static void whatscliDisableAppNap(void) {
    @autoreleasepool {
        whatscliActivityToken = [[[NSProcessInfo processInfo]
            beginActivityWithOptions:(NSActivityUserInitiated | NSActivityLatencyCritical)
                              reason:@"maintaining live WhatsApp connection"] retain];
    }
}
*/
import "C"

func disableAppNap() {
	C.whatscliDisableAppNap()
}

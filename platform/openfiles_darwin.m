#import <Cocoa/Cocoa.h>
#include "_cgo_export.h"
#include "openfiles_darwin.h"

@interface HCOpenFileHandler : NSObject
- (void)handleOpenDocuments:(NSAppleEventDescriptor *)event withReplyEvent:(NSAppleEventDescriptor *)reply;
@end

@implementation HCOpenFileHandler
- (void)handleOpenDocuments:(NSAppleEventDescriptor *)event withReplyEvent:(NSAppleEventDescriptor *)reply {
    NSAppleEventDescriptor *list = [event paramDescriptorForKeyword:keyDirectObject];
    NSInteger count = [list numberOfItems];
    for (NSInteger i = 1; i <= count; i++) {
        NSAppleEventDescriptor *item = [list descriptorAtIndex:i];
        NSString *value = [item stringValue];
        if (value == nil) {
            continue;
        }
        NSString *path = value;
        if ([value hasPrefix:@"file://"]) {
            NSURL *url = [NSURL URLWithString:value];
            if (url != nil && [url path] != nil) {
                path = [url path];
            }
        }
        const char *cpath = [path UTF8String];
        if (cpath != NULL) {
            goOpenFile((char *)cpath);
        }
    }
}

- (void)registerAppleEventHandler {
    [[NSAppleEventManager sharedAppleEventManager]
        setEventHandler:self
            andSelector:@selector(handleOpenDocuments:withReplyEvent:)
          forEventClass:kCoreEventClass
             andEventID:kAEOpenDocuments];
}

- (void)appWillFinishLaunching:(NSNotification *)note {
    [self registerAppleEventHandler];
}
@end

static HCOpenFileHandler *hcHandler = nil;

void installOpenFileHandler(void) {
    if (hcHandler != nil) {
        return;
    }
    hcHandler = [[HCOpenFileHandler alloc] init];

    // AppKit installs its own default kAEOpenDocuments handler while the app
    // finishes launching; one set now (before [NSApp run]) would be overwritten,
    // so the cold-launch event would hit AppKit's default and fail with "cannot
    // open files in the ... format". Installing from applicationWillFinishLaunching:
    // is the documented time that replaces that default, so both the queued
    // cold-launch event and later warm events reach our handler.
    if (NSApp != nil && [NSApp isRunning]) {
        [hcHandler registerAppleEventHandler];
    } else {
        [[NSNotificationCenter defaultCenter]
            addObserver:hcHandler
               selector:@selector(appWillFinishLaunching:)
                   name:NSApplicationWillFinishLaunchingNotification
                 object:nil];
    }
}

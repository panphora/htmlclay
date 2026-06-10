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
@end

static HCOpenFileHandler *hcHandler = nil;

void installOpenFileHandler(void) {
    if (hcHandler != nil) {
        return;
    }
    hcHandler = [[HCOpenFileHandler alloc] init];
    [[NSAppleEventManager sharedAppleEventManager]
        setEventHandler:hcHandler
            andSelector:@selector(handleOpenDocuments:withReplyEvent:)
          forEventClass:kCoreEventClass
             andEventID:kAEOpenDocuments];
}

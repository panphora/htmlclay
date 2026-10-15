#import <Cocoa/Cocoa.h>
#include <stdlib.h>
#include "_cgo_export.h"
#include "sparkle_darwin.h"

// Event kinds, matching sparkle.go.
enum {
    hcEventStarted = 1,
    hcEventStartFailed = 2,
    hcEventReady = 3,
    hcEventAborted = 4,
    hcEventInstallFailed = 5,
    hcEventCheckSkipped = 6,
};

// Sparkle's SUNoUpdateError. A check that finds nothing is not worth a log line.
static const NSInteger hcSUNoUpdateError = 1001;

// Sparkle is loaded at run time, not linked, so its headers are not used. These
// protocols declare only the methods called below, with Sparkle 2's signatures.
@protocol HCSPUUpdater <NSObject>
- (instancetype)initWithHostBundle:(NSBundle *)hostBundle applicationBundle:(NSBundle *)applicationBundle userDriver:(id)userDriver delegate:(id)delegate;
- (BOOL)startUpdater:(NSError **)error;
- (void)checkForUpdatesInBackground;
@property (nonatomic, readonly) BOOL canCheckForUpdates;
@property (nonatomic, readonly) BOOL sessionInProgress;
@end

@protocol HCSPUStandardUserDriver <NSObject>
- (instancetype)initWithHostBundle:(NSBundle *)hostBundle delegate:(id)delegate;
@end

@protocol HCSUAppcastItem <NSObject>
@property (nonatomic, copy, readonly) NSString *versionString;
@end

static void hcSend(int kind, NSString *text) {
    goSparkleEvent(kind, (char *)[(text ?: @"") UTF8String]);
}

// Every method runs on the main thread: Sparkle calls its delegate there, and
// hcSparkleStart and hcSparkleInstallNow dispatch to it.
@interface HCSparkleDelegate : NSObject
@end

@implementation HCSparkleDelegate {
@public
    id updater;
    id userDriver;
    void (^installNow)(void);
}

// Sparkle calls this once it has downloaded, verified and prepared an update
// that it would otherwise install when the app quits. Returning YES keeps the
// block for the tray's Restart to Update row and stops further update cycles.
// Sparkle still installs on any quit, whatever this returns.
- (BOOL)updater:(id)u willInstallUpdateOnQuit:(id)item immediateInstallationBlock:(void (^)(void))block {
    installNow = [block copy];
    hcSend(hcEventReady, [(id<HCSUAppcastItem>)item versionString]);
    return YES;
}

// Sparkle asks before it downloads an update. A version that failed to start
// after an earlier install is refused, so Sparkle never prepares it again and
// a plain quit cannot install it. A newer version is offered as usual.
- (BOOL)updater:(id)u shouldProceedWithUpdate:(id)item updateCheck:(NSInteger)check error:(NSError **)error {
    NSString *v = [(id<HCSUAppcastItem>)item versionString];
    char *reason = goSparkleSkipReason((char *)[(v ?: @"") UTF8String]);
    if (reason == NULL) {
        return YES;
    }
    NSString *text = [NSString stringWithUTF8String:reason];
    free(reason);
    if (error != NULL) {
        *error = [NSError errorWithDomain:@"com.htmlclay.update" code:1 userInfo:@{NSLocalizedDescriptionKey: text ?: @"skipped"}];
    }
    return NO;
}

- (void)updater:(id)u didAbortWithError:(NSError *)error {
    if ([error.domain isEqualToString:@"SUSparkleErrorDomain"] && error.code == hcSUNoUpdateError) {
        return;
    }
    hcSend(hcEventAborted, [NSString stringWithFormat:@"%@ (%@ %ld)", error.localizedDescription, error.domain, (long)error.code]);
}
@end

static HCSparkleDelegate *hcDelegate = nil;

char *hcSparkleLoad(const char *frameworkPath) {
    @autoreleasepool {
        NSString *path = [NSString stringWithUTF8String:frameworkPath];
        NSBundle *framework = [NSBundle bundleWithPath:path];
        if (framework == nil) {
            return strdup([[NSString stringWithFormat:@"no framework at %@", path] UTF8String]);
        }
        NSError *error = nil;
        if (![framework loadAndReturnError:&error]) {
            return strdup([[NSString stringWithFormat:@"cannot load %@: %@", path, error.localizedDescription] UTF8String]);
        }
        if (NSClassFromString(@"SPUUpdater") == nil || NSClassFromString(@"SPUStandardUserDriver") == nil) {
            return strdup([[NSString stringWithFormat:@"%@ has no SPUUpdater", path] UTF8String]);
        }
        return NULL;
    }
}

void hcSparkleStart(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (hcDelegate != nil) {
            return;
        }
        NSBundle *host = [NSBundle mainBundle];
        HCSparkleDelegate *d = [[HCSparkleDelegate alloc] init];
        // The standard driver is only ever seen when Sparkle needs a person:
        // an update marked critical or informational, or one needing an
        // administrator's password. The release pipeline publishes neither, and
        // the app never starts Sparkle on a bundle it cannot write.
        d->userDriver = [(id<HCSPUStandardUserDriver>)[NSClassFromString(@"SPUStandardUserDriver") alloc] initWithHostBundle:host delegate:nil];
        d->updater = [(id<HCSPUUpdater>)[NSClassFromString(@"SPUUpdater") alloc] initWithHostBundle:host applicationBundle:host userDriver:d->userDriver delegate:d];
        hcDelegate = d;
        NSError *error = nil;
        if (![d->updater startUpdater:&error]) {
            hcSend(hcEventStartFailed, error.localizedDescription ?: @"startUpdater failed");
            return;
        }
        hcSend(hcEventStarted, @"");
    });
}

void hcSparkleInstallNow(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (hcDelegate == nil || hcDelegate->installNow == nil) {
            hcSend(hcEventInstallFailed, @"Sparkle has no prepared update");
            return;
        }
        hcDelegate->installNow();
    });
}

void hcSparkleCheckNow(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (hcDelegate == nil) {
            hcSend(hcEventCheckSkipped, @"Sparkle is not started");
            return;
        }
        if (hcDelegate->installNow != nil) {
            hcSend(hcEventCheckSkipped, @"an update is already prepared");
            return;
        }
        id<HCSPUUpdater> u = hcDelegate->updater;
        if ([u sessionInProgress]) {
            hcSend(hcEventCheckSkipped, @"a check is already running");
            return;
        }
        if (![u canCheckForUpdates]) {
            hcSend(hcEventCheckSkipped, @"Sparkle cannot check right now");
            return;
        }
        [u checkForUpdatesInBackground];
    });
}

char *hcSparkleInfoString(const char *key) {
    @autoreleasepool {
        id value = [[NSBundle mainBundle] objectForInfoDictionaryKey:[NSString stringWithUTF8String:key]];
        if (![value isKindOfClass:[NSString class]]) {
            return NULL;
        }
        return strdup([(NSString *)value UTF8String]);
    }
}

//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#import <ApplicationServices/ApplicationServices.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>

static NSDictionary *failure(NSString *message) { return @{ @"error": message ?: @"Computer driver failed" }; }

static NSDictionary *displayInfo(void) {
    CGDirectDisplayID display = CGMainDisplayID();
    CGRect r = CGDisplayBounds(display);
    return @{ @"id": @(display), @"x": @(r.origin.x), @"y": @(r.origin.y), @"width": @(r.size.width), @"height": @(r.size.height) };
}

static NSDictionary *status(BOOL prompt) {
    BOOL supported = NO;
    if (@available(macOS 14.0, *)) { supported = YES; }
    BOOL screen = CGPreflightScreenCaptureAccess();
    if (prompt && supported && !screen) { screen = CGRequestScreenCaptureAccess(); }
    BOOL accessibility = AXIsProcessTrustedWithOptions((__bridge CFDictionaryRef)@{ (__bridge NSString *)kAXTrustedCheckOptionPrompt: @(prompt) });
    return @{ @"status": @{ @"supported": @(supported), @"screen_recording": @(screen), @"accessibility": @(accessibility), @"display": displayInfo() } };
}

static NSDictionary *screenshot(void) {
    if (!CGPreflightScreenCaptureAccess()) { return failure(@"Screen Recording permission was revoked"); }
    if (@available(macOS 14.0, *)) {
        NSDictionary *bounds = displayInfo();
        double width = [bounds[@"width"] doubleValue], height = [bounds[@"height"] doubleValue];
        if (width <= 0 || height <= 0) { return failure(@"No active main display"); }
        double scale = fmin(1.0, 1280.0 / fmax(width, height));
        NSInteger pixelWidth = MAX(1, (NSInteger)lround(width * scale));
        NSInteger pixelHeight = MAX(1, (NSInteger)lround(height * scale));
        dispatch_semaphore_t done = dispatch_semaphore_create(0);
        __block NSDictionary *result;
        [SCShareableContent getShareableContentExcludingDesktopWindows:YES onScreenWindowsOnly:YES completionHandler:^(SCShareableContent *content, NSError *error) {
            if (error) { result = failure(error.localizedDescription); dispatch_semaphore_signal(done); return; }
            SCDisplay *target = nil;
            for (SCDisplay *display in content.displays) {
                if (display.displayID == [bounds[@"id"] unsignedIntValue]) { target = display; break; }
            }
            if (!target) { result = failure(@"Main display disappeared"); dispatch_semaphore_signal(done); return; }
            SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:target excludingWindows:@[]];
            SCStreamConfiguration *config = [[SCStreamConfiguration alloc] init];
            config.width = pixelWidth;
            config.height = pixelHeight;
            config.showsCursor = YES;
            [SCScreenshotManager captureImageWithFilter:filter configuration:config completionHandler:^(CGImageRef image, NSError *captureError) {
                @autoreleasepool {
                    if (!image || captureError) { result = failure(captureError.localizedDescription ?: @"Empty screenshot"); }
                    else if (![bounds isEqualToDictionary:displayInfo()]) { result = failure(@"Display changed while capturing; take a fresh screenshot"); }
                    else {
                        NSBitmapImageRep *bitmap = [[NSBitmapImageRep alloc] initWithCGImage:image];
                        NSData *jpeg = [bitmap representationUsingType:NSBitmapImageFileTypeJPEG properties:@{ NSImageCompressionFactor: @0.75 }];
                        if (!jpeg || jpeg.length > (2 << 20)) { result = failure(@"Screenshot exceeded image budget"); }
                        else { result = @{ @"frame": @{ @"width": @(CGImageGetWidth(image)), @"height": @(CGImageGetHeight(image)), @"display": bounds }, @"image": [jpeg base64EncodedStringWithOptions:0] }; }
                    }
                    dispatch_semaphore_signal(done);
                }
            }];
        }];
        if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 15 * NSEC_PER_SEC)) != 0) { return failure(@"Screenshot timed out"); }
        return result ?: failure(@"Screenshot returned no result");
    }
    return failure(@"Computer requires macOS 14 or later");
}

static CGPoint point(NSArray *p) { return CGPointMake([p[0] doubleValue], [p[1] doubleValue]); }

static BOOL postMouse(CGEventType type, CGPoint p, CGMouseButton button, int clickCount) {
    CGEventRef e = CGEventCreateMouseEvent(NULL, type, p, button);
    if (!e) { return NO; }
    CGEventSetFlags(e, 0);
    CGEventSetIntegerValueField(e, kCGMouseEventClickState, clickCount);
    CGEventPost(kCGHIDEventTap, e);
    CFRelease(e);
    return YES;
}

static BOOL postKey(CGKeyCode code, CGEventFlags flags, const UniChar *text, UniCharCount count) {
    CGEventRef down = CGEventCreateKeyboardEvent(NULL, code, true);
    CGEventRef up = CGEventCreateKeyboardEvent(NULL, code, false);
    if (!down || !up) { if (down) CFRelease(down); if (up) CFRelease(up); return NO; }
    CGEventSetFlags(down, flags);
    CGEventSetFlags(up, flags);
    if (text) { CGEventKeyboardSetUnicodeString(down, count, text); CGEventKeyboardSetUnicodeString(up, count, text); }
    CGEventPost(kCGHIDEventTap, down);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
    return YES;
}

static NSDictionary *action(NSDictionary *request) {
    if (!AXIsProcessTrusted() || !CGPreflightScreenCaptureAccess()) { return failure(@"Desktop permissions were revoked; no input was sent"); }
    NSDictionary *a = request[@"action"];
    NSString *type = a[@"type"];
    BOOL ok = YES;
    if ([type isEqualToString:@"click"] || [type isEqualToString:@"drag"]) {
        BOOL right = [a[@"button"] isEqualToString:@"right"];
        CGMouseButton button = right ? kCGMouseButtonRight : kCGMouseButtonLeft;
        CGEventType downType = right ? kCGEventRightMouseDown : kCGEventLeftMouseDown;
        CGEventType upType = right ? kCGEventRightMouseUp : kCGEventLeftMouseUp;
        CGPoint start = point(a[@"position"]), end = start;
        BOOL drag = [type isEqualToString:@"drag"];
        int clicks = drag ? 1 : MAX(1, [a[@"clicks"] intValue]);
        // Allocate the release before posting any button-down. Cancellation
        // is handled between primitives, so the release always runs.
        CGEventRef release = CGEventCreateMouseEvent(NULL, upType, start, button);
        if (!release) { return failure(@"Could not allocate mouse event"); }
        for (int i = 1; i <= clicks; i++) {
            ok = postMouse(downType, start, button, i);
            if (drag && ok) {
                end = point(a[@"to"]);
                for (int step = 1; step <= 12; step++) {
                    CGPoint p = CGPointMake(start.x + (end.x-start.x)*step/12.0, start.y + (end.y-start.y)*step/12.0);
                    ok = postMouse(kCGEventLeftMouseDragged, p, button, 1) && ok;
                    usleep(15000);
                }
            }
            CGEventSetLocation(release, end);
            CGEventSetFlags(release, 0);
            CGEventSetIntegerValueField(release, kCGMouseEventClickState, i);
            CGEventPost(kCGHIDEventTap, release);
            if (!ok) { break; }
            if (i < clicks) { usleep(60000); }
        }
        CFRelease(release);
    } else if ([type isEqualToString:@"move"]) {
        ok = postMouse(kCGEventMouseMoved, point(a[@"position"]), kCGMouseButtonLeft, 0);
    } else if ([type isEqualToString:@"scroll"]) {
        // Positive deltas mean scrolling the viewport right/down.
        CGEventRef e = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitPixel, 2, -[a[@"delta_y"] intValue], -[a[@"delta_x"] intValue]);
        if (!e) { ok = NO; } else { CGEventSetFlags(e, 0); CGEventPost(kCGHIDEventTap, e); CFRelease(e); }
    } else if ([type isEqualToString:@"key"]) {
        ok = postKey([request[@"key_code"] unsignedShortValue], [request[@"key_flags"] unsignedLongLongValue], NULL, 0);
    } else if ([type isEqualToString:@"type"]) {
        NSString *text = a[@"text"];
        for (NSUInteger offset = 0; offset < text.length && ok;) {
            NSUInteger n = MIN((NSUInteger)20, text.length-offset);
            if (offset+n < text.length && CFStringIsSurrogateHighCharacter([text characterAtIndex:offset+n-1])) { n--; }
            UniChar chars[20];
            [text getCharacters:chars range:NSMakeRange(offset, n)];
            ok = postKey(0, 0, chars, n);
            offset += n;
            usleep(1000);
        }
    } else { return failure(@"Unsupported native action"); }
    return ok ? @{} : failure(@"Input delivery failed; inspect a fresh screenshot before continuing; do not replay actions");
}

char *ccdp_computer(const char *input) {
    @autoreleasepool {
        @try {
            NSData *data = [[NSString stringWithUTF8String:input] dataUsingEncoding:NSUTF8StringEncoding];
            NSDictionary *request = [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL];
            NSString *op = request[@"operation"];
            NSDictionary *result;
            if ([op isEqualToString:@"status"] || [op isEqualToString:@"authorize"]) { result = status([op isEqualToString:@"authorize"]); }
            else if ([op isEqualToString:@"screenshot"]) { result = screenshot(); }
            else if ([op isEqualToString:@"action"]) { result = action(request); }
            else { result = failure(@"Unsupported native operation"); }
            NSData *out = [NSJSONSerialization dataWithJSONObject:result options:0 error:NULL];
            return strdup([[NSString alloc] initWithData:out encoding:NSUTF8StringEncoding].UTF8String);
        } @catch (NSException *e) {
            NSData *out = [NSJSONSerialization dataWithJSONObject:failure(e.reason) options:0 error:NULL];
            return strdup([[NSString alloc] initWithData:out encoding:NSUTF8StringEncoding].UTF8String);
        }
    }
}

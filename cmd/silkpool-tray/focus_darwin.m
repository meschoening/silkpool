#import <Cocoa/Cocoa.h>

// The direct AppKit call that gives the panel the keyboard
// (focus_darwin.go).

// findWebView is the web view in v, or nil.
static NSView *findWebView(NSView *v, Class web) {
	if (v == nil || web == nil) return nil;
	if ([v isKindOfClass:web]) return v;
	for (NSView *s in [v subviews]) {
		NSView *w = findWebView(s, web);
		if (w) return w;
	}
	return nil;
}

// silkpoolFocusPanel activates the app, makes window the key window, and
// makes its web view the first responder. It runs on the main thread.
void silkpoolFocusPanel(void *window) {
	NSWindow *w = (NSWindow *)window;
	if (@available(macOS 14.0, *)) {
		[NSApp activate];
	} else {
		[NSApp activateIgnoringOtherApps:YES];
	}
	[w makeKeyAndOrderFront:nil];
	NSView *web = findWebView([w contentView], NSClassFromString(@"WKWebView"));
	if (web) [w makeFirstResponder:web];
}

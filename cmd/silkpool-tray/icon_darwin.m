#import <Cocoa/Cocoa.h>

// The direct AppKit call that sets the menu-bar icon (icon_darwin.go).

static NSStatusBarButton *findButton(NSView *v) {
	if (v == nil) return nil;
	if ([v isKindOfClass:[NSStatusBarButton class]]) return (NSStatusBarButton *)v;
	for (NSView *s in [v subviews]) {
		NSStatusBarButton *b = findButton(s);
		if (b) return b;
	}
	return nil;
}

// statusButton is the status item's button: the given NSStatusItem's,
// or, when item is NULL, the one among the app's windows, where the
// app's one status item keeps it.
static NSStatusBarButton *statusButton(void *item) {
	if (item) return ((NSStatusItem *)item).button;
	for (NSWindow *w in [NSApp windows]) {
		NSStatusBarButton *b = findButton([w contentView]);
		if (b) return b;
	}
	return nil;
}

// silkpoolSetStatusImage sets the status item's image to the PNG in
// bytes, sized to the menu bar, and its template flag. It runs on the
// main thread, and returns 1 if it found the button, 0 if not, and -1
// if the PNG doesn't decode.
int silkpoolSetStatusImage(void *item, const void *bytes, int len, int isTemplate) {
	NSImage *img = [[[NSImage alloc] initWithData:[NSData dataWithBytes:bytes length:len]] autorelease];
	if (!img) return -1;
	CGFloat t = [[NSStatusBar systemStatusBar] thickness];
	[img setSize:NSMakeSize(t, t)];
	[img setTemplate:isTemplate ? YES : NO];
	NSStatusBarButton *b = statusButton(item);
	if (!b) return 0;
	b.image = img;
	return 1;
}

package main

// The panel takes the keyboard when it opens on macOS (#286). The menu
// bar's status items are drawn by macOS, out of the app's process, so a
// click on the icon doesn't make the app active, and Wails' activation
// of a menu-bar app is refused under macOS's cooperative activation: the
// panel showed without being the key window, and the editor's time
// field took no keys. So the app asks to be activated, makes the panel
// the key window, and gives its web view the keyboard.

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
void silkpoolFocusPanel(void *window);
*/
import "C"

import "github.com/wailsapp/wails/v3/pkg/application"

// focusPanel makes w, the panel's window, the key window of the active
// app, with its web view taking the keyboard.
func focusPanel(w *application.WebviewWindow) {
	application.InvokeSync(func() {
		if nw := w.NativeWindow(); nw != nil {
			C.silkpoolFocusPanel(nw)
		}
	})
}

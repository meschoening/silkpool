package main

// The menu-bar icon on macOS. Wails v3.0.0 never clears an icon's
// template flag once it has set one, so the amber Degraded icon would
// draw as a white disc. So the icon is set by a direct AppKit call
// instead, which sets the status item's image and its template flag
// both, as ADR 0009 records. The app makes the icon once it has started,
// so its status item is there to set.

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
int silkpoolSetStatusImage(void *item, const void *bytes, int len, int isTemplate);
*/
import "C"

import (
	"reflect"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/meschoening/silkpool/cmd/silkpool-tray/icon"
)

// setIcon sets ti's image to i's menu-bar icon, by the direct call.
func setIcon(ti *application.SystemTray, i icon.Icon) {
	template := C.int(0)
	if i.Template {
		template = 1
	}
	application.InvokeSync(func() {
		// The status item's button is found among the app's windows,
		// through AppKit's public classes, or else through Wails' own
		// status item.
		if C.silkpoolSetStatusImage(nil, unsafe.Pointer(&i.Mac[0]), C.int(len(i.Mac)), template) != 0 {
			return
		}
		if item := statusItem(ti); item != nil {
			C.silkpoolSetStatusImage(item, unsafe.Pointer(&i.Mac[0]), C.int(len(i.Mac)), template)
		}
	})
}

// statusItem is the NSStatusItem Wails keeps unexported in ti, or nil if
// Wails no longer keeps it where v3.0.0 does.
func statusItem(ti *application.SystemTray) unsafe.Pointer {
	v := reflect.ValueOf(ti).Elem().FieldByName("impl")
	if !v.IsValid() {
		return nil
	}
	v = reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	if v.IsNil() || v.Elem().Kind() != reflect.Pointer {
		return nil
	}
	f := v.Elem().Elem().FieldByName("nsStatusItem")
	if !f.IsValid() || f.Kind() != reflect.UnsafePointer {
		return nil
	}
	return *(*unsafe.Pointer)(unsafe.Pointer(f.UnsafeAddr()))
}

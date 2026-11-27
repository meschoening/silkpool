package main

import (
	"reflect"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// followTheme makes the tray app, not Wails, change the icon's image
// when the taskbar turns light or dark, so its tooltip follows the image
// as setIcon sends them (#364). Wails v3.0.0 changes the image itself on
// a goroutine of its own, with a call that hides the tooltip, and
// nothing orders that call before the tray app's, so the tray app stops
// Wails' listener, as v3.0.0 keeps it in ti.
func (t *tray) followTheme(ti *application.SystemTray) {
	if cancel := themeListener(ti); cancel != nil {
		cancel()
	} else {
		t.log.Warn("can't stop Wails changing the tray icon on a theme change, so its tooltip may go")
	}
	t.app.Event.OnApplicationEvent(events.Windows.SystemThemeChanged, func(*application.ApplicationEvent) {
		t.setIcon(true)
	})
}

// themeListener is the func that stops the theme listener Wails keeps
// unexported in ti, or nil if Wails no longer keeps it where v3.0.0
// does.
func themeListener(ti *application.SystemTray) func() {
	v := reflect.ValueOf(ti).Elem().FieldByName("impl")
	if !v.IsValid() || v.Kind() != reflect.Interface {
		return nil
	}
	v = reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	if v.IsNil() || v.Elem().Kind() != reflect.Pointer {
		return nil
	}
	f := v.Elem().Elem().FieldByName("cancelTheme")
	if !f.IsValid() || f.Kind() != reflect.Func {
		return nil
	}
	cancel, _ := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(func())
	return cancel
}

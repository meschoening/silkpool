//go:build !darwin

package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/meschoening/silkpool/cmd/silkpool-tray/icon"
)

// setIcon sets ti's image to i's notification-area icons: the dark glyph
// for a light taskbar, and the white one for a dark taskbar. Windows has
// no template images.
func setIcon(ti *application.SystemTray, i icon.Icon) {
	ti.SetIcon(i.Light)
	ti.SetDarkModeIcon(i.Dark)
}

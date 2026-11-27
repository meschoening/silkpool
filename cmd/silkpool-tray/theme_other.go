//go:build !windows

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// followTheme does nothing beyond Wails: the menu bar tints the icon's
// template to match itself, and a theme change leaves its tooltip.
func (*tray) followTheme(*application.SystemTray) {}

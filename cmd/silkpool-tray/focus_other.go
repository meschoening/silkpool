//go:build !darwin

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// focusPanel does nothing beyond Wails' focus: on Windows, the panel
// takes the keyboard as it shows.
func focusPanel(*application.WebviewWindow) {}

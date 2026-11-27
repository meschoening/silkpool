package terminal

import "golang.org/x/sys/windows"

// isTerminal reports whether fd is a console's, which alone has a
// console mode.
func isTerminal(fd uintptr) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(fd), &mode) == nil
}

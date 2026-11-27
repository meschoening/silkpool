package terminal

import "golang.org/x/sys/unix"

// isTerminal reports whether fd has a terminal's settings, as isatty
// asks.
func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}

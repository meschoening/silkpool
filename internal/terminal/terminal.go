// Package terminal tells whether a file is a terminal that someone can
// answer a question on, which is what the node and Hub CLIs ask before
// they prompt.
//
// A character device isn't enough: /dev/null is one, and on Windows so
// is NUL, and a script, a cron job or a systemd unit gets its stdin from
// them. So it asks the OS whether the file is a terminal: for its
// termios on Linux and macOS, as isatty does, and for its console mode
// on Windows (#370).
package terminal

import "os"

// Is reports whether f is a terminal. A file Silkpool can't ask about,
// such as a closed one, isn't.
func Is(f *os.File) bool {
	conn, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var is bool
	if err := conn.Control(func(fd uintptr) { is = isTerminal(fd) }); err != nil {
		return false
	}
	return is
}

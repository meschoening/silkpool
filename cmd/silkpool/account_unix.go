//go:build unix

package main

import (
	"fmt"
	"os"
)

// accountMode runs do as the hidden mode name, which takes no arguments
// and runs as root.
func accountMode(name string, args []string, do func() error) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: silkpool "+name)
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "silkpool "+name+" runs as root, as the package's scripts run it")
		return 1
	}
	if err := do(); err != nil {
		fmt.Fprintln(os.Stderr, "silkpool "+name+":", err)
		return 1
	}
	return 0
}

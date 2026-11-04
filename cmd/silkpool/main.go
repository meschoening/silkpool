// Command silkpool is Silkpool's node binary: the node CLI that
// administrators run, and, through modes hidden from its help, the agent
// service and the helpers it starts as a slot account, which only the OS
// runs. See
// "Name the CLI and define its commands".
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/meschoening/silkpool/internal/cli"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/version"
)

// modes are the hidden modes, by their first argument, that only the OS
// runs. Each OS's file adds the ones it has.
var modes = map[string]func(ctx context.Context, args []string) int{}

// helper runs silkpool as one of the helpers the agent starts as a slot
// account, if args name one, and returns the exit code to exit with, and
// false if they name none. Each OS's file that has helpers sets it, as
// slot.Helper on Linux.
var helper = func(args []string) (code int, ok bool) { return 0, false }

// startCommand is what the CLI tells an administrator to run when the
// service isn't running. Each OS's file sets its own, which packaging
// supplies.
var startCommand = "start the Silkpool service"

func main() {
	args := os.Args[1:]
	// The helpers come first, with the signals' default handling, so that
	// stopping a helper's unit ends it.
	if code, ok := helper(args); ok {
		os.Exit(code)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) > 0 {
		if mode, ok := modes[args[0]]; ok {
			os.Exit(mode(ctx, args[1:]))
		}
	}
	os.Exit(cli.Run(ctx, args, cli.Env{
		Stdin:          os.Stdin,
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		Getenv:         os.Getenv,
		StdinTerminal:  terminal(os.Stdin),
		StdoutTerminal: terminal(os.Stdout),
		Socket:         control.Path,
		Version:        version.Version,
		StartCommand:   startCommand,
	}))
}

// terminal reports whether f is a terminal.
func terminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

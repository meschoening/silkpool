package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/slot"
	"github.com/meschoening/silkpool/packaging/macos"
)

func init() {
	modes["uninstall"] = runUninstall
}

// runUninstall is sudo silkpool uninstall, since macOS has no package
// uninstaller (ADR 0013). It's refused while the machine is enrolled,
// which the running agent answers. Once it isn't, it stops the agent and
// the tray app and deletes everything the .pkg installed and the agent
// made: the service account, as its ledger proves it Silkpool's, then
// the binary, the app, the launchd plists, the state and the logs. It's
// refused too while a slot's job is loaded, and keeps the state, with the
// account's ledger, while a process holds the account's UID or the
// account can't be deleted (macos.Uninstall).
func runUninstall(ctx context.Context, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: sudo silkpool uninstall")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "Uninstalling deletes what Silkpool installed, so run it with sudo: sudo silkpool uninstall")
		return 1
	}
	// With no agent to ask, the machine can't be enrolled only once its
	// state is gone, as after an uninstall that stopped partway.
	notRunning := ""
	if _, err := os.Lstat(installed.State); !errors.Is(err, fs.ErrNotExist) {
		notRunning = fmt.Sprintf("Silkpool's service isn't running on this machine, so it can't say whether it's enrolled. Start it with: %s", startCommand)
	}
	if refusal := askUninstall(ctx, control.Path, notRunning); refusal != "" {
		fmt.Fprintln(os.Stderr, refusal)
		return 1
	}
	u, err := uninstaller(ctx)
	if err == nil {
		err = u.Do(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "silkpool uninstall: %v\n", err)
		return 1
	}
	fmt.Println("Silkpool is uninstalled.")
	return 0
}

// uninstaller is the uninstall of this Mac: its root, its programs and
// processes, and its service account as the package lays it out, which
// it deletes, checking its ledger before and after, before it deletes
// the ledger.
func uninstaller(ctx context.Context) (macos.Uninstall, error) {
	slots, err := slot.Open(ctx, slot.Installed)
	if err != nil {
		return macos.Uninstall{}, err
	}
	return macos.Uninstall{Root: "/", Run: runProgram, Running: running, Log: os.Stdout, Unresolved: slots.Unresolved, DeleteAccount: slots.DeleteAccount}, nil
}

// programs are the programs the uninstall runs, by their full paths.
var programs = map[string]string{
	"launchctl": "/bin/launchctl",
	"pkgutil":   "/usr/sbin/pkgutil",
	"who":       "/usr/bin/who",
	"id":        "/usr/bin/id",
}

func runProgram(ctx context.Context, name string, args ...string) (macos.Result, error) {
	path, ok := programs[name]
	if !ok {
		return macos.Result{}, fmt.Errorf("%s isn't a program the uninstall runs", name)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return macos.Result{Code: exit.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}, nil
	}
	if err != nil {
		return macos.Result{}, err
	}
	return macos.Result{Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// running reports whether a process with the ID pid runs, as signal 0
// finds it: root may signal any process, so only ESRCH means there's
// none.
func running(pid int) (bool, error) {
	switch err := unix.Kill(pid, 0); {
	case err == nil, errors.Is(err, unix.EPERM):
		return true, nil
	case errors.Is(err, unix.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

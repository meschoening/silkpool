package slot

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Helper runs silkpool as one of the helpers Silkpool starts as a slot
// account, if args, the arguments after the program's name, name one:
// the runner's launcher (LaunchMode), the reset's kill helper
// (KillAllMode) or the exposure check (CheckMode). It returns the exit
// code to exit with, and false if args name none of them.
func Helper(args []string) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case LaunchMode:
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, LaunchMode+" takes the runner root")
			return 2, true
		}
		fmt.Fprintln(os.Stderr, Launch(args[1]))
		return 1, true
	case KillAllMode:
		uid, err := strconv.Atoi(strings.Join(args[1:], " "))
		if err != nil {
			fmt.Fprintln(os.Stderr, KillAllMode+" takes the slot account's UID")
			return 2, true
		}
		if err := KillAll(uid); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		return 0, true
	case CheckMode:
		return checkExposure(args[1:], os.Stdout), true
	}
	return 0, false
}

// LaunchMode is the hidden argument that runs silkpool as a runner's
// launcher, `silkpool launch-listener <runner root>`, which every runner
// unit starts as the slot account.
const LaunchMode = "launch-listener"

// configFile is the file in a runner root that holds its runner's JIT
// config until the launcher reads it.
const configFile = ".silkpool-jitconfig"

// listener is the runner application's listener program, in a runner
// root.
var listener = filepath.Join("bin", "Runner.Listener")

// configVar is the variable the listener reads its JIT config from, and
// deletes from its own environment before any job's step starts.
const configVar = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// forbidden are the variables a listener must never be started with:
// HTTP tracing dumps bearer tokens, and the stdout echo carries the
// Worker's lines onto the listener's output, as "Define Silkpool's
// events, audit trail, notices and redaction"
// found. A runner unit never sets them, but the service manager's own
// environment could.
var forbidden = []string{
	"GITHUB_ACTIONS_RUNNER_HTTPTRACE",
	"GITHUB_ACTIONS_RUNNER_TRACE",
	"ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT",
}

// Launch is the launcher of the runner whose root is root, which runs as
// the slot account (ADR 0006). It reads the runner's JIT config from the
// file the agent wrote, deletes the file, and becomes the runner's
// listener, `Runner.Listener run`, with the config in
// ACTIONS_RUNNER_INPUT_JITCONFIG, so that it's never on a command line or
// in the unit's settings. The listener's environment is the launcher's,
// without the variables no listener may be started with.
//
// Launch returns only if it fails, before the listener starts. The
// config file is gone by then, so the runner can't start again.
func Launch(root string) error {
	path := filepath.Join(root, configFile)
	config, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("can't read the runner's JIT config: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("can't delete the runner's JIT config file: %w", err)
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == configVar || slices.Contains(forbidden, name)
	})
	program := filepath.Join(root, listener)
	err = syscall.Exec(program, []string{program, "run"}, append(env, configVar+"="+string(config)))
	return fmt.Errorf("can't start the listener: %w", err)
}

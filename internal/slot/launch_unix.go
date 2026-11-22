//go:build unix

package slot

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// LaunchMode is the hidden argument that runs silkpool as a runner's
// launcher, `silkpool launch-listener <runner root>`, which every runner
// unit starts as the service account.
const LaunchMode = "launch-listener"

// configFile is the file in a runner root that holds its runner's JIT
// config until the launcher reads it.
const configFile = ".silkpool-jitconfig"

// listener is the runner application's listener program, in a runner
// root.
var listener = filepath.Join("bin", "Runner.Listener")

// WorkerProgram is the runner application's worker in a runner root:
// the program the listener starts to run its job, just after taking it
// (GLOSSARY.md's Worker).
const WorkerProgram = "bin/Runner.Worker"

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

// channelTimeoutVar, with every listener started with channelTimeout,
// is the runner's variable for how long, in seconds, its listener waits
// to hand the Worker it started a message, the job above all, and channelTimeout
// its longest, 300. The default, 30, can run out on a busy machine
// before the Worker is up, since the slots give way on CPU to the
// machine's other work (ADR 0014). The listener then kills the Worker
// and exits without completing the job, which GitHub fails only when the
// job's lease runs out, 10 minutes on (#285). The wait ends as soon as
// the Worker takes the message, so it costs nothing when the machine
// is quiet.
const (
	channelTimeoutVar = "GITHUB_ACTIONS_RUNNER_CHANNEL_TIMEOUT"
	channelTimeout    = "300"
)

// Launch is the launcher of the runner whose root is root, which runs as
// the service account (ADR 0006). It reads the runner's JIT config from the
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

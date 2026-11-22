// Package winslot is the Windows slot host and the services that run it
// (ADR 0006, as "Use one Silkpool service account per machine instead
// of per-slot accounts" amended it). Built in "Build the Windows slot
// host".
//
// Each slot has a demand-start Windows service, its name a parameter,
// that runs silkpool.exe in its hidden slot-host mode (Mode) as
// LocalSystem, from a copy of the program for its version (Copies),
// never the installed one. The agent starts the service with the
// runner's Spec as its start arguments. The host:
//
//  1. reads the runner's JIT config from its file in the runner root,
//     and deletes the file;
//  2. logs on as the machine's service account with LsaLogonUser,
//     MSV1_0_S4U_LOGON, logon type Batch and the token source
//     "Silkpool", and checks the token is the account's SID;
//  3. loads the account's profile;
//  4. starts the listener, `Runner.Listener.exe run`, in the slot's
//     job object, which it makes nested in the node job, with the JIT
//     config in its environment block, TEMP and TMP in the slot's own
//     folder, and its output in the log file the agent tails;
//  5. holds all of them until the listener exits, then ends whatever is
//     left in the job, unloads the profile and stops, with the
//     listener's exit code as its service-specific exit code.
//
// ControlCtrlC is a graceful stop: the host attaches to the listener's
// console and sends Ctrl+C, as GitHub's RunnerService.exe does, and
// terminates the job Grace later. ControlKill terminates it at once,
// after ControlCtrlC too. The service's own stop and shutdown controls
// send Ctrl+C as ControlCtrlC does, but once the service manager has
// delivered a stop it refuses every later control, ControlKill among
// them, so the agent never sends one. The agent and a host talk only
// through these SCM controls, the
// service's state and files, so an agent and a host one version apart
// keep working together across an upgrade, and a restarted agent asks
// the SCM which slot services still run (Adopt).
//
// The node job (NodeJob), which the agent keeps, is a named job object
// open to SYSTEM alone that holds every slot's job, so the node's budget
// bounds them together (ADR 0014): a job memory limit, which counts
// commit, a CPU rate hard cap, and always the BELOW_NORMAL priority
// class. The host holds it while its listener runs, so it outlives an
// agent restart with its limits, and starts nothing without it. A commit
// past the memory limit fails while its process carries on, and the
// agent's Watch names the slot whose job held the process, from the
// message Windows sends with its process ID. Built in "Build the node
// budget on Windows".
//
// What doesn't need Windows, the spec, the listener's environment, the
// copies, the rules for moving a service between them and the node job's
// limits, is portable and tested in every lane.
package winslot

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Mode is the hidden first argument that runs silkpool.exe as a slot
// host, which only the service manager starts.
const Mode = "slot-host"

// ConfigFile is the file in a runner root that holds its runner's JIT
// config until the host reads it, as on Linux and macOS.
const ConfigFile = ".silkpool-jitconfig"

// ConfigVar is the variable the listener reads its JIT config from, and
// deletes from its own environment before any job's step starts.
const ConfigVar = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// Forbidden are the variables a listener must never be started with:
// HTTP tracing dumps bearer tokens, and the stdout echo carries the
// Worker's lines onto the listener's output, as "Define Silkpool's
// events, audit trail, notices and redaction" found. The account's
// environment could hold them, so the host drops them.
var Forbidden = []string{
	"GITHUB_ACTIONS_RUNNER_HTTPTRACE",
	"GITHUB_ACTIONS_RUNNER_TRACE",
	"ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT",
}

// Listener is the runner application's listener program, in a runner
// root.
var Listener = filepath.Join("bin", "Runner.Listener.exe")

// Grace is how long after Ctrl+C the host terminates the listener's job
// object, as the reset does on Linux and macOS (ADR 0006).
const Grace = 30 * time.Second

// ControlKill is the service control, one of those a service defines for
// itself (128 to 255), that terminates the listener's job object at
// once. It's part of the protocol between the agent and an older host,
// so it never changes.
const ControlKill = 128

// ControlCtrlC is the service control, another of a service's own, that
// sends the listener Ctrl+C and terminates its job Grace later unless it
// has exited: the agent's graceful stop. The agent sends it, not the
// service's stop control, because the service manager refuses every
// control after a stop, and ControlKill must still reach the host
// during the grace. Like ControlKill, it never changes. A host older
// than it ignores it, so only ControlKill or the listener's own exit
// ends that host.
const ControlCtrlC = 129

// A Spec is what a slot host needs to start one runner's listener. The
// agent starts the slot's service with it as the start arguments (Args),
// which no other account can read, and never on the service's command
// line. It holds no secret: the JIT config is in a file in Root.
type Spec struct {
	// Account is the service account's name, and SID its SID as the agent
	// recorded it. The host logs on as Account, and refuses a token for
	// any other SID.
	Account, SID string
	// Root is the runner's root, which holds the JIT config file and the
	// listener, and is the listener's working folder.
	Root string
	// Temp is the slot's own temp folder, the listener's TEMP and TMP.
	Temp string
	// Log is the file the listener's output goes to, which the agent
	// tails. The host makes it or adds to it.
	Log string
	// NodeJob names the node job, which the agent keeps, and Job the
	// slot's own job, which the host makes inside it to hold the
	// listener (NodeJobName and SlotJob).
	NodeJob, Job string
}

// Args returns s as the start arguments of a slot's service.
func (s Spec) Args() []string {
	return []string{"-account", s.Account, "-sid", s.SID, "-root", s.Root, "-temp", s.Temp, "-log", s.Log, "-node-job", s.NodeJob, "-job", s.Job}
}

// ParseSpec reads a Spec from a service's start arguments, refusing
// any missing, any unknown, a relative path and anything after them.
func ParseSpec(args []string) (Spec, error) {
	var s Spec
	fs := flag.NewFlagSet(Mode, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&s.Account, "account", "", "")
	fs.StringVar(&s.SID, "sid", "", "")
	fs.StringVar(&s.Root, "root", "", "")
	fs.StringVar(&s.Temp, "temp", "", "")
	fs.StringVar(&s.Log, "log", "", "")
	fs.StringVar(&s.NodeJob, "node-job", "", "")
	fs.StringVar(&s.Job, "job", "", "")
	if err := fs.Parse(args); err != nil {
		return Spec{}, fmt.Errorf("the slot host's start arguments: %w", err)
	}
	if fs.NArg() > 0 {
		return Spec{}, fmt.Errorf("the slot host's start arguments end with %q", fs.Args())
	}
	var errs []error
	for _, f := range []struct{ name, value string }{{"account", s.Account}, {"sid", s.SID}, {"node-job", s.NodeJob}, {"job", s.Job}} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("no -%s", f.name))
		}
	}
	for _, f := range []struct{ name, value string }{{"root", s.Root}, {"temp", s.Temp}, {"log", s.Log}} {
		if !absolute(f.value) {
			errs = append(errs, fmt.Errorf("-%s %q isn't an absolute path", f.name, f.value))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Spec{}, fmt.Errorf("the slot host's start arguments: %w", err)
	}
	return s, nil
}

// absolute reports whether p is an absolute path on Windows, a drive's
// (C:\...) or a share's (\\server\share\...), whatever OS tests it.
func absolute(p string) bool {
	if strings.HasPrefix(p, `\\`) {
		return len(p) > 2
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		('a' <= p[0]|0x20 && p[0]|0x20 <= 'z')
}

// VersionRefusedVar is the variable that has the listener exit with
// node.ExitVersionRefused when GitHub refuses its version, as every
// listener on Linux and macOS is started with.
const VersionRefusedVar = "ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE"

// ChannelTimeoutVar, with every listener started with ChannelTimeout,
// is the runner's variable for how long, in seconds, its listener waits
// to hand the Worker it started a message, the job above all, and ChannelTimeout
// its longest, 300. The default, 30, can run out on a busy machine
// before the Worker is up, since the slots give way on CPU to the
// machine's other work (ADR 0014). The listener then kills the Worker
// and exits without completing the job, which GitHub fails only when the
// job's lease runs out, 10 minutes on (#285). The wait ends as soon as
// the Worker takes the message, so it costs nothing when the machine
// is quiet.
const (
	ChannelTimeoutVar = "GITHUB_ACTIONS_RUNNER_CHANNEL_TIMEOUT"
	ChannelTimeout    = "300"
)

// listenerEnv returns the listener's environment: the account's own,
// env, without ConfigVar, Forbidden, VersionRefusedVar,
// ChannelTimeoutVar, TEMP and TMP, then TEMP and TMP in the slot's temp
// folder, VersionRefusedVar and ChannelTimeoutVar set, and the JIT
// config in ConfigVar. Windows' variable names ignore case, so
// the names are matched ignoring it.
func listenerEnv(env []string, s Spec, config string) []string {
	drop := append([]string{ConfigVar, VersionRefusedVar, ChannelTimeoutVar, "TEMP", "TMP"}, Forbidden...)
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		// A variable whose name starts with "=", such as "=C:", is the
		// shell's, and keeps its first "=".
		name, _, _ := strings.Cut(kv[min(1, len(kv)):], "=")
		name = kv[:min(1, len(kv))] + name
		return slices.ContainsFunc(drop, func(d string) bool { return strings.EqualFold(d, name) })
	})
	return append(out, "TEMP="+s.Temp, "TMP="+s.Temp, VersionRefusedVar+"=1", ChannelTimeoutVar+"="+ChannelTimeout, ConfigVar+"="+config)
}

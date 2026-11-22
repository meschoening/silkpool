// Package cli is the node CLI, `silkpool`: every command an
// administrator runs on a node, which it sends to the local agent over
// the control channel. The decisions behind it are in "Name the CLI and
// define its commands".
//
// It acts only on its own machine, and only through the agent. Its words
// come from package wording, its human output is plain text with color
// only on a terminal, and every read and reply also comes as JSON with
// --json, the automation contract. The exit codes are 0 for applied or
// nothing to change, 1 for refused, 2 for a usage error, 3 when it
// couldn't ask the agent, and 4 for Not confirmed.
//
// The tray app reads and commands the agent through the same client, as
// a Link, so its revisions, resends and check of the server are the
// CLI's.
package cli

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/winexecpolicy"
	"github.com/meschoening/silkpool/internal/winsac"
	"github.com/meschoening/silkpool/internal/wording"
	"github.com/meschoening/silkpool/internal/zone"
)

// The exit codes.
const (
	ExitOK           = 0
	ExitRefused      = 1
	ExitUsage        = 2
	ExitCantAsk      = 3
	ExitNotConfirmed = 4
)

// Env is what the CLI runs with.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Getenv reads the environment, for NO_COLOR.
	Getenv func(string) string
	// StdinTerminal and StdoutTerminal report that they're terminals:
	// prompts need the one, and color the other.
	StdinTerminal, StdoutTerminal bool
	// Socket is the control socket, and ServerUID the uid the agent must
	// run as: control.Path and 0, outside tests.
	Socket    string
	ServerUID int
	// Dial connects to the agent at Socket, accepting only a server
	// running as ServerUID, or is nil for control.Dial. Only tests set it.
	Dial func(ctx context.Context, socket string, serverUID int) (*control.Client, error)
	// Version is this CLI's version.
	Version string
	// StartCommand is the command that starts the service, which the CLI
	// names when it isn't running.
	StartCommand string
	// Elevate says how to run a command that needs elevation, as in
	// "enroll changes this machine's accounts, so run it <Elevate>.", or
	// is empty for "with sudo".
	Elevate string
	// NewID returns a new command ID, or is nil for a random one.
	NewID func() string
	// SmartAppControl reads Windows' Smart App Control, which enroll
	// names while it's On or in Evaluation (winsac.Read), or is nil where
	// there's none.
	SmartAppControl func() (winsac.State, error)
	// ExecutionPolicy reads the execution policy each PowerShell's
	// scripts meet, which enroll names while it blocks a job's
	// PowerShell steps (winexecpolicy.Read), or is nil where there's no
	// PowerShell.
	ExecutionPolicy func() []winexecpolicy.Reading
}

// usage is the help, which leaves out the commands only the OS runs.
const usage = `Usage: silkpool <command> [flags]

Commands:
  status [--json] [--watch]          Show what this node is doing.
  dismiss [--json]                   Dismiss the notices about something that happened.
  pause [--indefinitely] [--json]    Stop taking new jobs. Running jobs finish first.
  resume [--revision N] [--json]     Take jobs again.
  emergency-stop [--yes] [--json]    Cancel the running jobs and stay paused.
  auto-resume [on|set|off] [flags]   Show or change when a pause ends by itself.
  slot-limit [<n>] [--revision N]    Show or change how many slots this node has.
  budget [--memory|--cpus|--none]    Show or change the memory and CPU its slots may use.
  enroll <code> [--name <name>]      Enroll this machine in a Hub. Needs sudo.
  remove [--force] [--yes] [--json]  Remove this machine from its Hub. Needs sudo.
  set-hub-url <url> [--json]         Point this node at its Hub's new URL. Needs sudo.
  diagnostics [--json]               Print the diagnostics report.
  version [--json]                   Print the CLI's and the service's versions.

Only administrators can use Silkpool on this machine.
`

// Run runs the CLI with args, the command line after the program's
// name, and returns its exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if env.NewID == nil {
		env.NewID = randomID
	}
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	s := &session{env: env, color: env.StdoutTerminal && env.Getenv("NO_COLOR") == ""}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	cmd, ok := commands[args[0]]
	if !ok {
		if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
			fmt.Fprint(env.Stdout, usage)
			return ExitOK
		}
		fmt.Fprintf(env.Stderr, "silkpool: %q isn't a command.\n\n%s", args[0], usage)
		return ExitUsage
	}
	defer s.close()
	return cmd(ctx, s, args[1:])
}

// commands are the CLI's commands by name.
var commands map[string]func(context.Context, *session, []string) int

func init() {
	commands = map[string]func(context.Context, *session, []string) int{
		"status":         runStatus,
		"dismiss":        runDismiss,
		"pause":          runPause,
		"resume":         runResume,
		"emergency-stop": runEmergencyStop,
		"auto-resume":    runAutoResume,
		"slot-limit":     runSlotLimit,
		"budget":         runBudget,
		"enroll":         runEnroll,
		"remove":         runRemove,
		"set-hub-url":    runSetHubURL,
		"diagnostics":    runDiagnostics,
		"version":        runVersion,
	}
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "cli-" + hex.EncodeToString(b)
}

// A session is one run of the CLI: its flags, and its connection to the
// agent. A Link's commands run as sessions too, from the tray.
type session struct {
	env    Env
	color  bool
	json   bool
	client *control.Client
	// tray reports that the session is the tray app's, whose commands
	// come from the tray.
	tray bool
}

func (s *session) close() {
	if s.client != nil {
		s.client.Close()
	}
}

// flags returns a flag set for a command, which takes --json unless
// noJSON.
func (s *session) flags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&s.json, "json", false, "")
	fs.Lookup("json").Usage = synopsis
	return fs
}

// parse parses a command's flags and its positional arguments, which can
// come before or after the flags, and returns the arguments, or reports a
// usage error.
func (s *session) parse(fs *flag.FlagSet, args []string, positional int) ([]string, int, bool) {
	return s.parseUpTo(fs, args, positional, positional)
}

// parseUpTo is parse for a command that takes from least to most
// positional arguments.
func (s *session) parseUpTo(fs *flag.FlagSet, args []string, least, most int) ([]string, int, bool) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, s.usage(fs, err.Error()), false
		}
		if fs.NArg() == 0 {
			break
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(rest) < least || len(rest) > most {
		return nil, s.usage(fs, ""), false
	}
	return rest, 0, true
}

// usage reports a usage error for a command.
func (s *session) usage(fs *flag.FlagSet, problem string) int {
	if problem != "" {
		fmt.Fprintf(s.env.Stderr, "silkpool %s: %s\n", fs.Name(), problem)
	}
	fmt.Fprintf(s.env.Stderr, "Usage: %s\n", fs.Lookup("json").Usage)
	return ExitUsage
}

// cantAsk reports that the CLI couldn't ask the agent, as a sentence or
// as JSON, and returns ExitCantAsk.
func (s *session) cantAsk(code, sentence string) int {
	if s.json {
		s.printJSON(map[string]any{"error": control.Error{Code: code, Message: sentence}})
	} else {
		fmt.Fprintln(s.env.Stderr, sentence)
	}
	return ExitCantAsk
}

// connect connects to the agent, checking that it's the same version as
// the CLI unless anyVersion, and returns 0, or the exit code once it has
// said why it couldn't.
func (s *session) connect(ctx context.Context, anyVersion bool) int {
	c, err := s.dial(ctx)
	if err != nil {
		return s.dialFailed(err)
	}
	s.client = c
	if !anyVersion && c.Hello.Version != s.env.Version {
		return s.cantAsk("version", fmt.Sprintf("This CLI is %s but the running service is %s. Restart the service.", s.env.Version, c.Hello.Version))
	}
	return ExitOK
}

// dial connects to the agent through Env's Dial, or control.Dial.
func (s *session) dial(ctx context.Context) (*control.Client, error) {
	dial := control.Dial
	if s.env.Dial != nil {
		dial = s.env.Dial
	}
	c, err := dial(ctx, s.env.Socket, s.env.ServerUID)
	if err != nil {
		return nil, err
	}
	c.Tray = s.tray
	return c, nil
}

// dialFailed says why the CLI couldn't connect.
func (s *session) dialFailed(err error) int {
	var notServer control.NotServer
	var notAdmin control.NotAdmin
	switch {
	case errors.Is(err, control.ErrNotRunning):
		return s.cantAsk("not-running", fmt.Sprintf("Silkpool's service isn't running on this machine. Start it with: %s", s.env.StartCommand))
	case errors.As(err, &notServer):
		return s.cantAsk("not-the-service", fmt.Sprintf("Something other than Silkpool's service is listening at %s, so the CLI won't talk to it.", notServer.Path))
	case errors.As(err, &notAdmin):
		return s.cantAsk(control.CodeNotAdmin, fmt.Sprintf("Only administrators can use Silkpool on this machine, and %s isn't one.", notAdmin.Account))
	}
	return s.cantAsk("unreachable", wording.Sentence(fmt.Sprintf("couldn't ask Silkpool's service: %v", err)))
}

// elevated returns 0 if the caller is elevated, or says what to do and
// returns ExitCantAsk.
func (s *session) elevated(why string) int {
	if s.client.Hello.Elevated {
		return ExitOK
	}
	return s.cantAsk(control.CodeNotElevated, why+", so run it "+cmp.Or(s.env.Elevate, "with sudo")+".")
}

// a read is the status the CLI read, decoded, with its view.
type read struct {
	status control.Status
	node   node.Node
	view   wording.View
}

// readStatus reads the agent's status, or returns the exit code once
// it has said why it couldn't.
func (s *session) readStatus(ctx context.Context) (read, int) {
	st, err := s.client.Status(ctx)
	if err != nil {
		return read{}, s.failed(err)
	}
	r, err := decode(st)
	if err != nil {
		return read{}, s.failed(err)
	}
	return r, ExitOK
}

// decode decodes a status for the words.
func decode(st control.Status) (read, error) {
	n, err := st.Node.ToNode()
	if err != nil {
		return read{}, err
	}
	now, err := store.Unstamp(st.Now)
	if err != nil {
		return read{}, err
	}
	since, err := store.Unstamp(st.Hub.Since)
	if err != nil {
		return read{}, err
	}
	// Before its schedule has a zone, the node shows times in its OS's,
	// or in Central if the agent found none.
	z := n.Availability.AutoResume.Zone
	if z == nil {
		z = zone.Own(st.Machine.Zone)
	}
	view := wording.View{Node: n, Name: shortName(st.Machine.Hostname), HubSince: since,
		Clock: wording.Clock{Now: now, Zone: z, Own: st.Machine.Zone}}
	return read{status: st, node: n, view: view}, nil
}

// with is the view of r's node with the settings a, as a command left
// them, printing times in a's auto-resume zone.
func (r read) with(a availability.Node) wording.View {
	v := r.view
	v.Node.Availability = a
	if a.AutoResume.Zone != nil {
		v.Clock.Zone = a.AutoResume.Zone
	}
	return v
}

// shortName is a host name without its domain.
func shortName(host string) string {
	name, _, _ := strings.Cut(host, ".")
	if name == "" {
		return "this machine"
	}
	return name
}

// enrolled returns 0 if the machine is enrolled, or says it isn't and
// returns ExitCantAsk.
func (s *session) enrolled(r read) int {
	if r.node.Enrolled {
		return ExitOK
	}
	return s.cantAsk("not-enrolled", "This machine isn't enrolled in a Hub.")
}

// failed reports an error from the agent: a refusal exits 1, and
// anything else means the CLI couldn't ask.
func (s *session) failed(err error) int {
	var e *control.Error
	if errors.As(err, &e) && e.Code != control.CodeStopped && e.Code != control.CodeBadRequest {
		if s.json {
			s.printJSON(map[string]any{"applied": false, "refused": e})
		} else {
			fmt.Fprintln(s.env.Stderr, wording.Sentence(e.Message))
		}
		return ExitRefused
	}
	return s.cantAsk("unreachable", wording.Sentence(fmt.Sprintf("couldn't ask Silkpool's service: %v", err)))
}

// ErrNotConfirmed is what a command returns when the connection to the
// agent dropped before its answer, twice: it may have applied.
var ErrNotConfirmed = errors.New("not confirmed: the connection to Silkpool's service dropped before it answered, twice")

// once runs do, and if the connection drops before its answer,
// reconnects and runs it once more, which the agent answers with the
// first answer if the first one applied. Once the first has dropped, it
// returns ErrNotConfirmed unless the agent answers the second: the first
// may have applied.
func (s *session) once(ctx context.Context, do func(*control.Client) error) error {
	err := do(s.client)
	if !errors.Is(err, control.ErrDropped) {
		return err
	}
	s.client.Close()
	s.client = nil
	c, err := s.dial(ctx)
	if err != nil {
		return ErrNotConfirmed
	}
	s.client = c
	if err = do(c); err != nil && !errors.As(err, new(*control.Error)) {
		return ErrNotConfirmed
	}
	return err
}

// send sends a command through do, with its one resend under the same
// command ID. It returns the reply, or the exit code once it has said
// why there's none.
func (s *session) send(ctx context.Context, do func(*control.Client) (control.Reply, error)) (control.Reply, int) {
	var r control.Reply
	err := s.once(ctx, func(c *control.Client) (err error) {
		r, err = do(c)
		return err
	})
	switch {
	case errors.Is(err, ErrNotConfirmed):
		return r, s.notConfirmed()
	case err != nil:
		return r, s.failed(err)
	}
	return r, ExitOK
}

// notConfirmed says the command may or may not have applied.
func (s *session) notConfirmed() int {
	msg := "Not confirmed: the connection to Silkpool's service dropped before it answered, twice. Run `silkpool status` to see whether it applied."
	if s.json {
		s.printJSON(map[string]any{"error": control.Error{Code: "not-confirmed", Message: msg}})
	} else {
		fmt.Fprintln(s.env.Stderr, msg)
	}
	return ExitNotConfirmed
}

// printJSON prints v as indented JSON.
func (s *session) printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(s.env.Stderr, err)
		return
	}
	fmt.Fprintln(s.env.Stdout, string(b))
}

// replyJSON is a command's reply as --json prints it.
type replyJSON struct {
	Applied  bool               `json:"applied"`
	ID       string             `json:"id"`
	Revision int64              `json:"revision"`
	Settings store.Availability `json:"settings"`
	Refused  *control.Error     `json:"refused,omitempty"`
}

func toJSON(r control.Reply) replyJSON {
	return replyJSON{Applied: r.Refused == nil, ID: r.ID, Revision: r.Settings.Revision, Settings: r.Settings, Refused: r.Refused}
}

// confirm asks question and reports whether the answer is yes. Without a
// terminal it doesn't ask: it says to use --yes and returns false, with
// ExitUsage.
func (s *session) confirm(command, question string) (bool, int) {
	if !s.env.StdinTerminal {
		fmt.Fprintf(s.env.Stderr, "%s asks before it acts, and there's no terminal to ask on. Run it again with --yes.\n", command)
		return false, ExitUsage
	}
	fmt.Fprintf(s.env.Stdout, "%s [y/N] ", question)
	line, _ := bufio.NewReader(s.env.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, ExitOK
	}
	fmt.Fprintln(s.env.Stdout, "Nothing changed.")
	return false, ExitRefused
}

// amber and red color text on a terminal, unless NO_COLOR is set.
func (s *session) amber(text string) string { return s.paint("33", text) }
func (s *session) red(text string) string   { return s.paint("31", text) }

func (s *session) paint(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

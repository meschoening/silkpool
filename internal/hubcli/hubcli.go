// Package hubcli is the Hub's CLI, silkpool-hub: every command the
// operator runs as root on the Hub's host. Each asks the running Hub
// through its socket (package hubsocket), except init, which sets the
// Hub up before it runs (package hubservice). The decisions behind it
// are in "Name the CLI and define its commands".
//
// It acts only on its own machine. Its words come from package wording,
// and --json prints the operation's reply as the Hub gave it, which is
// the automation contract. The exit codes are the node CLI's: 0 for
// applied or nothing to change, 1 for refused, 2 for a usage error, and
// 3 when it couldn't ask the Hub.
package hubcli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/cli"
	"github.com/meschoening/silkpool/internal/hubsocket"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wording"
)

// The exit codes, the node CLI's.
const (
	ExitOK      = cli.ExitOK
	ExitRefused = cli.ExitRefused
	ExitUsage   = cli.ExitUsage
	ExitCantAsk = cli.ExitCantAsk
)

// Env is what the CLI runs with.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// StdinTerminal reports that stdin is a terminal, which a prompt
	// needs.
	StdinTerminal bool
	// Socket is the Hub's socket, and ServerUID the uid the Hub must run
	// as: hubsocket.Path and the silkpool-hub account's, outside tests.
	Socket    string
	ServerUID int
	// Version is this CLI's version.
	Version string
	// Zone is the Hub's host's time zone, which times print in, and Now
	// reads its clock.
	Zone *time.Location
	Now  func() time.Time
	// Root is the root of the file system init writes into, "/" outside
	// tests, and Elevated reports that the CLI runs as root, which init
	// needs. Systemctl runs systemctl with args.
	Root      string
	Elevated  bool
	Systemctl func(ctx context.Context, args ...string) error
	// NewID returns a removal's command ID, or is nil for a random one.
	// NewSecret returns init's setup code, or is nil for a random one.
	// Only tests set them.
	NewID     func() string
	NewSecret func() secret.Value
	// Wait waits between the looks at a node's removal, or is nil for a
	// second.
	Wait func(ctx context.Context) error
}

// startCommand is what starts the Hub's service.
const startCommand = "sudo systemctl start silkpool-hub"

// usage is the help, which leaves out serve, which only the OS runs.
const usage = `Usage: silkpool-hub <command> [flags]

Setting up and running the Hub:
  init --url <url> [--cert <file> --key <file> | --behind-proxy] [--listen <addr:port>]
                                       Set the Hub up and start it.
  setup-code [--reassign]              Make a fresh setup link.
  set-url <url>                        Change the Hub's URL, then restart it.
  status                               Show the Hub's settings, GitHub and pool.
  version                              Print the CLI's and the Hub's versions.

Enrollment codes:
  code new                             Make a code that enrolls one machine.
  code list                            Show the codes and their state.
  code revoke <id>                     Revoke an open code.

Nodes:
  node list                            Show each node.
  node rename <name> <new-name>        Rename a node.
  node set-target <name> <target>      Give a node another target.
  node label add|remove <name> <label>...
                                       Add or remove a node's own labels.
  node remove <name> [--yes]           Remove a node from the Hub for good.

Targets:
  target list                          Show the targets and the nodes on each.
  target available                     Show what GitHub offers as targets.
  target add|remove|default <target>   Add or remove a target, or make it the default.
  pool-label [<label>]                 Show or rename the pool label.

Every command takes --json. Only root can use silkpool-hub.
`

// A command runs one command.
type command func(ctx context.Context, s *session, args []string) int

// commands are the CLI's commands by name, and its nouns' by noun and
// verb.
var commands map[string]command

func init() {
	commands = map[string]command{
		"init":       runInit,
		"setup-code": runSetupCode,
		"set-url":    runSetURL,
		"status":     runStatus,
		"version":    runVersion,
		"code":       noun("code", map[string]command{"new": runCodeNew, "list": runCodeList, "revoke": runCodeRevoke}),
		"node": noun("node", map[string]command{
			"list": runNodeList, "rename": runNodeRename, "set-target": runNodeSetTarget,
			"label":  noun("node label", map[string]command{"add": runLabels("add"), "remove": runLabels("remove")}),
			"remove": runNodeRemove,
		}),
		"target": noun("target", map[string]command{
			"list": runTargetList, "available": runTargetAvailable,
			"add": runTarget("add"), "remove": runTarget("remove"), "default": runTarget("default"),
		}),
		"pool-label": runPoolLabel,
	}
}

// Run runs the CLI with args, the command line after the program's
// name, and returns its exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if env.NewID == nil {
		env.NewID = randomID
	}
	if env.Zone == nil {
		env.Zone = time.Local
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Wait == nil {
		env.Wait = func(ctx context.Context) error {
			select {
			case <-time.After(time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	s := &session{env: env}
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
		fmt.Fprintf(env.Stderr, "silkpool-hub: %q isn't a command.\n\n%s", args[0], usage)
		return ExitUsage
	}
	return cmd(ctx, s, args[1:])
}

// noun runs one of a noun's verbs.
func noun(name string, verbs map[string]command) command {
	return func(ctx context.Context, s *session, args []string) int {
		if len(args) == 0 {
			fmt.Fprintf(s.env.Stderr, "silkpool-hub %s: which? %s\n", name, verbList(verbs))
			return ExitUsage
		}
		verb, ok := verbs[args[0]]
		if !ok {
			fmt.Fprintf(s.env.Stderr, "silkpool-hub %s: %q isn't one of %s.\n", name, args[0], verbList(verbs))
			return ExitUsage
		}
		return verb(ctx, s, args[1:])
	}
}

func verbList(verbs map[string]command) string {
	var names []string
	for v := range verbs {
		names = append(names, v)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "hub-cli-" + hex.EncodeToString(b)
}

// A session is one run of the CLI.
type session struct {
	env  Env
	json bool
}

// clock is the clock times print by.
func (s *session) clock() wording.Clock {
	return wording.Clock{Now: s.env.Now(), Zone: s.env.Zone, Own: s.env.Zone.String()}
}

// flags returns a flag set for a command, which takes --json.
func (s *session) flags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&s.json, "json", false, "")
	fs.Lookup("json").Usage = synopsis
	return fs
}

// parse parses a command's flags and its positional arguments, which can
// come before or after the flags: from least to most of them, or any
// number from least if most is negative. It returns the arguments, or
// reports a usage error.
func (s *session) parse(fs *flag.FlagSet, args []string, least, most int) ([]string, int, bool) {
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
	if len(rest) < least || most >= 0 && len(rest) > most {
		return nil, s.usage(fs, ""), false
	}
	return rest, ExitOK, true
}

// usage reports a usage error for a command.
func (s *session) usage(fs *flag.FlagSet, problem string) int {
	if problem != "" {
		fmt.Fprintf(s.env.Stderr, "silkpool-hub %s: %s\n", fs.Name(), problem)
	}
	fmt.Fprintf(s.env.Stderr, "Usage: %s\n", fs.Lookup("json").Usage)
	return ExitUsage
}

// ask sends req to the Hub and decodes its reply into out. It returns
// ExitOK, or the exit code once it has said why there's no reply to
// print: a refusal, or that it couldn't ask. With --json, it prints the
// reply as it came first, whatever it holds.
func (s *session) ask(ctx context.Context, req hubsocket.Request, out any) int {
	req.Version = s.env.Version
	r, err := hubsocket.Do(ctx, s.env.Socket, s.env.ServerUID, req)
	if err != nil {
		return s.cantAsk(err)
	}
	if s.json {
		s.printRaw(r.Raw)
	}
	switch {
	case r.Refused != nil:
		if !s.json {
			fmt.Fprintln(s.env.Stderr, wording.Sentence(r.Refused.Message))
		}
		return ExitRefused
	case r.Error != nil:
		if !s.json {
			fmt.Fprintln(s.env.Stderr, s.failure(r.Error))
		}
		return ExitCantAsk
	}
	if err := json.Unmarshal(r.Raw, out); err != nil {
		fmt.Fprintf(s.env.Stderr, "The Hub's reply to %s isn't one the CLI reads: %v\n", req.Op, err)
		return ExitCantAsk
	}
	return ExitOK
}

// failure says what a failure the Hub replied with means.
func (s *session) failure(e *hubsocket.Error) string {
	switch e.Code {
	case hubsocket.CodeNotRoot:
		return "Only root can use silkpool-hub. Run it with sudo."
	case hubsocket.CodeVersion:
		return wording.Sentence(e.Message) + " Restart it with: sudo systemctl restart silkpool-hub"
	}
	return wording.Sentence(e.Message)
}

// cantAsk says why the CLI couldn't ask the Hub, as a sentence or as
// JSON, and returns ExitCantAsk.
func (s *session) cantAsk(err error) int {
	code, sentence := "unreachable", wording.Sentence(fmt.Sprintf("couldn't ask the Hub: %v", err))
	var notServer hubsocket.NotServer
	switch {
	case errors.Is(err, hubsocket.ErrNotRunning):
		code, sentence = "not-running", "The Hub's service isn't running on this machine. Start it with: "+startCommand+", or set the Hub up first with silkpool-hub init."
	case errors.Is(err, hubsocket.ErrNotRoot):
		code, sentence = hubsocket.CodeNotRoot, "Only root can use silkpool-hub. Run it with sudo."
	case errors.As(err, &notServer):
		code, sentence = "not-the-hub", fmt.Sprintf("Something other than the Hub's service is listening at %s, so the CLI won't talk to it.", notServer.Path)
	}
	return s.say(code, sentence, ExitCantAsk)
}

// say reports a failure the CLI found itself, as a sentence or as JSON,
// and returns exit.
func (s *session) say(code, sentence string, exit int) int {
	if s.json {
		s.printJSON(map[string]any{"error": hubsocket.Error{Code: code, Message: sentence}})
	} else {
		fmt.Fprintln(s.env.Stderr, sentence)
	}
	return exit
}

// printRaw prints a reply as it came, indented.
func (s *session) printRaw(raw []byte) {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		b.Reset()
		b.Write(raw)
	}
	fmt.Fprintln(s.env.Stdout, b.String())
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

// out prints a line.
func (s *session) out(format string, args ...any) { fmt.Fprintf(s.env.Stdout, format+"\n", args...) }

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

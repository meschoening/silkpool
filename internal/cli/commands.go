package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/wire"
	"github.com/meschoening/silkpool/internal/wording"
)

// begin parses a command's flags, connects to the agent and reads its
// status, which must be an enrolled node's. It returns the status, or
// the exit code once it has said why it can't go on.
func (s *session) begin(ctx context.Context, parse func() (int, bool)) (read, int) {
	if code, ok := parse(); !ok {
		return read{}, code
	}
	if code := s.connect(ctx, false); code != ExitOK {
		return read{}, code
	}
	r, code := s.readStatus(ctx)
	if code != ExitOK {
		return r, code
	}
	return r, s.enrolled(r)
}

// command sends an availability command, and says why if it's refused:
// a stale refusal prints what changed since before, the settings the CLI
// read, or since the revision --revision gave. It returns the reply, and
// the exit code, which is ExitOK if it applied.
func (s *session) command(ctx context.Context, r read, e availability.Event, given bool) (control.Reply, availability.Node, int) {
	id := s.env.NewID()
	reply, code := s.send(ctx, func(c *control.Client) (control.Reply, error) { return c.Command(ctx, id, e) })
	if code != ExitOK {
		return reply, availability.Node{}, code
	}
	after, err := settings(reply)
	if err != nil {
		return reply, after, s.failed(err)
	}
	if s.json {
		s.printJSON(toJSON(reply))
	}
	if reply.Refused == nil {
		return reply, after, ExitOK
	}
	if !s.json {
		s.refused(r, reply, after, given, e)
	}
	return reply, after, ExitRefused
}

// refused says why a command was refused.
func (s *session) refused(r read, reply control.Reply, after availability.Node, given bool, e availability.Event) {
	out, v := s.env.Stderr, r.view
	if reply.Refused.Code != "stale" {
		fmt.Fprintln(out, wording.Sentence(reply.Refused.Message))
		return
	}
	base := baseOf(e)
	fmt.Fprintf(out, "Not applied: %s changed after revision %d, which the command was based on.\n", v.Name, base)
	lines := v.Changed(r.node.Availability, after)
	if given {
		fmt.Fprintf(out, "It's at revision %d now.\n", after.Revision)
		lines = v.Settings(after)
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	fmt.Fprintln(out, "Check it with `silkpool status`, then run the command again if you still want it.")
}

// baseOf is the revision a command was based on.
func baseOf(e availability.Event) int64 {
	switch e := e.(type) {
	case availability.Resume:
		return e.Base
	case availability.EditAutoResume:
		return e.Base
	case availability.SetSlotLimit:
		return e.Base
	case availability.SetBudget:
		return e.Base
	}
	return 0
}

// settings decodes a reply's settings.
func settings(r control.Reply) (availability.Node, error) {
	n, err := store.Reply{ID: r.ID, At: r.At, Settings: r.Settings}.ToReply()
	return n.Settings, err
}

// revisionFlag adds --revision, the revision a script read, to a command.
func revisionFlag(fs interface {
	Int64Var(*int64, string, int64, string)
}) *int64 {
	var rev int64
	fs.Int64Var(&rev, "revision", 0, "")
	return &rev
}

// base is the revision a command is based on: --revision if it's given,
// or the one the CLI just read.
func base(r read, given int64) (int64, bool) {
	if given > 0 {
		return given, true
	}
	return r.node.Availability.Revision, false
}

// runPause works like the tray's Pause button: it pauses until the next
// resume time while auto-resume is on, and indefinitely while it's off.
func runPause(ctx context.Context, s *session, args []string) int {
	fs := s.flags("pause", "silkpool pause [--indefinitely] [--json]")
	indefinitely := fs.Bool("indefinitely", false, "")
	r, code := s.begin(ctx, func() (int, bool) { _, code, ok := s.parse(fs, args, 0); return code, ok })
	if code != ExitOK {
		return code
	}
	var e availability.Event = availability.Pause{}
	if *indefinitely || !r.node.Availability.AutoResume.On {
		e = availability.PauseIndefinitely{}
	}
	reply, after, code := s.command(ctx, r, e, false)
	if code != ExitOK || s.json {
		return code
	}
	v := r.view
	if reply.Settings.Revision == r.node.Availability.Revision {
		fmt.Fprintf(s.env.Stdout, "%s was already %s\n", v.Name, lower(v.Paused(after)))
		return ExitOK
	}
	fmt.Fprintln(s.env.Stdout, v.Paused(after))
	if jobs := v.RunningJobs(); len(jobs) > 0 {
		fmt.Fprintln(s.env.Stdout, "Its running jobs finish first.")
	}
	return ExitOK
}

// lower makes a sentence's first letter lower case.
func lower(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]+'a'-'A') + s[1:]
}

// runResume puts the node On duty, based on the revision the CLI reads,
// or the one --revision gives.
func runResume(ctx context.Context, s *session, args []string) int {
	fs := s.flags("resume", "silkpool resume [--revision N] [--json]")
	rev := revisionFlag(fs)
	r, code := s.begin(ctx, func() (int, bool) { _, code, ok := s.parse(fs, args, 0); return code, ok })
	if code != ExitOK {
		return code
	}
	b, given := base(r, *rev)
	reply, _, code := s.command(ctx, r, availability.Resume{Base: b}, given)
	if code != ExitOK || s.json {
		return code
	}
	if reply.Settings.Revision == b {
		fmt.Fprintf(s.env.Stdout, "%s was already on duty.\n", r.view.Name)
		return ExitOK
	}
	fmt.Fprintf(s.env.Stdout, "Resumed: %s takes jobs again.\n", r.view.Name)
	return ExitOK
}

// runEmergencyStop cancels the running jobs and leaves the node Paused
// indefinitely, after the tray's confirmation unless --yes.
func runEmergencyStop(ctx context.Context, s *session, args []string) int {
	fs := s.flags("emergency-stop", "silkpool emergency-stop [--yes] [--json]")
	yes := fs.Bool("yes", false, "")
	r, code := s.begin(ctx, func() (int, bool) { _, code, ok := s.parse(fs, args, 0); return code, ok })
	if code != ExitOK {
		return code
	}
	v := r.view
	if !*yes {
		fmt.Fprintln(s.env.Stdout, v.StopConfirm())
		if ok, code := s.confirm("emergency-stop", s.red(v.StopQuestion())); !ok {
			return code
		}
	}
	reply, _, code := s.command(ctx, r, availability.EmergencyStop{}, false)
	if code != ExitOK || s.json {
		return code
	}
	after, code := s.readStatus(ctx)
	if code != ExitOK {
		return code
	}
	var cancelled []string
	if c, ok := after.node.IntentSet.What.(node.AppliedCommand); ok && c.ID == reply.ID {
		for _, j := range c.Cancelled {
			cancelled = append(cancelled, j.Job)
		}
	}
	fmt.Fprintln(s.env.Stdout, s.red(v.Stopped(cancelled)))
	return ExitOK
}

// runDismiss dismisses the notices about something that happened, as the
// tray's Dismiss does. Notices about a condition stay, each with what
// clears it.
func runDismiss(ctx context.Context, s *session, args []string) int {
	fs := s.flags("dismiss", "silkpool dismiss [--json]")
	r, code := s.begin(ctx, func() (int, bool) { _, code, ok := s.parse(fs, args, 0); return code, ok })
	if code != ExitOK {
		return code
	}
	ids := r.view.Dismissible()
	if len(ids) > 0 {
		id := s.env.NewID()
		reply, code := s.send(ctx, func(c *control.Client) (control.Reply, error) { return c.Dismiss(ctx, id, ids) })
		if code != ExitOK {
			return code
		}
		if s.json {
			s.printJSON(toJSON(reply))
		}
		if reply.Refused != nil {
			if !s.json {
				fmt.Fprintln(s.env.Stderr, wording.Sentence(reply.Refused.Message))
			}
			return ExitRefused
		}
	} else if s.json {
		s.printJSON(map[string]any{"applied": true, "dismissed": 0})
	}
	if s.json {
		return ExitOK
	}
	switch len(ids) {
	case 0:
		fmt.Fprintln(s.env.Stdout, "No notices to dismiss.")
	case 1:
		fmt.Fprintln(s.env.Stdout, "Dismissed 1 notice.")
	default:
		fmt.Fprintf(s.env.Stdout, "Dismissed %d notices.\n", len(ids))
	}
	for _, x := range r.view.Notices() {
		if x.Clears != "" {
			fmt.Fprintf(s.env.Stdout, "Still showing: %s %s\n", x.Text, x.Clears)
		}
	}
	return ExitOK
}

// runEnroll enrolls the machine in the Hub its code names, offering the
// host name, cleaned up, as the node's name.
func runEnroll(ctx context.Context, s *session, args []string) int {
	fs := s.flags("enroll", "silkpool enroll <code> [--name <name>] [--json]")
	name := fs.String("name", "", "")
	rest, code, ok := s.parse(fs, args, 1)
	if !ok {
		return code
	}
	text := rest[0]
	hubCode, err := wire.ParseCode(text)
	if err != nil {
		return s.usage(fs, wording.Sentence(err.Error()))
	}
	if *name != "" {
		if err := node.CheckName(*name); err != nil {
			return s.usage(fs, wording.Sentence(err.Error()))
		}
	}
	if code := s.connect(ctx, false); code != ExitOK {
		return code
	}
	if code := s.elevated("enroll changes this machine's accounts"); code != ExitOK {
		return code
	}
	r, code := s.readStatus(ctx)
	if code != ExitOK {
		return code
	}
	if r.node.Enrolled {
		if !s.json {
			fmt.Fprintf(s.env.Stderr, "This machine is already enrolled in %s. Remove it first with `sudo silkpool remove`.\n", r.status.Hub.URL)
		}
		return s.refusedJSON(control.ErrorOf(node.ErrEnrolled))
	}
	if *name == "" {
		if *name = node.NameFromHostname(r.status.Machine.Hostname); *name == "" {
			return s.usage(fs, "This machine's host name has no letters or digits to make a node name from. Give it one with --name.")
		}
	}
	if !s.json {
		fmt.Fprintf(s.env.Stdout, "Checking the Hub at %s…\n", hubCode.URL)
	}
	id := s.env.NewID()
	reply, code := s.send(ctx, func(c *control.Client) (control.Reply, error) { return c.Enroll(ctx, id, text, *name) })
	if code != ExitOK {
		return code
	}
	if s.json {
		s.printJSON(toJSON(reply))
	}
	if e := reply.Refused; e != nil {
		if !s.json {
			fmt.Fprintln(s.env.Stderr, wording.Sentence(e.Message))
			switch e.Code {
			case control.CodeNameTaken:
				fmt.Fprintf(s.env.Stderr, "Run it again with `--name %s`.\n", e.Suggestion)
			case control.CodeAnswerLost:
				fmt.Fprintln(s.env.Stderr, wording.EnrollAnswerLost)
			}
		}
		if e.Code == control.CodeUnreachable || e.Code == control.CodeAnswerLost {
			return ExitCantAsk
		}
		return ExitRefused
	}
	if !s.json {
		fmt.Fprintf(s.env.Stdout, "Enrolled %s in %s. It's paused until you resume it. Check auto-resume and the slot limit, then run `silkpool resume`.\n", *name, hubCode.URL)
	}
	return ExitOK
}

// refusedJSON prints a refusal as JSON, if --json, and returns
// ExitRefused.
func (s *session) refusedJSON(e *control.Error) int {
	if s.json {
		s.printJSON(map[string]any{"applied": false, "refused": e})
	}
	return ExitRefused
}

// runRemove removes the machine from its Hub, after the dashboard's
// confirmation unless --yes, and follows the removal until the machine
// has left the Hub. Ctrl+C only stops watching. --force removes it here
// without the Hub, which keeps its record.
func runRemove(ctx context.Context, s *session, args []string) int {
	fs := s.flags("remove", "silkpool remove [--force] [--yes] [--json]")
	force := fs.Bool("force", false, "")
	yes := fs.Bool("yes", false, "")
	if _, code, ok := s.parse(fs, args, 0); !ok {
		return code
	}
	if code := s.connect(ctx, false); code != ExitOK {
		return code
	}
	if code := s.elevated("remove deletes this machine's slot accounts"); code != ExitOK {
		return code
	}
	r, code := s.readStatus(ctx)
	if code != ExitOK {
		return code
	}
	if code := s.enrolled(r); code != ExitOK {
		return code
	}
	v := r.view
	if !*yes {
		question := v.RemoveQuestion()
		if *force {
			for _, l := range v.ForceRemoveConfirm() {
				fmt.Fprintln(s.env.Stdout, l)
			}
			question = v.ForceRemoveQuestion()
		} else {
			fmt.Fprintln(s.env.Stdout, v.RemoveConfirm())
		}
		if ok, code := s.confirm("remove", question); !ok {
			return code
		}
	}
	id := s.env.NewID()
	reply, code := s.send(ctx, func(c *control.Client) (control.Reply, error) { return c.Remove(ctx, id, *force) })
	if code != ExitOK {
		return code
	}
	if reply.Refused != nil {
		if s.json {
			s.printJSON(toJSON(reply))
		} else {
			fmt.Fprintln(s.env.Stderr, wording.Sentence(reply.Refused.Message))
		}
		if reply.Refused.Code == "hub-unreachable" {
			return ExitCantAsk
		}
		return ExitRefused
	}
	if !s.json {
		fmt.Fprintf(s.env.Stdout, "Removing %s. Ctrl+C stops watching; the removal carries on.\n", v.Name)
	}
	code = s.follow(ctx)
	if s.json {
		s.printJSON(toJSON(reply))
	}
	if code != ExitOK {
		return code
	}
	if !s.json {
		if *force {
			fmt.Fprintf(s.env.Stdout, "Removed %s here. Remove it in the Hub too.\n", v.Name)
		} else {
			fmt.Fprintf(s.env.Stdout, "%s has left the Hub. It's safe to uninstall Silkpool now.\n", v.Name)
		}
	}
	return ExitOK
}

// errLeft ends the watch once the machine has left its Hub.
var errLeft = errors.New("left the Hub")

// follow prints each new sentence of a removal until the machine isn't
// enrolled any more.
func (s *session) follow(ctx context.Context) int {
	last := ""
	err := s.client.Watch(ctx, func(st control.Status) error {
		r, err := decode(st)
		if err != nil {
			return err
		}
		if !r.node.Enrolled {
			return errLeft
		}
		if lead := r.view.Lead(); lead != last && !s.json {
			fmt.Fprintln(s.env.Stdout, lead)
			last = lead
		}
		return nil
	})
	switch {
	case errors.Is(err, errLeft):
		return ExitOK
	case err == nil && ctx.Err() != nil:
		fmt.Fprintln(s.env.Stderr, "Stopped watching. The removal carries on: `silkpool status` follows it.")
		return ExitNotConfirmed
	}
	fmt.Fprintf(s.env.Stderr, "Stopped watching: %v. The removal carries on: `silkpool status` follows it.\n", err)
	return ExitNotConfirmed
}

// runSetHubURL points the node at its Hub's new URL, once the agent has
// checked it's the same Hub.
func runSetHubURL(ctx context.Context, s *session, args []string) int {
	fs := s.flags("set-hub-url", "silkpool set-hub-url <url> [--json]")
	rest, code, ok := s.parse(fs, args, 1)
	if !ok {
		return code
	}
	if code := s.connect(ctx, false); code != ExitOK {
		return code
	}
	if code := s.elevated("set-hub-url changes the Hub this machine connects to"); code != ExitOK {
		return code
	}
	r, code := s.readStatus(ctx)
	if code != ExitOK {
		return code
	}
	if code := s.enrolled(r); code != ExitOK {
		return code
	}
	err := s.once(ctx, func(c *control.Client) error { return c.SetHubURL(ctx, rest[0]) })
	if errors.Is(err, errNotConfirmed) {
		return s.notConfirmed()
	}
	if err != nil {
		var e *control.Error
		if !errors.As(err, &e) {
			return s.failed(err)
		}
		code := ExitRefused
		if e.Code == control.CodeUnreachable {
			code = ExitCantAsk
		}
		if s.json {
			s.refusedJSON(e)
			return code
		}
		fmt.Fprintf(s.env.Stderr, "%s still connects to %s: %s\n", r.view.Name, r.status.Hub.URL, wording.Sentence(e.Message))
		return code
	}
	if s.json {
		s.printJSON(map[string]any{"applied": true, "url": rest[0]})
	} else {
		fmt.Fprintf(s.env.Stdout, "%s now connects to %s\n", r.view.Name, rest[0])
	}
	return ExitOK
}

// runVersion prints the CLI's version and the running service's.
func runVersion(ctx context.Context, s *session, args []string) int {
	fs := s.flags("version", "silkpool version [--json]")
	if _, code, ok := s.parse(fs, args, 0); !ok {
		return code
	}
	c, err := control.Dial(ctx, s.env.Socket, s.env.ServerUID)
	if err != nil {
		if !s.json {
			fmt.Fprintf(s.env.Stdout, "CLI      %s\n", s.env.Version)
		}
		return s.dialFailed(err)
	}
	s.client = c
	if s.json {
		s.printJSON(map[string]string{"cli": s.env.Version, "service": c.Hello.Version})
		return ExitOK
	}
	fmt.Fprintf(s.env.Stdout, "CLI      %s\nService  %s\n", s.env.Version, c.Hello.Version)
	return ExitOK
}

// runDiagnostics prints the diagnostics report, for an administrator to
// read or pass on. It holds no copied runner logs.
func runDiagnostics(ctx context.Context, s *session, args []string) int {
	fs := s.flags("diagnostics", "silkpool diagnostics [--json]")
	if _, code, ok := s.parse(fs, args, 0); !ok {
		return code
	}
	if code := s.connect(ctx, false); code != ExitOK {
		return code
	}
	rep, err := s.client.Diagnostics(ctx)
	if err != nil {
		return s.failed(err)
	}
	if s.json {
		s.printJSON(rep)
		return ExitOK
	}
	for _, p := range wording.Diagnostics(rep) {
		s.line(p.Label, p.Lines...)
	}
	fmt.Fprintf(s.env.Stdout, "\n%s\n", wording.LogHeading(len(rep.Log)))
	for _, l := range rep.Log {
		fmt.Fprintln(s.env.Stdout, "  "+l)
	}
	return ExitOK
}

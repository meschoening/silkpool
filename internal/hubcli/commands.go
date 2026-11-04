package hubcli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubservice"
	"github.com/meschoening/silkpool/internal/hubsocket"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/wording"
)

// line prints a labelled line of a block, and any more lines under it.
func (s *session) line(label string, lines ...string) {
	for i, l := range lines {
		if l == "" {
			continue
		}
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(s.env.Stdout, "%-12s %s\n", label, l)
	}
}

func runStatus(ctx context.Context, s *session, args []string) int {
	fs := s.flags("status", "silkpool-hub status [--json]")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	var st hubops.Status
	if code := s.ask(ctx, hubsocket.Request{Op: "status"}, &st); code != ExitOK || s.json {
		return code
	}
	accepts := make([]string, len(st.Accepts))
	for i, v := range st.Accepts {
		accepts[i] = fmt.Sprint(v)
	}
	s.line("Hub", fmt.Sprintf("%s · %s, listening on %s", st.URL, wording.Listener(st.Listener.Mode), st.Listener.Address))
	s.line("Version", fmt.Sprintf("%s · protocol %d · accepts agents at protocol %s", st.Version, st.Protocol, strings.Join(accepts, " or ")))
	operator := "Nobody has signed in yet."
	if st.Operator != 0 {
		operator = fmt.Sprintf("GitHub user %d", st.Operator)
	}
	s.line("Operator", operator)
	s.line("Setup", wording.Setup(st.Setup.Step, st.Setup.Done, st.Setup.Of, st.URL))
	var conns []string
	for _, c := range st.Connections {
		conns = append(conns, fmt.Sprintf("%s · %s · App %s · installation %d", c.Account, c.Kind, c.App, c.Installation))
	}
	if len(conns) == 0 {
		conns = []string{"None yet: setup connects the first."}
	}
	s.line("GitHub", conns...)
	nodes := fmt.Sprintf("%d, %d connected", st.Nodes, st.Connected)
	if st.Removed > 0 {
		nodes += fmt.Sprintf(" · %d removed", st.Removed)
	}
	s.line("Nodes", nodes)
	targets := fmt.Sprint(st.Targets)
	if st.DefaultTarget != "" {
		targets += " · new nodes get " + wording.Quote(st.DefaultTarget)
	}
	s.line("Targets", targets)
	s.line("Pool label", st.PoolLabel)
	return ExitOK
}

func runVersion(ctx context.Context, s *session, args []string) int {
	fs := s.flags("version", "silkpool-hub version [--json]")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	asJSON := s.json
	s.json = false
	var v hubops.Version
	code := s.ask(ctx, hubsocket.Request{Op: "version"}, &v)
	if asJSON {
		out := map[string]string{"cli": s.env.Version}
		if code == ExitOK {
			out["hub"] = v.Hub
		}
		s.printJSON(out)
		return code
	}
	s.line("CLI", s.env.Version)
	if code == ExitOK {
		s.line("Hub", v.Hub)
	}
	return code
}

func runCodeNew(ctx context.Context, s *session, args []string) int {
	fs := s.flags("code new", "silkpool-hub code new [--json]")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	var c struct {
		hubops.Code
		Text string `json:"text"`
	}
	if code := s.ask(ctx, hubsocket.Request{Op: "code.new"}, &c); code != ExitOK || s.json {
		return code
	}
	s.out("Enrollment code %s · single use · expires %s", c.ID, s.clock().When(c.Expires))
	s.out("")
	for _, cmd := range wording.EnrollCommands(c.Text) {
		s.out("On %s, %s:", cmd.OSes, cmd.Where)
		s.out("  %s", cmd.Command)
	}
	s.out("")
	s.out("%s", wording.EnrollNote)
	return ExitOK
}

func runCodeList(ctx context.Context, s *session, args []string) int {
	fs := s.flags("code list", "silkpool-hub code list [--json]")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	var l struct{ Codes []hubops.Code }
	if code := s.ask(ctx, hubsocket.Request{Op: "code.list"}, &l); code != ExitOK || s.json {
		return code
	}
	if len(l.Codes) == 0 {
		s.out("No codes. Make one with: silkpool-hub code new")
		return ExitOK
	}
	for _, c := range l.Codes {
		s.out("%s  %s · made %s", c.ID, s.clock().Code(wording.CodeState(c.State), c.Expires, c.Node), s.clock().When(c.Made))
	}
	return ExitOK
}

func runCodeRevoke(ctx context.Context, s *session, args []string) int {
	fs := s.flags("code revoke", "silkpool-hub code revoke <id> [--json]")
	rest, code, ok := s.parse(fs, args, 1, 1)
	if !ok {
		return code
	}
	var r hubops.CodeChange
	if code := s.ask(ctx, hubsocket.Request{Op: "code.revoke", ID: rest[0]}, &r); code != ExitOK || s.json {
		return code
	}
	switch {
	case r.Changed:
		s.out("Revoked code %s. It enrolls nothing now.", r.Code.ID)
	case r.Code.State == hubops.Expired:
		s.out("Code %s has expired already, so it enrolls nothing.", r.Code.ID)
	default:
		s.out("Code %s is revoked already.", r.Code.ID)
	}
	return ExitOK
}

// state is a node's state as node list shows it, and its connection.
func (s *session) state(n hubops.Node) (state, connection string) {
	state = n.State
	switch {
	case n.State == "":
		state = "—"
	case !n.Connected:
		state = "Last known: " + n.State
	}
	if n.Removing {
		state += " · Removing"
	}
	switch {
	case n.Connected:
		connection = "Connected"
	case n.Seen.IsZero():
		connection = "Never connected"
	default:
		connection = "Not connected since " + s.clock().When(n.Seen)
	}
	return state, connection
}

func runNodeList(ctx context.Context, s *session, args []string) int {
	fs := s.flags("node list", "silkpool-hub node list [--json]")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	var l struct{ Nodes []hubops.Node }
	if code := s.ask(ctx, hubsocket.Request{Op: "node.list"}, &l); code != ExitOK || s.json {
		return code
	}
	if len(l.Nodes) == 0 {
		s.out("No nodes yet. Enroll a machine to give the pool work, with a code from silkpool-hub code new. It joins paused, so nothing runs until you resume it.")
		return ExitOK
	}
	tw := tabwriter.NewWriter(s.env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Name\tOS\tState\tConnection\tTarget\tLabels")
	for _, n := range l.Nodes {
		state, connection := s.state(n)
		target := n.Target
		if target == "" {
			target = "No target"
		}
		labels := strings.Join(n.Labels, ", ")
		if labels == "" {
			labels = "—"
		}
		platform := n.Platform()
		if platform == "" {
			platform = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", n.Name, platform, state, connection, wording.Quote(target), labels)
	}
	tw.Flush()
	return ExitOK
}

func runNodeRename(ctx context.Context, s *session, args []string) int {
	fs := s.flags("node rename", "silkpool-hub node rename <name> <new-name> [--json]")
	rest, code, ok := s.parse(fs, args, 2, 2)
	if !ok {
		return code
	}
	var r hubops.NodeChange
	if code := s.ask(ctx, hubsocket.Request{Op: "node.rename", Name: rest[0], To: rest[1]}, &r); code != ExitOK || s.json {
		return code
	}
	if !r.Changed {
		s.out("The node is named %s already.", r.Node.Name)
		return ExitOK
	}
	s.out("Renamed %s to %s. Its new runners carry the new name.", rest[0], r.Node.Name)
	return ExitOK
}

// paused reports whether a node's state is a paused one's.
func paused(n hubops.Node) bool { return n.State == "Drained" || n.State == "Draining" }

func runNodeSetTarget(ctx context.Context, s *session, args []string) int {
	fs := s.flags("node set-target", "silkpool-hub node set-target <name> <target> [--json]")
	rest, code, ok := s.parse(fs, args, 2, 2)
	if !ok {
		return code
	}
	var r hubops.NodeChange
	if code := s.ask(ctx, hubsocket.Request{Op: "node.set-target", Name: rest[0], Target: rest[1]}, &r); code != ExitOK || s.json {
		return code
	}
	n := r.Node
	if !r.Changed {
		s.out("%s runs jobs for %s already.", n.Name, wording.Quote(n.Target))
		return ExitOK
	}
	s.out("%s runs jobs for %s now. Its idle runners are retired and minted again there, and busy ones finish their jobs first.", n.Name, wording.Quote(n.Target))
	if paused(n) {
		s.out("It's paused, so it takes jobs there once it's resumed.")
	}
	return ExitOK
}

func runLabels(verb string) command {
	return func(ctx context.Context, s *session, args []string) int {
		fs := s.flags("node label "+verb, "silkpool-hub node label "+verb+" <name> <label>... [--json]")
		rest, code, ok := s.parse(fs, args, 2, -1)
		if !ok {
			return code
		}
		var r hubops.NodeChange
		if code := s.ask(ctx, hubsocket.Request{Op: "node.label." + verb, Name: rest[0], Labels: rest[1:]}, &r); code != ExitOK || s.json {
			return code
		}
		n := r.Node
		labels, with := "no labels of its own", "without them"
		switch len(n.Labels) {
		case 0:
		case 1:
			labels, with = "the label "+n.Labels[0], "with it"
		default:
			labels, with = "the labels "+strings.Join(n.Labels, ", "), "with them"
		}
		if !r.Changed {
			s.out("%s has %s already.", n.Name, labels)
			return ExitOK
		}
		s.out("%s has %s now. Its idle runners are retired and minted again %s, and busy ones finish their jobs first.", n.Name, labels, with)
		for _, l := range n.Labels {
			if l == "self-hosted" {
				s.out("%s", wording.SelfHosted(n.Name))
			}
		}
		return ExitOK
	}
}

func runTargetList(ctx context.Context, s *session, args []string) int {
	fs := s.flags("target list", "silkpool-hub target list [--json]")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	var l hubops.TargetList
	if code := s.ask(ctx, hubsocket.Request{Op: "target.list"}, &l); code != ExitOK || s.json {
		return code
	}
	if len(l.Targets) == 0 {
		s.out("No targets yet. Add one with silkpool-hub target add, from what silkpool-hub target available lists.")
		return ExitOK
	}
	for i, t := range l.Targets {
		if i > 0 {
			s.out("")
		}
		head := wording.Quote(t.Target) + " · repository"
		if t.Kind == hubops.Organization {
			head = wording.Quote(t.Target) + " · runner group of " + t.Connection
		}
		if t.Default {
			head += " · default for new nodes"
		}
		s.out("%s", head)
		if !t.Lost.IsZero() {
			s.line("  Lost", fmt.Sprintf("The App can't see it on GitHub since %s, so its runners were stopped.", s.clock().When(t.Lost)))
		}
		switch {
		case t.Group != nil:
			s.line("  Serves", wording.Serves(t.Connection, t.Group.Visibility, t.Group.Repositories))
		case t.GroupFailed != "":
			s.line("  Serves", wording.Sentence(t.GroupFailed))
		}
		nodes := "None"
		if len(t.Nodes) > 0 {
			nodes = wording.Nodes(t.Nodes)
		}
		s.line("  Nodes", nodes)
		for _, r := range t.RunsOn {
			s.out("  %s", r)
		}
	}
	s.out("")
	s.line("Pool label", l.PoolLabel)
	return ExitOK
}

func runTargetAvailable(ctx context.Context, s *session, args []string) int {
	fs := s.flags("target available", "silkpool-hub target available [--json]")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	var a hubops.Available
	if code := s.ask(ctx, hubsocket.Request{Op: "target.available"}, &a); code != ExitOK || s.json {
		return code
	}
	if len(a.Candidates) == 0 && len(a.Failed) == 0 {
		s.out("The Hub has no GitHub connection yet: setup in the dashboard connects the first.")
		return ExitOK
	}
	tw := tabwriter.NewWriter(s.env.Stdout, 0, 0, 2, ' ', 0)
	last := ""
	for _, c := range a.Candidates {
		if c.Connection != last {
			if last != "" {
				fmt.Fprintln(tw)
			}
			what := "repositories in the App's installation"
			if c.Kind == hubops.Organization {
				what = "runner groups"
			}
			fmt.Fprintf(tw, "%s's %s\n", c.Connection, what)
			last = c.Connection
		}
		note := wording.CandidateRefused(c.Refused)
		switch {
		case note != "":
		case c.Added:
			note = "A target already."
		case c.Group != nil:
			note = "Serves " + wording.Serves(c.Connection, c.Group.Visibility, c.Group.Repositories) + "."
		default:
			note = "Private."
		}
		fmt.Fprintf(tw, "  %s\t%s\n", wording.Quote(c.Target), note)
	}
	tw.Flush()
	for _, f := range a.Failed {
		fmt.Fprintln(s.env.Stderr, wording.Sentence(f.Message))
	}
	if len(a.Failed) > 0 {
		return ExitCantAsk
	}
	return ExitOK
}

func runTarget(verb string) command {
	return func(ctx context.Context, s *session, args []string) int {
		fs := s.flags("target "+verb, "silkpool-hub target "+verb+" <target> [--json]")
		rest, code, ok := s.parse(fs, args, 1, 1)
		if !ok {
			return code
		}
		var r hubops.TargetChange
		if code := s.ask(ctx, hubsocket.Request{Op: "target." + verb, Target: rest[0]}, &r); code != ExitOK || s.json {
			return code
		}
		t := wording.Quote(r.Target.Target)
		switch {
		case verb == "add" && !r.Changed:
			s.out("%s is a target already.", t)
		case verb == "add":
			s.out("Added the target %s. Point a node at it with: silkpool-hub node set-target <name> %s", t, t)
			if r.Target.Default {
				s.out("It's the Hub's first, so new nodes get it.")
			}
		case verb == "remove":
			s.out("Removed the target %s.", t)
		case !r.Changed:
			s.out("New nodes get %s already.", t)
		default:
			s.out("New nodes get %s now. The nodes the Hub has keep theirs.", t)
		}
		return ExitOK
	}
}

func runPoolLabel(ctx context.Context, s *session, args []string) int {
	fs := s.flags("pool-label", "silkpool-hub pool-label [<label>] [--json]")
	rest, code, ok := s.parse(fs, args, 0, 1)
	if !ok {
		return code
	}
	var l hubops.PoolLabel
	req := hubsocket.Request{Op: "pool-label"}
	if len(rest) == 1 {
		req = hubsocket.Request{Op: "pool-label.set", Label: rest[0]}
	}
	if code := s.ask(ctx, req, &l); code != ExitOK || s.json {
		return code
	}
	runsOn := fmt.Sprintf("runs-on: [%s, <os>, <arch>]", l.PoolLabel)
	switch {
	case req.Op == "pool-label":
		s.out("The pool label is %s. Workflows choose the pool's runners with %s.", l.PoolLabel, runsOn)
	case !l.Changed:
		s.out("The pool label is %s already.", l.PoolLabel)
	default:
		s.out("The pool label is %s now. Every node's idle runners are retired and minted again with it, and workflows choose them with %s.", l.PoolLabel, runsOn)
	}
	return ExitOK
}

func runSetupCode(ctx context.Context, s *session, args []string) int {
	fs := s.flags("setup-code", "silkpool-hub setup-code [--reassign] [--json]")
	reassign := fs.Bool("reassign", false, "")
	if _, code, ok := s.parse(fs, args, 0, 0); !ok {
		return code
	}
	var l struct {
		hubops.SetupLink
		Link string `json:"link"`
	}
	if code := s.ask(ctx, hubsocket.Request{Op: "setup-code", Reassign: *reassign}, &l); code != ExitOK || s.json {
		return code
	}
	s.setupLink(l.Link, l.SetupLink)
	s.out("It replaces the last setup link, which no longer works.")
	return ExitOK
}

// setupLink prints a setup link and how long it works.
func (s *session) setupLink(link string, l hubops.SetupLink) {
	if l.Reassign {
		s.out("To make another GitHub user the Hub's operator, sign in as them at:")
	} else {
		s.out("Finish setting up the Hub in your browser, at:")
	}
	s.out("  %s", link)
	s.out("%s", s.clock().SetupValid(l.Expires, l.Reassign))
}

func runSetURL(ctx context.Context, s *session, args []string) int {
	fs := s.flags("set-url", "silkpool-hub set-url <url> [--json]")
	rest, code, ok := s.parse(fs, args, 1, 1)
	if !ok {
		return code
	}
	var r hubops.URLChange
	if code := s.ask(ctx, hubsocket.Request{Op: "set-url", URL: rest[0]}, &r); code != ExitOK {
		return code
	}
	if !r.Changed {
		if !s.json {
			s.out("The Hub's URL is %s already.", r.URL)
		}
		return ExitOK
	}
	if !s.json {
		s.printURLChange(r)
	}
	if err := s.env.Systemctl(ctx, "restart", hubservice.Unit); err != nil {
		return s.say("not-restarted", fmt.Sprintf("Couldn't restart the Hub (%v), which it needs to serve its new URL. Restart it with: sudo systemctl restart silkpool-hub", err), ExitCantAsk)
	}
	if !s.json {
		s.out("")
		s.out("Restarted the Hub.")
	}
	return ExitOK
}

// printURLChange prints what to change once the Hub's URL has: each
// App's settings, then each node's Hub URL.
func (s *session) printURLChange(r hubops.URLChange) {
	s.out("The Hub's URL is %s now, in place of %s.", r.URL, r.From)
	if len(r.Apps) > 0 {
		s.out("")
		s.out("Change each GitHub App's settings to match, on GitHub:")
		for _, a := range r.Apps {
			which := a.Connection + "'s App"
			if a.SignIn {
				which += ", the one you sign in through"
			}
			s.out("  %s: %s", which, a.Page)
			s.out("    Homepage URL  %s", a.Homepage)
			s.out("    Callback URL  %s", a.Callback)
			s.out("    Setup URL     %s", a.Setup)
		}
		s.out("%s", wording.SignInFails)
	}
	s.out("")
	if len(r.Nodes) == 0 {
		s.out("No node to point at the new URL.")
		return
	}
	s.out("Then point each node at the new URL:")
	groups := map[string][]string{}
	var never, unknown []string
	for _, n := range r.Nodes {
		switch {
		case n.UnknownPlatform:
			unknown = append(unknown, n.Name)
		case n.OS == "":
			never = append(never, n.Name)
		case n.OS == "windows":
			groups["windows"] = append(groups["windows"], n.Name)
		default:
			groups["unix"] = append(groups["unix"], n.Name)
		}
	}
	for _, os := range []string{"unix", "windows"} {
		if len(groups[os]) == 0 {
			continue
		}
		cmd := wording.SetHubURLCommand(os, r.URL)
		s.out("  On %s, %s:", wording.Nodes(groups[os]), cmd.Where)
		s.out("    %s", cmd.Command)
	}
	if len(never) > 0 {
		s.out("  %s never connected: run the command for its OS there.", wording.Nodes(never))
	}
	if len(unknown) > 0 {
		s.out("  %s", wording.SetHubURLUnknown(unknown))
	}
}

// listenerWords says how the Hub listens, as init sets it up.
func listenerWords(l hubops.Listener, cert, key string) string {
	switch l.Mode {
	case string(hubstore.CertificateFiles):
		return fmt.Sprintf("listening on %s with the certificate files %s and %s. The Hub restarts whenever they change, to serve the new ones.", l.Address, cert, key)
	case string(hubstore.BehindProxy):
		return fmt.Sprintf("listening on %s over plain HTTP, for the proxy in front of it.", l.Address)
	}
	return fmt.Sprintf("listening on %s with a self-signed certificate.", l.Address)
}

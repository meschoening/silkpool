package hubops

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
)

// Commands from the Hub to a node, as "Prototype the Hub dashboard"
// has them: the dashboard never runs ahead of the node. A command is
// sent only to a connected node, under an ID of its own, and is then
// sending until the node's reply. The reply's content reaches the Hub in
// the node's report, which follows every reply, since the node keeps
// its replies for node.KeepReplies. If the connection drops first, the
// command is not confirmed, and the report after the node reconnects
// settles it: confirmed if it holds the reply, and otherwise not
// applied.
//
// The Ops keep the last command each node was sent, in memory: a
// restarted Hub has none. Reading a command not confirmed marks it, so
// the report after reconnecting confirms it: "Confirmed" answers a "Not
// confirmed" a page showed, and every page reads it through Fleet. A
// reply that was lost and found again before anything read it reads as
// applied, which it was.

// A CommandKind is what a command asks of a node.
type CommandKind string

// The kinds of command.
const (
	PauseCommand             CommandKind = "pause"
	PauseIndefinitelyCommand CommandKind = "pause-indefinitely"
	ResumeCommand            CommandKind = "resume"
	StopCommand              CommandKind = "stop"
	SlotLimitCommand         CommandKind = "slot-limit"
	AutoResumeCommand        CommandKind = "auto-resume"
	BudgetCommand            CommandKind = "budget"
	DismissCommand           CommandKind = "dismiss"
)

// A CommandState is where a command is.
type CommandState string

// The states of a command.
const (
	// CommandSending is sent, waiting for the node's reply.
	CommandSending CommandState = "sending"
	// CommandNotConfirmed lost its connection before the reply, so it
	// settles once the node reconnects and reports.
	CommandNotConfirmed CommandState = "not-confirmed"
	// CommandApplied is applied, as the node replied.
	CommandApplied CommandState = "applied"
	// CommandRefused is refused by the node, which says why.
	CommandRefused CommandState = "refused"
	// CommandConfirmed is applied, as the node's report after it
	// reconnected shows.
	CommandConfirmed CommandState = "confirmed"
	// CommandNotApplied never reached the node, as its report after it
	// reconnected shows.
	CommandNotApplied CommandState = "not-applied"
)

// A Command is the last command the Hub sent a node, and where it is.
type Command struct {
	ID    string
	Kind  CommandKind
	Sent  time.Time
	State CommandState
	// Reply is the node's reply, once its report holds it.
	Reply node.Reply
	// Settled is when the command settled, as far as the Hub knows: the
	// reply's time once it's applied or refused, and the connection the
	// report came on once the report after reconnecting settles it.
	Settled time.Time
}

// sentCommand is what the Ops keep of the last command a node was sent:
// the connection it went out on, whether it was ever read not
// confirmed, and, once the report after reconnecting has settled it,
// how and when, which later connections don't change.
type sentCommand struct {
	Command
	connection  time.Time
	unconfirmed bool
	outcome     CommandState
	settled     time.Time
}

// Command sends the node named name the availability command e, from
// from, under a new ID, and returns it, sending. A node that isn't
// connected isn't sent it: "not-connected", and nor is a Mac its budget:
// "no-budget". Fleet follows it.
func (o *Ops) Command(ctx context.Context, from node.Origin, name string, e availability.Event) (Command, error) {
	kind, ok := kindOf(e)
	if !ok {
		return Command{}, fmt.Errorf("hubops: %T isn't a command", e)
	}
	return o.send(ctx, name, kind, func(id string) node.Event { return node.Command{ID: id, Origin: from, Event: e} })
}

// Dismiss sends the node named name the dismissal of its notice with ID
// notice, from from, as Command does.
func (o *Ops) Dismiss(ctx context.Context, from node.Origin, name string, notice int64) (Command, error) {
	return o.send(ctx, name, DismissCommand, func(id string) node.Event { return node.Dismiss{ID: id, Origin: from, Notice: notice} })
}

// send issues the message m makes, under a new ID, to the node named
// name, and keeps it as the node's last command.
func (o *Ops) send(ctx context.Context, name string, kind CommandKind, m func(id string) node.Event) (Command, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return Command{}, err
	}
	n, err := findNode(h, strings.TrimSpace(name))
	if err != nil {
		return Command{}, err
	}
	id := rand.Text()
	effects, err := o.c.Hub.Apply(ctx, pool.Issued{Node: n.ID, Command: m(id)})
	if err != nil {
		return Command{}, err
	}
	for _, eff := range effects {
		switch eff := eff.(type) {
		case pool.NotSent:
			return Command{}, refuse("not-connected", "not connected: not sent")
		case pool.ChangeRefused:
			return Command{}, refusalOf(eff.Reason)
		}
	}
	s := &sentCommand{Command: Command{ID: id, Kind: kind, Sent: o.c.Now().UTC()}, connection: n.ConnectedAt}
	o.mu.Lock()
	if o.commands == nil {
		o.commands = map[string]*sentCommand{}
	}
	o.commands[n.ID] = s
	o.mu.Unlock()
	if h, err = o.state(ctx); err != nil {
		return Command{}, err
	}
	n, _ = byID(h, n.ID)
	return o.commandOf(n), nil
}

// kindOf returns the kind of the availability command e.
func kindOf(e availability.Event) (CommandKind, bool) {
	switch e.(type) {
	case availability.Pause:
		return PauseCommand, true
	case availability.PauseIndefinitely:
		return PauseIndefinitelyCommand, true
	case availability.Resume:
		return ResumeCommand, true
	case availability.EmergencyStop:
		return StopCommand, true
	case availability.SetSlotLimit:
		return SlotLimitCommand, true
	case availability.EditAutoResume, availability.TurnOffAutoResume:
		return AutoResumeCommand, true
	case availability.SetBudget:
		return BudgetCommand, true
	}
	return "", false
}

// commandOf returns where n's last command is, or the zero Command if
// the Ops sent it none, or none within node.KeepReplies, after which the
// node no longer holds its reply.
func (o *Ops) commandOf(n pool.Node) Command {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.commands[n.ID]
	if !ok {
		return Command{}
	}
	if !o.c.Now().Before(s.Sent.Add(node.KeepReplies)) {
		delete(o.commands, n.ID)
		return Command{}
	}
	c := s.Command
	c.Settled = time.Time{}
	reconnected := n.Connected && !n.ConnectedAt.Equal(s.connection)
	i := slices.IndexFunc(n.Mirror.Replies, func(r node.Reply) bool { return r.ID == c.ID })
	if i >= 0 {
		c.Reply = n.Mirror.Replies[i]
	}
	// A settled command stays so, unless a report holds the reply to one
	// that seemed not applied: its report had been lost with the
	// connection, after the reply.
	if s.outcome == CommandConfirmed || s.outcome == CommandNotApplied && i < 0 {
		c.State, c.Settled = s.outcome, s.settled
		return c
	}
	if i >= 0 {
		switch {
		case c.Reply.Refused != nil:
			c.State, c.Settled = CommandRefused, c.Reply.At
		case reconnected && s.unconfirmed:
			c.State, c.Settled = CommandConfirmed, s.settle(n, CommandConfirmed)
		default:
			c.State, c.Settled = CommandApplied, c.Reply.At
		}
		return c
	}
	switch {
	case slices.Contains(n.Sent, c.ID):
		c.State = CommandSending
	case slices.Contains(n.NotConfirmed, c.ID), !n.Connected:
		// A node that isn't connected may have replied without the
		// report that holds the reply reaching the Hub.
		c.State = CommandNotConfirmed
		s.unconfirmed = true
	case reconnected:
		c.State, c.Settled = CommandNotApplied, s.settle(n, CommandNotApplied)
	default:
		// The reply came, and the report that holds it is on its way.
		c.State = CommandSending
	}
	return c
}

// settle keeps outcome as how the report after n reconnected settled
// s, as of the connection it came on, and returns when that was.
func (s *sentCommand) settle(n pool.Node, outcome CommandState) time.Time {
	s.outcome, s.settled = outcome, n.ConnectedAt.UTC()
	return s.settled
}

// A Detail is a node as the dashboard shows it: what node list shows,
// and the node as it last reported itself, the Hub's own notices about
// it, its runs-on line, and its last command.
type Detail struct {
	Node
	Mirror node.Node
	// ConnectedAt is when the Hub accepted the connection the node holds,
	// or zero.
	ConnectedAt time.Time
	Notices     []pool.Notice
	// RunsOn is the runs-on line that chooses the node's runners: the pool
	// label, its OS and architecture, and its custom labels, or empty
	// until it has connected.
	RunsOn string
	// Command is the last command the Hub sent it through the Ops, or nil.
	Command *Command
}

// A Fleet is the pool as the dashboard shows it, from one reading of
// the Hub's state: its nodes, but tombstones, in the order they
// enrolled, the targets a node can have, in the order they were added,
// the pool label and the Hub's version.
type Fleet struct {
	Nodes     []Detail
	Targets   []string
	PoolLabel string
	Version   string
}

// Find returns the node named name, ignoring case.
func (f Fleet) Find(name string) (Detail, bool) {
	i := slices.IndexFunc(f.Nodes, func(d Detail) bool { return strings.EqualFold(d.Name, strings.TrimSpace(name)) })
	if i < 0 {
		return Detail{}, false
	}
	return f.Nodes[i], true
}

// Fleet returns the pool as the dashboard shows it.
func (o *Ops) Fleet(ctx context.Context) (Fleet, error) {
	h, err := o.state(ctx)
	if err != nil {
		return Fleet{}, err
	}
	f := Fleet{PoolLabel: h.Settings.PoolLabel, Version: o.c.Version}
	for _, t := range h.Targets {
		f.Targets = append(f.Targets, t.Key)
	}
	notices := h.Pool.Notices()
	for _, n := range h.Pool.Nodes {
		if !n.Removed.IsZero() {
			continue
		}
		d := Detail{Node: nodeOf(h, n), Mirror: n.Mirror, ConnectedAt: n.ConnectedAt, RunsOn: runsOn(h, n)}
		for _, x := range notices {
			if x.Node == n.ID {
				d.Notices = append(d.Notices, x)
			}
		}
		if c := o.commandOf(n); c.ID != "" {
			d.Command = &c
		}
		f.Nodes = append(f.Nodes, d)
	}
	return f, nil
}

// runsOn returns n's runs-on line, or empty until it has connected.
func runsOn(h hubstore.Hub, n pool.Node) string {
	osName, arch := osArch(n.Platform)
	if osName == "" {
		return ""
	}
	return fmt.Sprintf("runs-on: [%s]", strings.Join(slices.Concat([]string{h.Settings.PoolLabel, osName, arch}, h.Labels[n.ID]), ", "))
}

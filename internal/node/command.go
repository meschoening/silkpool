package node

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
)

// KeepReplies is how long the node keeps a command's reply, so that a
// duplicate of the command gets it again.
const KeepReplies = 24 * time.Hour

// An Origin is where a command or a change started, for history: its
// surface and who was behind it. Where it came from is the surface's
// machine: the node for the tray and the node's CLI, and the Hub's host
// for the dashboard and the Hub's CLI. Commands carry no address.
type Origin struct {
	Surface Surface
	// Account is the OS account the peer check saw, for the tray, the
	// node's CLI and the Hub's CLI.
	Account string
	// Login is the signed-in GitHub login, for the dashboard. There's one
	// operator today, but keeping the login keeps history honest if that
	// changes.
	Login string
}

// A Surface is where a command or a change can start. AutoResume and
// Hub aren't surfaces anyone uses: they stand in for one when the node
// or the Hub acts on its own.
type Surface int

const (
	// Dashboard is the Hub's dashboard.
	Dashboard Surface = iota + 1
	// CommandLine is the node's own CLI.
	CommandLine
	// Tray is the node's tray, or its menu bar on macOS.
	Tray
	// HubCommandLine is the Hub's CLI.
	HubCommandLine
	// AutoResume is the node's auto-resume, ending a Paused intent at its
	// resume time.
	AutoResume
	// Hub is the Hub acting on its own, as when it finds a target lost.
	Hub
)

func (s Surface) String() string {
	switch s {
	case Dashboard:
		return "dashboard"
	case CommandLine:
		return "node CLI"
	case Tray:
		return "tray"
	case HubCommandLine:
		return "Hub CLI"
	case AutoResume:
		return "auto-resume"
	case Hub:
		return "Hub"
	}
	return fmt.Sprintf("Surface(%d)", int(s))
}

// The rules, beyond package availability's, under which a command is
// refused.
var (
	// ErrNoID refuses a command without a command ID.
	ErrNoID = errors.New("the command has no ID")
	// ErrNoTarget refuses Resume for a node with no target.
	ErrNoTarget = errors.New("the node has no target: choose a target first")
	// ErrRemoving refuses every command but Emergency stop while the
	// node is being removed.
	ErrRemoving = errors.New("the node is being removed")
	// ErrHubUnreachable refuses the node's own remove, without --force,
	// while it isn't connected to its Hub.
	ErrHubUnreachable = errors.New("the Hub can't be reached: remove the node in the Hub, or use --force")
	// ErrNotEnrolled refuses every command once the node is removed.
	ErrNotEnrolled = errors.New("the machine isn't enrolled")
	// ErrEnrolled refuses enroll while the machine is enrolled.
	ErrEnrolled = errors.New("the machine is already enrolled: remove it first")
)

// replying is the reply to the command under way.
type replying struct {
	Reply
	// origin and event are the command's, once availability applies it,
	// for the outbox, and cancelled names the jobs an Emergency stop
	// cancelled.
	origin    Origin
	event     availability.Event
	cancelled []CancelledJob
}

// commanded starts the command e, if it is one, and returns the
// availability event to apply for it, or a Tick. A duplicate gets its
// original reply again and applies nothing, as does a command refused
// before availability sees it, or one that waits on the Hub.
func (a *applying) commanded(e Event) availability.Event {
	var id string
	switch e := e.(type) {
	case Command:
		id = e.ID
	case Remove:
		id = e.ID
	case Dismiss:
		id = e.ID
	case Enroll:
		id = e.ID
	case Removed:
		// Not a command, but the node removes itself all the same.
		if !a.n.Removal.Started {
			a.startRemoval()
			return availability.PauseIndefinitely{}
		}
		return availability.Tick{}
	default:
		return availability.Tick{}
	}
	n := a.n
	if i := slices.IndexFunc(n.Replies, func(r Reply) bool { return r.ID == id }); id != "" && i >= 0 {
		a.emit(n.Replies[i])
		return availability.Tick{}
	}
	a.replying = &replying{Reply: Reply{ID: id, At: a.now}}
	switch e := e.(type) {
	case Command:
		if id != "" {
			return a.command(e)
		}
	case Remove:
		if id != "" {
			return a.remove(e)
		}
	case Dismiss:
		if id != "" {
			a.dismiss(e)
			return availability.Tick{}
		}
	case Enroll:
		if id != "" {
			a.replying.Refused = ErrEnrolled
			return availability.Tick{}
		}
	}
	a.replying.Refused = ErrNoID
	return availability.Tick{}
}

// command checks the node's own rules for e, then hands its event to
// availability.
func (a *applying) command(e Command) availability.Event {
	_, stop := e.Event.(availability.EmergencyStop)
	_, resume := e.Event.(availability.Resume)
	budget, setsBudget := e.Event.(availability.SetBudget)
	switch {
	case a.n.Removal.Started && !stop:
		a.replying.Refused = ErrRemoving
	case resume && a.n.Target == "":
		a.replying.Refused = ErrNoTarget
	case setsBudget && a.n.Machine.check(budget) != nil:
		a.replying.Refused = a.n.Machine.check(budget)
	default:
		a.replying.origin, a.replying.event = e.Origin, e.Event
		return e.Event
	}
	return availability.Tick{}
}

// reply completes the reply to the command under way, if any, keeps it
// for duplicates, and sends it. A command that applied goes into the
// outbox with its origin, whether or not the node is connected (ADR
// 0012).
func (a *applying) reply() {
	r := a.replying
	if r == nil {
		return
	}
	r.Settings = a.n.Availability
	if r.ID != "" {
		a.n.Replies = append(a.n.Replies, r.Reply)
	}
	a.emit(r.Reply)
	if r.Refused == nil && r.event != nil {
		c := AppliedCommand{ID: r.ID, Origin: r.origin, Event: r.event, Cancelled: r.cancelled}
		if _, ok := r.event.(availability.SetBudget); ok {
			c.Budget = a.n.Availability.Budget
		}
		a.happened(c)
		_, stop := r.event.(availability.EmergencyStop)
		now := a.n.Availability
		if stop || now.Intent != a.prior.Intent || !now.ResumeAt.Equal(a.prior.ResumeAt) {
			a.n.IntentSet = a.n.Outbox[len(a.n.Outbox)-1]
		}
	}
}

// forget drops the replies kept for KeepReplies.
func (a *applying) forget() {
	a.n.Replies = slices.DeleteFunc(slices.Clone(a.n.Replies), func(r Reply) bool {
		return !a.now.Before(r.At.Add(KeepReplies))
	})
}

// notEnrolled refuses e if it's a command other than Enroll.
func (a *applying) notEnrolled(e Event) {
	switch e := e.(type) {
	case Command:
		a.emit(Reply{ID: e.ID, At: a.now, Refused: ErrNotEnrolled})
	case Remove:
		a.emit(Reply{ID: e.ID, At: a.now, Refused: ErrNotEnrolled})
	case Dismiss:
		a.emit(Reply{ID: e.ID, At: a.now, Refused: ErrNotEnrolled})
	}
}

// Package availability is the pure core of a node's availability: its
// intent, its auto-resume settings, its slot limit and budget, and the
// revision that guards them. The decisions behind it are in "Define availability
// intents, schedule rules and override priority"
// and, for the slot limit, "Define node states and the drain/retirement
// contract".
//
// Everything here is a function of (node, event, now). Nothing reads
// the clock, sleeps or does I/O, so the agent decides when now is.
package availability

import (
	"errors"
	"fmt"
	"time"

	// Windows has no IANA database of its own, and every program that
	// computes resume times needs one.
	_ "time/tzdata"
)

// An Intent is the availability the operator has asked of a node. The
// zero Intent is Paused indefinitely, the one that takes no jobs.
type Intent int

const (
	// PausedIndefinitely takes no new jobs until someone resumes the node.
	PausedIndefinitely Intent = iota
	// Paused takes no new jobs until the next resume time.
	Paused
	// OnDuty takes jobs.
	OnDuty
)

func (i Intent) String() string {
	switch i {
	case PausedIndefinitely:
		return "Paused indefinitely"
	case Paused:
		return "Paused"
	case OnDuty:
		return "On duty"
	}
	return fmt.Sprintf("Intent(%d)", int(i))
}

// A TimeOfDay is a 24-hour wall-clock time, to the minute.
type TimeOfDay struct {
	Hour, Minute int
}

func (t TimeOfDay) String() string {
	return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute)
}

// Weekdays is a set of days of the week.
type Weekdays uint8

// EveryDay holds all seven days.
const EveryDay Weekdays = 1<<7 - 1

// DaysOf returns the set holding days.
func DaysOf(days ...time.Weekday) Weekdays {
	var w Weekdays
	for _, d := range days {
		w |= 1 << d
	}
	return w
}

// Has reports whether w holds d.
func (w Weekdays) Has(d time.Weekday) bool {
	return w&(1<<d) != 0
}

// AutoResume is a node's auto-resume settings. They change together, as
// one edit, except that turning auto-resume off leaves the rest alone.
type AutoResume struct {
	On   bool
	Time TimeOfDay
	Days Weekdays
	// Zone is the IANA time zone resume times are computed in. It is
	// stored explicitly and never follows the OS's zone, so time.Local
	// is refused. The caller loads it by name, with time.LoadLocation:
	// the core doesn't, since that reads zone files.
	Zone *time.Location
}

// MaxSlotLimit is the highest slot limit a node accepts. It's a sanity
// bound, not a capacity rule: those belong to node resource budgets.
const MaxSlotLimit = 64

// A Node is one node's availability. The agent owns it; the Hub's copy is
// a mirror.
type Node struct {
	Intent     Intent
	AutoResume AutoResume
	// SlotLimit is the number of slots: at least 1, and at most
	// MaxSlotLimit. It only matters On duty, so an edit while paused
	// takes effect on Resume.
	SlotLimit int
	// Budget is the most memory and CPU the slots may use together, as
	// the operator set it, or none.
	Budget Budget
	// Revision rises by one with every change.
	Revision int64
	// ResumeAt is when a Paused intent ends, and zero for the other two.
	ResumeAt time.Time
}

// New returns a newly enrolled node: Paused indefinitely, with
// auto-resume off at 08:00 every day in zone, the node's OS zone at
// enrollment, and one slot.
func New(zone *time.Location) (Node, error) {
	if err := checkZone(zone); err != nil {
		return Node{}, err
	}
	return Node{
		Intent: PausedIndefinitely,
		AutoResume: AutoResume{
			Time: TimeOfDay{Hour: 8},
			Days: EveryDay,
			Zone: zone,
		},
		SlotLimit: 1,
		Revision:  1,
	}, nil
}

// An Event is something that happens to a node: a command, an
// auto-resume edit, a slot-limit or budget change, or the clock moving
// on.
type Event interface{ event() }

// Pause sets Paused, ending at the next resume time. It always applies,
// except that it's refused while auto-resume is off.
type Pause struct{}

// PauseIndefinitely sets Paused indefinitely. It always applies.
type PauseIndefinitely struct{}

// Resume sets On duty. It's refused unless Base is the node's current
// revision.
type Resume struct{ Base int64 }

// EmergencyStop cancels the node's running jobs and leaves it Paused
// indefinitely. It always applies, and always counts as a change.
type EmergencyStop struct{}

// EditAutoResume replaces every auto-resume setting with To. It's
// refused unless Base is the node's current revision and To is valid.
type EditAutoResume struct {
	Base int64
	To   AutoResume
}

// TurnOffAutoResume is the off-only edit: it turns auto-resume off and
// leaves the time, days and zone alone. It always applies.
type TurnOffAutoResume struct{}

// SetSlotLimit sets the slot limit to To. Lowering it always applies;
// raising it is refused unless Base is the node's current revision, since
// only an increase in availability needs an up-to-date view.
type SetSlotLimit struct {
	Base int64
	To   int
}

// Tick tells the node that the clock reads now. The agent applies one on
// start, before any command, and then periodically.
type Tick struct{}

func (Pause) event()             {}
func (PauseIndefinitely) event() {}
func (Resume) event()            {}
func (EmergencyStop) event()     {}
func (EditAutoResume) event()    {}
func (TurnOffAutoResume) event() {}
func (SetSlotLimit) event()      {}
func (Tick) event()              {}

// An Effect is something Apply asks of the agent, beyond keeping the node
// it returns.
type Effect interface{ effect() }

// Refused reports that the event was refused and changed nothing.
type Refused struct{ Reason error }

// CancelJobs asks the agent to cancel every running job.
type CancelJobs struct{}

// AutoResumed reports that a resume time ended a Paused intent. At is
// the resume time, which is earlier than now if the agent wasn't running
// when it passed.
type AutoResumed struct{ At time.Time }

func (Refused) effect()     {}
func (CancelJobs) effect()  {}
func (AutoResumed) effect() {}

// The reasons Apply refuses an event.
var (
	// ErrStale refuses a Resume, an auto-resume edit, a slot-limit raise
	// or a budget's loosening based on a revision that isn't the current
	// one. The sender re-reads the node and tries again.
	ErrStale = errors.New("based on an out-of-date revision")
	// ErrAutoResumeOff refuses Pause while auto-resume is off.
	ErrAutoResumeOff = errors.New("auto-resume is off")
	// ErrInvalid refuses auto-resume settings, a slot limit or a budget
	// that can't be used.
	ErrInvalid = errors.New("invalid settings")
)

// Apply applies e to n at now. It returns the node that results and the
// effects the agent must carry out, in order. A resume time that has
// passed by now ends a Paused intent before e applies, so a resume time
// missed while the agent wasn't running applies once, before any command.
func (n Node) Apply(e Event, now time.Time) (Node, []Effect) {
	var effects []Effect
	if n.Intent == Paused && !n.ResumeAt.IsZero() && !now.Before(n.ResumeAt) {
		effects = append(effects, AutoResumed{At: n.ResumeAt})
		n.Intent, n.ResumeAt = OnDuty, time.Time{}
		n.Revision++
	}
	next, err := n.apply(e, now)
	if err != nil {
		return n, append(effects, Refused{Reason: err})
	}
	if _, stop := e.(EmergencyStop); stop {
		effects = append(effects, CancelJobs{})
	} else if next.same(n) {
		return n, effects
	}
	next.Revision = n.Revision + 1
	return next, effects
}

// apply returns n with e applied, leaving the revision to Apply.
func (n Node) apply(e Event, now time.Time) (Node, error) {
	switch e := e.(type) {
	case Pause:
		if !n.AutoResume.On {
			return n, ErrAutoResumeOff
		}
		n.Intent, n.ResumeAt = Paused, n.AutoResume.Next(now)
	case PauseIndefinitely, EmergencyStop:
		n.Intent, n.ResumeAt = PausedIndefinitely, time.Time{}
	case Resume:
		if e.Base != n.Revision {
			return n, ErrStale
		}
		n.Intent, n.ResumeAt = OnDuty, time.Time{}
	case EditAutoResume:
		if err := e.To.check(); err != nil {
			return n, err
		}
		if e.Base != n.Revision {
			return n, ErrStale
		}
		n.AutoResume = e.To
		n = n.repause(now)
	case TurnOffAutoResume:
		n.AutoResume.On = false
		n = n.repause(now)
	case SetSlotLimit:
		if e.To < 1 || e.To > MaxSlotLimit {
			return n, fmt.Errorf("%w: the slot limit must be from 1 to %d", ErrInvalid, MaxSlotLimit)
		}
		if e.To > n.SlotLimit && e.Base != n.Revision {
			return n, ErrStale
		}
		n.SlotLimit = e.To
	case SetBudget:
		b, err := n.Budget.apply(e, n.Revision)
		if err != nil {
			return n, err
		}
		n.Budget = b
	}
	return n, nil
}

// repause recomputes a Paused intent after an auto-resume edit at now:
// its resume time moves to the first one after now, or, if auto-resume
// is now off, it becomes Paused indefinitely.
func (n Node) repause(now time.Time) Node {
	switch {
	case n.Intent != Paused:
	case n.AutoResume.On:
		n.ResumeAt = n.AutoResume.Next(now)
	default:
		n.Intent, n.ResumeAt = PausedIndefinitely, time.Time{}
	}
	return n
}

// same reports whether n and m differ in nothing but their revisions.
func (n Node) same(m Node) bool {
	a, b := n.AutoResume, m.AutoResume
	return n.Intent == m.Intent && n.ResumeAt.Equal(m.ResumeAt) && n.SlotLimit == m.SlotLimit && n.Budget == m.Budget &&
		a.On == b.On && a.Time == b.Time && a.Days == b.Days && a.Zone.String() == b.Zone.String()
}

// check reports whether a can be used.
func (a AutoResume) check() error {
	if a.Time.Hour < 0 || a.Time.Hour > 23 || a.Time.Minute < 0 || a.Time.Minute > 59 {
		return fmt.Errorf("%w: %v isn't a time of day", ErrInvalid, a.Time)
	}
	if a.Days&EveryDay == 0 || a.Days&^EveryDay != 0 {
		return fmt.Errorf("%w: the days must be a non-empty set of weekdays", ErrInvalid)
	}
	return checkZone(a.Zone)
}

func checkZone(zone *time.Location) error {
	if zone == nil || zone.String() == "" || zone.String() == "Local" {
		return fmt.Errorf("%w: the zone must be an IANA name", ErrInvalid)
	}
	return nil
}

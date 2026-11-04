package node

import (
	"fmt"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
)

// An Entry is one event in the node's outbox, kept until the Hub has
// stored it. Entries are delivered at least once, so the Hub drops any
// whose Seq it has already stored.
type Entry struct {
	// Seq rises by one with every entry, over the node's life.
	Seq  int64
	At   time.Time
	What Happening
}

// A Happening is what an Entry records: an AppliedCommand, a Job, a
// DegradedBegan or a DegradedEnded, a NoticeRaised, or a
// RunnerVersionRefused.
type Happening interface{ happening() }

// AppliedCommand records a command the node applied, from any origin,
// whether or not it was connected: the node is history's only source of
// commands (ADR 0012). A refused command, a duplicate and a dismissal
// add none. An auto-resume is recorded as a Resume from AutoResume, with
// no ID and no base revision.
type AppliedCommand struct {
	ID     string
	Origin Origin
	Event  availability.Event
	// Cancelled names the jobs an Emergency stop cancelled, in slot
	// order, and is empty when none was running: Section 11's audited
	// Emergency stop is this entry.
	Cancelled []CancelledJob
	// Budget is the budget a SetBudget left, both halves, so history can
	// say what the budget was set to.
	Budget availability.Budget
}

// RunnerVersionRefused records that GitHub refused the runner
// application's Version: a runner's listener in Slot exited with
// ExitVersionRefused before it took a job. The Hub checks its target's
// version again.
type RunnerVersionRefused struct {
	Slot            int
	Runner, Version string
}

// DegradedBegan records that the node became Degraded, naming the slot
// that has failed longest and its reason. A reason that changes during
// the episode writes nothing.
type DegradedBegan struct {
	Slot   int
	Reason Reason
	Detail string
}

// DegradedEnded records that the node stopped being Degraded, and why:
// it recovered, or was paused or removed while Degraded. Began is when
// the episode began. The node records both ends because the most common
// reason, Hub unreachable, is exactly when the Hub can't see it.
type DegradedEnded struct {
	Began time.Time
	Why   Ending
}

// An Ending is why a Degraded episode ended.
type Ending int

const (
	// Recovered: no slot is past its grace any more.
	Recovered Ending = iota + 1
	// EndedByPause: the node was paused, which wants no runners.
	EndedByPause
	// EndedByRemoval: the node's removal started.
	EndedByRemoval
)

func (e Ending) String() string {
	switch e {
	case Recovered:
		return "recovered"
	case EndedByPause:
		return "until paused"
	case EndedByRemoval:
		return "until removed"
	}
	return fmt.Sprintf("Ending(%d)", int(e))
}

// A CancelledJob is a job Emergency stop cancelled.
type CancelledJob struct {
	Slot        int
	Runner, Job string
}

func (AppliedCommand) happening()       {}
func (Job) happening()                  {}
func (DegradedBegan) happening()        {}
func (DegradedEnded) happening()        {}
func (RunnerVersionRefused) happening() {}

// happened puts h in the outbox.
func (a *applying) happened(h Happening) {
	n := a.n
	n.Seq++
	n.Outbox = append(n.Outbox, Entry{Seq: n.Seq, At: a.now, What: h})
}

// deliver sends the Hub every entry it hasn't been sent on this
// connection: all of them just after connecting, and otherwise the ones
// this Apply put in the outbox.
func (a *applying) deliver() {
	n := a.n
	if !n.Connected {
		return
	}
	var due []Entry
	for _, e := range n.Outbox {
		if e.Seq > a.sent {
			due = append(due, e)
		}
	}
	if len(due) > 0 {
		a.emit(Deliver{Entries: due})
	}
}

// episode records the start or the end of a Degraded episode, if the
// node became Degraded or stopped being Degraded.
func (a *applying) episode() {
	n := a.n
	was, is := !n.DegradedSince.IsZero(), n.State() == Degraded
	switch {
	case is && !was:
		s := n.DegradedSlots()[0]
		n.DegradedSince = a.now
		a.happened(DegradedBegan{Slot: s.Number, Reason: s.Failure.Reason, Detail: s.Failure.Detail})
	case was && !is:
		why := Recovered
		switch {
		case n.Removal.Started:
			why = EndedByRemoval
		case n.Availability.Intent != availability.OnDuty:
			why = EndedByPause
		}
		a.happened(DegradedEnded{Began: n.DegradedSince, Why: why})
		n.DegradedSince = time.Time{}
	}
}

// acknowledged drops the entries the Hub has stored.
func (a *applying) acknowledged(through int64) {
	n := a.n
	for len(n.Outbox) > 0 && n.Outbox[0].Seq <= through {
		n.Outbox = n.Outbox[1:]
	}
}

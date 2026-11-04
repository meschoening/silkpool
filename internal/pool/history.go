package pool

import (
	"fmt"
	"time"

	"github.com/meschoening/silkpool/internal/node"
)

// What the Hub's history records, and who writes each entry, are in
// "Define Silkpool's events, audit trail, notices and redaction"
// and ADR 0012. Every command a node applies reaches history only
// through its outbox. The Hub writes entries only for what it does
// itself, or sees: connections, enrollment and removal, its targets and
// their runner versions, its settings changes, and sign-ins.

// An Entry is one entry of the Hub's history, which the Hub keeps
// through a Store.
type Entry struct {
	// Node is the ID of the node it's about, or empty for one about the
	// Hub, such as a target's.
	Node string
	// At is when it happened: the node's time for an entry from its
	// outbox, and the Hub's for one the Hub writes.
	At time.Time
	// Seq is the node's sequence number for an entry from its outbox, or
	// 0 for one the Hub writes.
	Seq int64
	// Origin is where it started: an applied command's, or the origin of
	// what the Hub did. It's zero for what has none, such as a job or a
	// connection gap.
	Origin node.Origin
	// Late reports that an entry from a node's outbox happened before
	// the connection that delivered it started, so it's shown with the
	// ↓ late mark. The node applied it while it wasn't connected, or its
	// delivery was lost with a connection.
	Late bool
	// What is what happened: a node.Happening for an entry from a node's
	// outbox, or one of the Hub's own, from this package.
	What any
}

// A Filter is one of the dashboard's Activity filters, each of which
// shows some kinds of entry.
type Filter int

const (
	// FilterCommands: applied commands, and removals started.
	FilterCommands Filter = iota + 1
	// FilterJobs: jobs.
	FilterJobs
	// FilterProblems: Degraded episodes, their ends, notices raised,
	// runner versions refused, and connections refused.
	FilterProblems
	// FilterConnections: connection gaps, Hub starts, sign-ins and
	// refused sign-ins.
	FilterConnections
	// FilterSettings: settings changes, runner version changes,
	// enrollments and tombstones.
	FilterSettings
)

func (f Filter) String() string {
	switch f {
	case FilterCommands:
		return "Commands"
	case FilterJobs:
		return "Jobs"
	case FilterProblems:
		return "Problems"
	case FilterConnections:
		return "Connections"
	case FilterSettings:
		return "Settings"
	}
	return fmt.Sprintf("Filter(%d)", int(f))
}

// Filter returns the Activity filter e is under, or 0 if what e records
// isn't one of history's kinds.
func (e Entry) Filter() Filter {
	switch e.What.(type) {
	case node.AppliedCommand, RemovalStarted:
		return FilterCommands
	case node.Job:
		return FilterJobs
	case node.DegradedBegan, node.DegradedEnded, node.NoticeRaised, node.RunnerVersionRefused, ConnectionRefused:
		return FilterProblems
	case ConnectionGap, HubStarted, SignedIn, SignInRefused:
		return FilterConnections
	case Enrollment, Tombstoned, LostTarget, RegainedTarget, RunnerVersionChanged,
		CodeMade, CodeRevoked, AddedTarget, RemovedTarget, Renamed, Retargeted,
		LabelsChanged, PoolLabelChanged, GitHubConnectionAdded, KeepHistoryChanged:
		return FilterSettings
	}
	return 0
}

// History is kept for the Hub's setting, "Keep history for N days" in
// Settings: DefaultKeepHistory days unless the operator changed it, at
// least MinKeepHistory, or forever. It's pruned once a day by each
// entry's own time, removed nodes' history included. Tombstones stay.
const (
	DefaultKeepHistory = 90
	MinKeepHistory     = 7
	// KeepForever keeps history forever.
	KeepForever = -1
	// PruneEvery is how often the Hub prunes history, and it does on
	// every start.
	PruneEvery = 24 * time.Hour
)

// ErrKeepHistory refuses a history setting that keeps less than
// MinKeepHistory days.
var ErrKeepHistory = fmt.Errorf("history is kept for %d days or more, or forever", MinKeepHistory)

// CheckKeepHistory returns ErrKeepHistory unless days is a history
// setting the Hub takes: MinKeepHistory or more, or KeepForever.
func CheckKeepHistory(days int) error {
	if days != KeepForever && days < MinKeepHistory {
		return ErrKeepHistory
	}
	return nil
}

// keepDays returns p's history setting in days, DefaultKeepHistory if
// it's unset, or KeepForever.
func (p Pool) keepDays() int {
	if p.KeepHistory == 0 {
		return DefaultKeepHistory
	}
	return p.KeepHistory
}

// keep returns how long p keeps history, or 0 to keep it forever. A
// setting below the minimum keeps the minimum, so a bad setting can't
// prune too much.
func (p Pool) keep() time.Duration {
	days := p.keepDays()
	switch {
	case days == KeepForever:
		return 0
	case days < MinKeepHistory:
		days = MinKeepHistory
	}
	return time.Duration(days) * 24 * time.Hour
}

// setKeepHistory sets the history setting, and writes it, unless it's
// what's already kept. One CheckKeepHistory refuses is refused.
func (a *applying) setKeepHistory(e SetKeepHistory) {
	if err := CheckKeepHistory(e.Days); err != nil {
		a.emit(ChangeRefused{Reason: err})
		return
	}
	if from := a.p.keepDays(); from != e.Days {
		a.store(Entry{At: a.now, Origin: e.Origin, What: KeepHistoryChanged{From: from, To: e.Days}})
		a.p.KeepHistory = e.Days
	}
}

// KeepHistoryChanged records that the operator changed the history
// setting From the days it kept To the days it keeps, either of which
// may be KeepForever.
type KeepHistoryChanged struct{ From, To int }

// prune asks the Hub to drop the entries older than p keeps, if it's
// due, and when to next.
func (a *applying) prune() {
	p := a.p
	if p.PruneAt.IsZero() || a.now.Before(p.PruneAt) {
		return
	}
	p.PruneAt = a.now.Add(PruneEvery)
	if keep := p.keep(); keep > 0 {
		a.emit(Prune{Before: a.now.Add(-keep)})
	}
}

// fromOutbox returns the Hub's entry for an entry n delivered, on the
// connection it holds.
func (n *Node) fromOutbox(e node.Entry) Entry {
	entry := Entry{Node: n.ID, At: e.At, Seq: e.Seq, Late: e.At.Before(n.ConnectedAt), What: e.What}
	if c, ok := e.What.(node.AppliedCommand); ok {
		entry.Origin = c.Origin
	}
	return entry
}

// ConnectionGap records that a node reconnected after its connection
// ended while the Hub was running: "Not connected 2:42–2:56 PM", from
// Since, its last seen, until the entry's time. A gap that began because
// the Hub went down writes none, since HubStarted covers it.
type ConnectionGap struct{ Since time.Time }

// HubStarted records that the Hub started, after being down since Alive,
// the latest time it noted it was running: "Hub started, after being
// down 12 min". It stands for the gap of every node that was connected
// when the Hub went down. A Hub's first start writes none.
type HubStarted struct{ Alive time.Time }

// ConnectionRefused records that the Hub refused a node's connection, at
// the start of an episode of refusals: Reason is a VersionMismatch or
// ErrWrongKey. The agent retries every 1 to 60 s, and the retries write
// nothing more.
type ConnectionRefused struct{ Reason error }

// Enrollment records that a machine enrolled as the entry's node, named
// Name, with Target as its target. Its origin is the machine's enroll.
// A retry answered again writes none.
type Enrollment struct{ Name, Target string }

// RemovalStarted records that a node's removal started, from the entry's
// origin. Like a command, it's under the Commands filter.
type RemovalStarted struct{}

// Tombstoned records that a node became a tombstone: it confirmed its
// wipe, or its connection ended while it was being removed, or it wasn't
// connected when its removal started. The Hub does it, so the Hub is its
// origin.
type Tombstoned struct{}

// RunnerVersionChanged records that package runnerapp moved Target's
// runner version From one version To another, or to its first. The Hub
// does it, so the Hub is its origin.
type RunnerVersionChanged struct{ Target, From, To string }

// LostTarget records that Target became a lost target: the Hub can no
// longer see it on GitHub. The Hub finds it, so the Hub is its origin.
type LostTarget struct{ Target string }

// RegainedTarget records that the Hub can see Target on GitHub again.
type RegainedTarget struct{ Target string }

// hubOrigin is the origin of what the Hub does on its own.
var hubOrigin = node.Origin{Surface: node.Hub}

// store asks the Hub to keep e in its history.
func (a *applying) store(e Entry) { a.emit(Store{Entry: e}) }

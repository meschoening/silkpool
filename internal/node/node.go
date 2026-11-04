// Package node is the pure core of a node: its availability, its slots
// and the runners they host, and the state they add up to, with the
// commands it applies, its outbox to the Hub, its notices, and its
// enrollment and removal. The decisions behind it are in "Define node states and the
// drain/retirement contract";
// for resets and the exposure check, "Choose the isolation models
// Silkpool supports";
// for commands, the outbox, enrollment, the Hub's identity and removal,
// "Define node enrollment, removal, identity and Hub-agent transport";
// for node names and checking the zone before enrolling, "Define what
// the Hub and the agent persist";
// for the runner application, "Define how the agent installs, verifies
// and updates the runner application"; and for what
// the node records for the Hub's history, and its notices, "Define
// Silkpool's events, audit trail, notices and redaction",
// recorded as ADR 0012.
//
// Everything here is a function of (node, event, now), like package
// availability, which it builds on. The agent applies an event for
// everything it learns, carries out the effects that come back, and
// applies a Tick at each WakeAt. Nothing reads the clock, sleeps or does
// I/O.
package node

import (
	"fmt"
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
)

// The node's timings.
const (
	// Grace is how long a wanted slot may go without a runner, while
	// getting one fails, before the node is Degraded.
	Grace = 60 * time.Second
	// OnlineTimeout is how long a started listener has to start
	// listening before its slot counts as failing with RunnerOffline.
	OnlineTimeout = 60 * time.Second
	// KillAfter is how long an interrupted listener has to exit before
	// it's killed.
	KillAfter = 30 * time.Second
	// WorkerWait is the longest a graceful stop of a runner holding a job
	// waits for the job's worker before it interrupts the listener anyway.
	// A listener interrupted before its worker is up drops the job without
	// anyone reporting it.
	WorkerWait = 5 * time.Second
	// FirstRetry and MaxRetry bound the wait between attempts, which
	// doubles from FirstRetry up to MaxRetry.
	FirstRetry = 5 * time.Second
	MaxRetry   = 5 * time.Minute
)

// A Node is one node as its agent sees it. The agent owns and persists
// it; the Hub's copy is a mirror of its reports. The zero Node isn't
// enrolled: Apply refuses its commands but Enroll, and ignores everything
// else but the Hub's answer to it.
type Node struct {
	Enrolled     bool
	Availability availability.Node
	// Machine is what the node knows of its machine, as the agent last
	// told it.
	Machine Machine
	// InForce is the budget the slots are held to, which the agent last
	// applied. It differs from Availability.Budget only while a lowered
	// memory budget is pending: its memory is then the one still in force.
	InForce availability.Budget
	// Slots are in order, numbered from 1: one for each slot up to the
	// slot limit, plus any above it still occupied.
	Slots []Slot
	// Target names the node's target, a Hub-owned setting, or is empty
	// if it has none.
	Target string
	// Scope is what the target covers, which comes with it: the node keeps
	// a job's run only if the target covers its repository.
	Scope Scope
	// RunnerApp is the runner application the Hub wants the node to run,
	// a Hub-owned setting, or zero until the Hub has sent one. It isn't
	// stored: it arrives in HubSettings on every connection.
	RunnerApp RunnerApp
	// Installed is the version of the verified install new runners start
	// from, or empty while the node has none. Installs are directories,
	// not stored: Started brings the ones the agent finds.
	Installed string
	// Discarded reports that the agent reported an install ready that
	// the node didn't keep, because the Hub had moved on, while it had
	// none: until it has one, the agent may prepare a slot from that one,
	// and no runner could start in it.
	Discarded bool
	// Update is the install of RunnerApp under way, while it isn't the
	// one Installed.
	Update Update
	// DegradedSince is when the node's Degraded episode began, while it's
	// Degraded, or zero.
	DegradedSince time.Time
	// Connected reports that the agent holds a live, authenticated
	// connection to the Hub.
	Connected bool
	Removal   Removal
	// Notices are the node's notices, oldest first.
	Notices []Notice
	// Replies are the replies to the commands of the last KeepReplies,
	// oldest first.
	Replies []Reply
	// Outbox holds the entries the Hub hasn't stored yet, oldest first,
	// and Seq is the newest entry's sequence number.
	Outbox []Entry
	Seq    int64
	// IntentSet is the outbox entry of the applied command that last
	// changed the intent, or Emergency stop, or the auto-resume that
	// ended a pause. It's kept after the Hub stores the entry, so every
	// surface can say who set the intent and when. It's zero until a
	// command changes the intent after enrollment.
	IntentSet Entry
}

// New returns a newly enrolled node: Paused indefinitely, as
// availability.New makes it, with one free slot.
func New(zone *time.Location) (Node, error) {
	a, err := availability.New(zone)
	if err != nil {
		return Node{}, err
	}
	return Node{Enrolled: true, Availability: a, Slots: []Slot{{Number: 1}}}, nil
}

// Removal is how far the node's removal has gone. The zero Removal is
// none.
type Removal struct {
	// Started reports that the removal has started, which can't be
	// undone.
	Started bool
	// Force reports a forced removal, which wipes without the Hub.
	Force bool
	// Heard reports that the Hub answered a connection with "removed".
	Heard bool
}

// A Slot is a numbered position on a node that hosts at most one runner
// at a time.
type Slot struct {
	Number int
	Phase  Phase
	// Runner is the runner the slot hosts, while Hosting.
	Runner Runner
	// Failure is why the slot is failing to get a runner, or to reset.
	// It lasts until the slot has a runner, or is free and unwanted. An
	// exposure check hit lasts while the slot is unwanted too, so a paused
	// node shows it before it resumes, until a check passes.
	Failure Failure
	// RunnerlessSince is when the slot, wanted, last went without a
	// runner listening or holding a job. It's zero while the slot has one
	// or isn't wanted.
	RunnerlessSince time.Time
	// Degraded reports that the slot has been without a runner for
	// longer than Grace, while getting one failed.
	Degraded bool
	// Ended is the job the slot's last runner held when it ended, kept
	// until the slot's reset copies the runner's logs and the node records
	// it, or zero.
	Ended Job
}

// A Phase is what's under way in a slot.
type Phase int

const (
	// Free holds nothing and has been reset.
	Free Phase = iota
	// Preparing is checking the slot before it gets a runner. It doesn't
	// hold up Drained.
	Preparing
	// Requesting has asked the Hub for a runner: a registration in
	// flight.
	Requesting
	// Hosting holds a runner.
	Hosting
	// Resetting is resetting the slot after its runner ended.
	Resetting
	// RetryingReset failed to reset the slot, and tries again at
	// Failure.RetryAt.
	RetryingReset
	// DeletingAccount is deleting the slot's account, after its reset,
	// because the node is being removed.
	DeletingAccount
	// RetryingDelete failed to delete the slot's account, and tries
	// again at Failure.RetryAt.
	RetryingDelete
	// Deleted has had its account deleted, and is gone with the node.
	Deleted
)

func (p Phase) String() string {
	switch p {
	case Free:
		return "free"
	case Preparing:
		return "preparing"
	case Requesting:
		return "requesting"
	case Hosting:
		return "hosting"
	case Resetting:
		return "resetting"
	case RetryingReset:
		return "retrying reset"
	case DeletingAccount:
		return "deleting account"
	case RetryingDelete:
		return "retrying delete"
	case Deleted:
		return "deleted"
	}
	return fmt.Sprintf("Phase(%d)", int(p))
}

// occupied reports whether s holds up Drained.
func (s Slot) occupied() bool {
	return s.Phase != Free && s.Phase != Preparing && s.Phase != Deleted
}

// serving reports whether s has a runner listening for a job or holding
// one.
func (s Slot) serving() bool {
	r := s.Runner
	return s.Phase == Hosting && (r.Listening || r.Job != "") && !r.Retirement.signalled()
}

// A Runner is one single-job registration minted by the Hub, with the
// listener process that serves it.
type Runner struct {
	Name string
	// Started is when the agent was asked to start its listener.
	Started time.Time
	// Listening reports that the listener has connected and is listening
	// for a job.
	Listening bool
	// Late reports that it didn't start listening within OnlineTimeout.
	// A late runner is retired, DELETE-first, at the slot's next retry,
	// unless it starts listening first.
	Late bool
	// Job names the job it holds, if any, and JobSince is when the node
	// heard of it.
	Job      string
	JobSince time.Time
	// Result is GitHub's result for the job, once the listener reports
	// that it completed.
	Result Result
	// WorkerUp reports that the worker of its job is up.
	WorkerUp bool
	// Version is the version of the install its listener started from.
	Version    string
	Retirement Retirement
	// RetireAttempts counts unanswered requests to retire it, and
	// RetireAt is when to ask again, or zero while a request is out.
	RetireAttempts int
	RetireAt       time.Time
	// KillAt is when to kill an interrupted listener, or zero once it's
	// been killed, or while its stop is held.
	KillAt time.Time
	// InterruptAt is when a stop held for the job's worker interrupts the
	// listener anyway, or zero while no stop is held.
	InterruptAt time.Time
}

// waitsForWorker reports whether a graceful stop of r holds its
// Interrupt: r holds a job that hasn't completed, whose worker isn't up
// yet.
func (r Runner) waitsForWorker() bool {
	return r.Job != "" && r.Result == "" && !r.WorkerUp
}

// Retirement is how far a runner is in being taken out of service.
type Retirement int

const (
	// NotRetiring is in service.
	NotRetiring Retirement = iota
	// Retiring has asked the Hub to delete its registration.
	Retiring
	// ClosingAfterJob was refused deletion (422) because it has, or is
	// about to get, its one job. It's left alone until it exits.
	ClosingAfterJob
	// Stopping had its registration deleted, so it can never get a job,
	// and its listener has been interrupted.
	Stopping
	// Cancelling had its listener interrupted by Emergency stop, job and
	// all.
	Cancelling
	// StoppingLost had its listener interrupted, job and all, because
	// its target is lost and the Hub can't delete its registration.
	StoppingLost
)

// signalled reports whether r's listener is being stopped gracefully:
// interrupted, to be killed at KillAt, or, while the stop is held for
// its job's worker, to be interrupted by InterruptAt.
func (r Retirement) signalled() bool {
	return r == Stopping || r == Cancelling || r == StoppingLost
}

// A Reason is why a slot is failing to get a runner. Each String is the
// title the node shows.
type Reason int

const (
	// HubUnreachable couldn't get a registration from the Hub.
	HubUnreachable Reason = iota + 1
	// GitHubRefused: the App isn't installed on the target, lacks a
	// permission, or the target isn't allowed.
	GitHubRefused
	// GitHubUnavailable: API errors or rate limiting, as the Hub sees
	// them.
	GitHubUnavailable
	// RunnerOffline: the listener exited, or didn't connect.
	RunnerOffline
	// RunnerAppUnavailable: the runner application is missing or failed
	// verification, or GitHub no longer accepts its version.
	RunnerAppUnavailable
	// SlotCantStart: the slot's isolation context failed, or its reset
	// did.
	SlotCantStart
	// SlotExposed: the exposure check found something outside the slot
	// that it can open.
	SlotExposed
	// VersionMismatch: the Hub refused the agent's protocol version.
	VersionMismatch
	// HubIdentityChanged: the Hub presented a key other than the one the
	// node pinned.
	HubIdentityChanged
	// WrongNodeKey: the Hub refused the agent's connection because it
	// holds a different node key for the node, as for a machine whose
	// state was restored or copied with a key of its own. Node keys don't
	// rotate, so the Detail names the fix: remove the node and enroll the
	// machine again.
	WrongNodeKey
)

func (r Reason) String() string {
	switch r {
	case HubUnreachable:
		return "Hub unreachable"
	case GitHubRefused:
		return "GitHub refused the registration"
	case GitHubUnavailable:
		return "GitHub unavailable"
	case RunnerOffline:
		return "Runner didn't come online"
	case RunnerAppUnavailable:
		return "Runner application unavailable"
	case SlotCantStart:
		return "Slot can't start"
	case SlotExposed:
		return "Slot can see files outside its own"
	case VersionMismatch:
		return "Agent and Hub versions don't match"
	case HubIdentityChanged:
		return "Hub identity changed"
	case WrongNodeKey:
		return "Hub holds a different key for this node"
	}
	return fmt.Sprintf("Reason(%d)", int(r))
}

// offHub reports whether r means the slot's request never reached the
// Hub: the Hub couldn't be reached, or it refused the agent's connection.
func (r Reason) offHub() bool {
	return r == HubUnreachable || r == VersionMismatch || r == HubIdentityChanged || r == WrongNodeKey
}

// A Failure is a slot's run of failed attempts. The zero Failure is
// none.
type Failure struct {
	Reason Reason
	// Detail is the plain detail of the latest failure. For SlotExposed
	// it names the path and the fix, and for WrongNodeKey the fix.
	Detail   string
	Attempts int
	// Since is when the first attempt failed.
	Since time.Time
	// RetryAt is the earliest time of the next attempt.
	RetryAt time.Time
}

// Backoff is the wait after the attempts-th failed attempt: it doubles
// from FirstRetry up to MaxRetry. The Hub retries stale registrations on
// the same backoff.
func Backoff(attempts int) time.Duration {
	d := FirstRetry
	for i := 1; i < attempts && d < MaxRetry; i++ {
		d *= 2
	}
	return min(d, MaxRetry)
}

// A State is what a node is observed doing.
type State int

const (
	// Drained: paused, and every slot is free.
	Drained State = iota
	// Draining: paused, and a slot is still occupied.
	Draining
	// Starting: On duty, and no runner is listening or busy yet.
	Starting
	// Available: On duty, with a runner listening and none busy.
	Available
	// Busy: On duty, with a runner holding a job.
	Busy
	// Degraded: On duty, with a slot past its grace without a runner
	// because getting one failed.
	Degraded
)

func (s State) String() string {
	switch s {
	case Drained:
		return "Drained"
	case Draining:
		return "Draining"
	case Starting:
		return "Starting"
	case Available:
		return "Available"
	case Busy:
		return "Busy"
	case Degraded:
		return "Degraded"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// State returns the node's state. While On duty the first match wins:
// Degraded, Busy, Available, then Starting. While paused it's Draining
// until every slot is free, then Drained.
func (n Node) State() State {
	if n.Availability.Intent != availability.OnDuty {
		if slices.ContainsFunc(n.Slots, Slot.occupied) {
			return Draining
		}
		return Drained
	}
	state := Starting
	for _, s := range n.Slots {
		switch {
		case s.Degraded:
			return Degraded
		case !s.serving():
		case s.Runner.Job != "":
			state = Busy
		case state == Starting:
			state = Available
		}
	}
	return state
}

// DegradedSlots returns the slots that make the node Degraded, the one
// failing longest first. The node shows that one's reason and lists the
// others in its details.
func (n Node) DegradedSlots() []Slot {
	var degraded []Slot
	for _, s := range n.Slots {
		if s.Degraded {
			degraded = append(degraded, s)
		}
	}
	slices.SortStableFunc(degraded, func(a, b Slot) int {
		return a.Failure.Since.Compare(b.Failure.Since)
	})
	return degraded
}

// wants reports whether n wants s to have a runner.
func (n Node) wants(s Slot) bool {
	return n.Availability.Intent == availability.OnDuty && s.Number <= n.Availability.SlotLimit
}

// deadline returns the earliest time at which n has something to do, or
// zero if there's none.
func (n Node) deadline() time.Time {
	var times []time.Time
	if n.Availability.Intent == availability.Paused {
		times = append(times, n.Availability.ResumeAt)
	}
	if u := n.Update; !u.Out && u.Attempts > 0 && u.Version == n.RunnerApp.Version && u.Version != n.Installed && !n.Removal.Started {
		times = append(times, u.RetryAt)
	}
	for _, s := range n.Slots {
		wanted, r := n.wants(s), s.Runner
		switch {
		case s.Phase == Free && wanted, s.Phase == RetryingReset, s.Phase == RetryingDelete:
			times = append(times, s.Failure.RetryAt)
		case s.Phase != Hosting:
		case r.Retirement == NotRetiring && wanted && !r.Listening && !r.Late:
			times = append(times, r.Started.Add(OnlineTimeout))
		case r.Retirement == NotRetiring && wanted && !r.Listening:
			times = append(times, s.Failure.RetryAt)
		case r.Retirement == Retiring:
			times = append(times, r.RetireAt)
		case r.Retirement.signalled():
			times = append(times, r.InterruptAt, r.KillAt)
		}
		if wanted && s.Failure.Attempts > 0 && !s.Degraded && !s.RunnerlessSince.IsZero() {
			times = append(times, s.RunnerlessSince.Add(Grace))
		}
	}
	times = slices.DeleteFunc(times, time.Time.IsZero)
	if len(times) == 0 {
		return time.Time{}
	}
	return slices.MinFunc(times, time.Time.Compare)
}

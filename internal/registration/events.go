package registration

import (
	"time"

	"github.com/meschoening/silkpool/internal/node"
)

// An Event is something the Hub learns: from a node, from GitHub, or
// that the clock has moved on. Events about a runner the Hub holds no
// record of, or that no longer match what's under way, are ignored.
type Event interface{ event() }

// Started tells the Hub that it has just started, and comes before any
// other event. No node is connected yet, and every answer from GitHub
// that was still out has been lost.
type Started struct{}

// Connected tells the Hub that a node has connected. A newer connection
// replaces any older one.
type Connected struct{ Node string }

// Disconnected tells the Hub that a node's connection has ended.
type Disconnected struct{ Node string }

// Reported brings a node's report: the full snapshot it sends when it
// connects, or one it pushes after a change. Report is the node as it
// reported itself.
type Reported struct {
	Node   string
	Report node.Node
}

// RunnerRequested relays a node's request for a runner for a slot, as
// its RequestRunner effect makes it. NodeName is the node's name now,
// and Target the target it registers in. Random is a random number the
// caller draws, from which the runner's name gets its suffix.
type RunnerRequested struct {
	Node, NodeName string
	Slot           int
	Target         string
	Random         uint32
}

// Generated reports that GitHub generated a runner's JIT config, which
// the Hub keeps, and gave it ID.
type Generated struct {
	Runner string
	ID     int64
}

// NameTaken reports that GitHub refused to generate a runner's JIT
// config because its name is taken (409). Random is a fresh random
// number for the next name.
type NameTaken struct {
	Runner string
	Random uint32
}

// GenerateFailed reports that GitHub didn't generate a runner's JIT
// config. Status is GitHub's HTTP status, 429 for any rate limit, or 0
// if GitHub couldn't be reached or didn't answer. With no answer, or a
// 5xx, the registration may exist all the same.
type GenerateFailed struct {
	Runner string
	Status int
	Detail string
}

// RetireRequested relays a node's request to retire a runner, as its
// RetireRunner effect makes it.
type RetireRequested struct{ Node, Runner string }

// RunnersOutdated reports that a node's runners carry labels or a target
// it no longer has, after the operator edited them. The Hub retires each
// runner DELETE-first: an idle one is stopped and minted again, and a
// busy one keeps its job.
type RunnersOutdated struct{ Node string }

// Deleted reports the result of a DeleteRunner. Status is GitHub's HTTP
// status, or 0 if GitHub couldn't be reached. TargetVisible reports that
// the Hub can still see the target, without which a 404 doesn't mean the
// registration is gone.
type Deleted struct {
	Runner        string
	Status        int
	TargetVisible bool
}

// TargetLost reports positive evidence that the Hub can no longer see a
// target: the installation's repository list no longer holds it, or the
// installation is gone or suspended.
type TargetLost struct{ Target string }

// Removing tells the Hub that a node's removal has started, whether or
// not it's connected. The Hub mints nothing more for it, and deletes
// every registration it has, DELETE first: a 204 stops the runner, and a
// 422 leaves it to finish its job while the Hub retries.
type Removing struct{ Node string }

// Listed brings GitHub's list of the runners registered in a target, for
// the periodic comparison.
type Listed struct {
	Target  string
	Runners []Listing
}

// A Listing is one runner in GitHub's list.
type Listing struct {
	ID   int64
	Name string
}

// Tick tells the Hub that the clock reads now. The Hub applies one at
// every WakeAt.
type Tick struct{}

func (Started) event()         {}
func (Connected) event()       {}
func (Disconnected) event()    {}
func (Reported) event()        {}
func (RunnerRequested) event() {}
func (Generated) event()       {}
func (NameTaken) event()       {}
func (GenerateFailed) event()  {}
func (RetireRequested) event() {}
func (RunnersOutdated) event() {}
func (Deleted) event()         {}
func (TargetLost) event()      {}
func (Removing) event()        {}
func (Listed) event()          {}
func (Tick) event()            {}

// An Effect is something Apply asks of the Hub, beyond saving the
// Records it returns, which comes first. Granted, GrantFailed, Retired
// and StopLostRunners are messages to a node, which applies each as the
// node event of the same name. The Hub sends one only while the node is connected, and
// never queues it.
type Effect interface{ effect() }

// GenerateJITConfig asks the Hub to have GitHub generate a JIT config
// for a runner of a node in a target, with the node's labels, and to
// apply Generated, NameTaken or GenerateFailed.
type GenerateJITConfig struct{ Node, Runner, Target string }

// Granted sends a node the runner granted to a slot, with its JIT
// config.
type Granted struct {
	Node   string
	Slot   int
	Runner string
}

// GrantFailed tells a node that a slot's runner couldn't be granted:
// Reason is GitHubRefused or GitHubUnavailable.
type GrantFailed struct {
	Node   string
	Slot   int
	Reason node.Reason
	Detail string
}

// DeleteRunner asks the Hub to DELETE a runner's registration from its
// target, and to apply the result as Deleted.
type DeleteRunner struct {
	Runner string
	ID     int64
	Target string
}

// Retired sends a node the result of a DELETE of one of its runners:
// the reply to its RetireRequested, or "stop runner X" after one of the
// Hub's own DELETEs got 204.
type Retired struct {
	Node          string
	Runner        string
	Status        int
	TargetVisible bool
}

// StopLostRunners sends a node "stop a lost target's runners": the
// runners it may host whose target is lost, which the Hub can't delete.
// The node stops them itself.
type StopLostRunners struct {
	Node    string
	Runners []string
}

// WakeAt asks the Hub to apply a Tick at At. It comes last, whenever a
// retry is due later, and replaces any earlier WakeAt.
type WakeAt struct{ At time.Time }

func (GenerateJITConfig) effect() {}
func (Granted) effect()           {}
func (GrantFailed) effect()       {}
func (DeleteRunner) effect()      {}
func (Retired) effect()           {}
func (StopLostRunners) effect()   {}
func (WakeAt) effect()            {}

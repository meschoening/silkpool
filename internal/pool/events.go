package pool

import (
	"time"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/runnerapp"
)

// An Event is something the Hub learns: from a node, from its operator,
// or that it has just started. Events about a node the pool doesn't hold
// are ignored.
type Event interface{ event() }

// Started tells the Hub it has just started, and comes before any other
// event. No node is connected yet. Protocol is the version of the
// Hub-agent protocol the Hub speaks. A Hub that has run before writes
// how long it was down, from the latest time it noted it was alive, and
// every start prunes history.
type Started struct{ Protocol int }

// Tick tells the Hub that the clock reads now. The Hub applies one at
// every WakeAt, which comes at least once a minute once it has started,
// so it notes it's alive.
type Tick struct{}

// Connected tells the Hub that a node has connected and proved itself
// with its node key, stating its agent's protocol version and its
// platform. A newer connection replaces any older one, unless the Hub
// refuses it. The agent is built only for the platforms Silkpool
// supports, so it always states one of them.
type Connected struct {
	Node     string
	Protocol int
	Platform runnerapp.Platform
}

// KeyRefused tells the Hub that a connection named Node but didn't prove
// itself with the node's key. The Hub refuses it with ErrWrongKey, and
// leaves any connection the node holds alone.
type KeyRefused struct{ Node string }

// Disconnected tells the Hub that a node's connection has ended. Heard is
// when the connection last heard the node, its keepalive pings
// included, which no other event brings, or zero if it heard nothing
// since the latest event the pool applied from the node.
type Disconnected struct {
	Node  string
	Heard time.Time
}

// Reported brings a node's report: the full snapshot it sends when it
// connects, or one it pushes after a change.
type Reported struct {
	Node   string
	Report node.Node
}

// Issued is a command from the dashboard or the Hub's CLI for a node: a
// node.Command, or a node.Dismiss of one of its notices. It's sent only
// to a connected node, and otherwise refused with "Not connected: not
// sent". Nothing is queued. A budget for a Mac is refused with
// ChangeRefused and node.ErrNoBudget, since a Mac has none.
type Issued struct {
	Node    string
	Command node.Event
}

// Replied brings a node's reply to a command the Hub sent it.
type Replied struct {
	Node  string
	Reply node.Reply
}

// Remove starts a node's removal, from the dashboard, the Hub's CLI, or
// the node's own remove passed on by its agent. Unlike a command, it
// doesn't need the node to be connected.
type Remove struct {
	Node   string
	ID     string
	Origin node.Origin
}

// Confirmed tells the Hub that a removing node has wiped itself. The
// Hub tombstones it and ends its connection.
type Confirmed struct{ Node string }

// Delivered brings outbox entries from a node, in order.
type Delivered struct {
	Node    string
	Entries []node.Entry
}

// RunnerAppChanged tells the pool that Target's runner version changed,
// as package runnerapp reports it. Each of the target's connected nodes
// is sent its Hub settings, with the SHA-256 for its own platform. A
// target the pool doesn't hold is ignored.
type RunnerAppChanged struct {
	Target  string
	Version runnerapp.Version
}

// AddTarget tells the pool the operator added Target, which covers
// Scope, from Origin, or that its scope changed. Its runner version comes
// with RunnerAppChanged.
type AddTarget struct {
	Target string
	Scope  node.Scope
	Origin node.Origin
}

// DropTarget tells the pool the operator removed Target, from Origin.
type DropTarget struct {
	Target string
	Origin node.Origin
}

// TargetLost reports positive evidence that the Hub can no longer see
// Target on GitHub, as package github's Lost finds it: the installation's
// repository list no longer holds it, or the installation is gone or
// suspended. The Hub's registration records hear of it too.
type TargetLost struct{ Target string }

// TargetRegained reports that the Hub can see Target on GitHub again
// after losing it.
type TargetRegained struct{ Target string }

// MakeCode makes an enrollment code, from "Add node" in the dashboard or
// the Hub's CLI, whose origin is Origin. The caller draws its ID and its
// secret, since the core has no randomness of its own, and passes the
// secret's Hash.
type MakeCode struct {
	ID     string
	Hash   Hash
	Origin node.Origin
}

// RevokeCode revokes the enrollment code ID names, so it enrolls
// nothing, from Origin. A code that has been used stays as it is.
type RevokeCode struct {
	ID     string
	Origin node.Origin
}

// Rename renames a node Name, from Origin, under node.CheckName's rule,
// to a name no other node has, ignoring case. It's answered with
// ChangeRefused if the name breaks the rule or is taken. A tombstone
// keeps its name.
type Rename struct {
	Node   string
	Name   string
	Origin node.Origin
}

// Retarget gives a node Target as its target, from Origin. If it's
// connected, it's sent its new Hub settings, and its runners, minted for
// its old target, are retired. It's answered with ChangeRefused if the
// pool doesn't hold the target. A tombstone keeps its target.
type Retarget struct {
	Node   string
	Target string
	Origin node.Origin
}

// SetKeepHistory sets the Hub's history setting, "Keep history for N
// days", from Origin: Days, or KeepForever. It's answered with
// ChangeRefused if CheckKeepHistory refuses it.
type SetKeepHistory struct {
	Days   int
	Origin node.Origin
}

// SignIn tells the Hub the operator signed in to the dashboard as the
// GitHub user Login, from the remote address Address.
type SignIn struct{ Login, Address string }

// RefuseSignIn tells the Hub it refused a sign-in to the dashboard by
// the GitHub user Login, who isn't the operator, from the remote address
// Address.
type RefuseSignIn struct{ Login, Address string }

// Enroll is a machine's request to enroll with a code. It's answered
// with Enrolled, or with EnrollRefused, in which case nothing is saved.
type Enroll struct {
	// Hash is the hash of the secret in the code the machine sent, which
	// the Hub hashes before it applies Enroll.
	Hash Hash
	// Node is the ID the new node gets. The caller draws it at random.
	Node string
	// Name is the name the machine asks for: the one its administrator
	// gave enroll, or node.NameFromHostname's offer.
	Name string
	// Key is the node key's public half, which the machine made before it
	// sent its first Enroll with this code, and presents again on a retry.
	Key string
	// Protocol is the agent's protocol version.
	Protocol int
	// Origin is the origin of the machine's enroll: the node's CLI, and
	// the administrator's account, as node.EnrollAtHub carries it. The
	// enrollment's entry in history has it.
	Origin node.Origin
}

func (Started) event()          {}
func (Tick) event()             {}
func (Connected) event()        {}
func (Disconnected) event()     {}
func (KeyRefused) event()       {}
func (Reported) event()         {}
func (Issued) event()           {}
func (Replied) event()          {}
func (Remove) event()           {}
func (Confirmed) event()        {}
func (Delivered) event()        {}
func (RunnerAppChanged) event() {}
func (AddTarget) event()        {}
func (DropTarget) event()       {}
func (TargetLost) event()       {}
func (TargetRegained) event()   {}
func (MakeCode) event()         {}
func (RevokeCode) event()       {}
func (Enroll) event()           {}
func (Rename) event()           {}
func (Retarget) event()         {}
func (SetKeepHistory) event()   {}
func (SignIn) event()           {}
func (RefuseSignIn) event()     {}

// An Effect is something Apply asks of the Hub, beyond saving the Pool
// it returns, which comes first. Send and Acknowledge are messages to a
// node, which the Hub sends only while it's connected.
type Effect interface{ effect() }

// Send sends a node a message: a node.Command, a node.Dismiss or a
// node.Remove, which the node replies to, or a node.HubSettings, which it
// doesn't.
type Send struct {
	Node    string
	Message node.Event
}

// NotSent refuses a command for a node that isn't connected: "Not
// connected: not sent".
type NotSent struct{ Node, ID string }

// DeleteRegistrations asks the Hub to apply registration.Removing for a
// node: it mints nothing more for it and deletes every registration it
// has.
type DeleteRegistrations struct{ Node string }

// RetireRunners asks the Hub to apply registration.RunnersOutdated for a
// node: its runners were minted for a target it no longer has, so the
// records retire them, as they do after a label change.
type RetireRunners struct{ Node string }

// AnswerRemoved answers a tombstone's connection with "removed", which
// the agent applies as node.Removed, and then ends the connection. Lost
// names the node's runners in lost targets, which the Hub can't delete,
// for the node to stop itself. The pool keeps no runners: the Hub's
// store adds them from its registration records.
type AnswerRemoved struct {
	Node string
	Lost []string
}

// AnswerRefused answers a node's connection with a refusal naming the
// rule, and then ends that connection. The node isn't connected through
// it, and a connection it already holds is left as it is.
type AnswerRefused struct {
	Node   string
	Reason error
}

// Store asks the Hub to keep an entry in its history, in the same
// commit as the Pool that comes with it.
type Store struct{ Entry Entry }

// Prune asks the Hub to drop every history entry whose time is before
// Before, removed nodes' included, in the same commit as the Pool that
// comes with it.
type Prune struct{ Before time.Time }

// RunnerVersionRefused reports that a node's refusal entry was stored:
// GitHub refused Version on the node's target. The Hub applies it as
// runnerapp.Refused.
type RunnerVersionRefused struct{ Node, Target, Version string }

// Acknowledge tells a node the Hub has stored its entries through
// Through, which it applies as node.Acknowledged.
type Acknowledge struct {
	Node    string
	Through int64
}

// Enrolled accepts a machine's enrollment: it's now node Node, named
// Name, with Target as its target.
type Enrolled struct{ Node, Name, Target string }

// EnrollRefused refuses an enrollment with the code whose secret hashes
// to Hash, naming the rule. Nothing is saved.
type EnrollRefused struct {
	Hash   Hash
	Reason error
}

// ChangeRefused refuses an operator's change, naming the rule. Nothing
// changes, and nothing is written.
type ChangeRefused struct{ Reason error }

// WakeAt asks the Hub to apply a Tick at At. It comes last, once the Hub
// has started or while it holds a code: to note it's alive, to prune its
// history, or to drop a code. It replaces any earlier WakeAt.
type WakeAt struct{ At time.Time }

func (Send) effect()                 {}
func (NotSent) effect()              {}
func (DeleteRegistrations) effect()  {}
func (RetireRunners) effect()        {}
func (AnswerRemoved) effect()        {}
func (AnswerRefused) effect()        {}
func (Store) effect()                {}
func (Prune) effect()                {}
func (RunnerVersionRefused) effect() {}
func (Acknowledge) effect()          {}
func (Enrolled) effect()             {}
func (EnrollRefused) effect()        {}
func (ChangeRefused) effect()        {}
func (WakeAt) effect()               {}

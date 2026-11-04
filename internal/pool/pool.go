// Package pool is the pure core of the Hub's view of its nodes: their
// enrollment, whether each is connected, the mirror of its reports, the
// commands the Hub has sent it, the outbox entries it has delivered, and
// its removal. The decisions behind it are in "Define node enrollment,
// removal, identity and Hub-agent transport".
//
// It's the Hub's half of that decision, beside package registration,
// which keeps the runners the Hub mints, and package node, which is the
// agent's half. It also sends each node its Hub settings: its target,
// what the target covers, and the runner version package runnerapp
// picks for it.
//
// It writes the Hub's history, keeps each node's last seen, and raises
// the Hub's own notices, as "Define Silkpool's events, audit trail,
// notices and redaction"
// and ADR 0012 decide. A node's outbox entries reach history through
// it, marked late if they happened before the connection that delivered
// them. The Hub writes its own for connections, enrollment and removal,
// its targets, the operator's settings changes and sign-ins, and prunes
// history daily.
//
// Everything here is a function of (pool, event, now). The Hub applies
// an event for everything it learns from its nodes and its operator,
// saves the Pool that comes back with the history it asks for, and only
// then carries out the effects. It applies a Tick at each WakeAt.
// Nothing reads the clock, sleeps or does I/O.
//
// What the Hub keeps of its codes and nodes, and the node names it
// accepts, are in "Define what the Hub and the agent persist".
package pool

import (
	"crypto/sha256"
	"time"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/runnerapp"
)

// A Pool is the Hub's nodes, and the codes that enroll new ones.
type Pool struct {
	// Nodes are in the order they enrolled, tombstones included. Each
	// name is unique among the nodes that aren't tombstones, ignoring
	// case.
	Nodes []Node
	// Codes are the enrollment codes the Hub has made, oldest first, for
	// CodeRetention after each was made.
	Codes []Code
	// DefaultTarget is the target a new node starts with, or empty if
	// the Hub has none. Target management sets it.
	DefaultTarget string
	// Targets are what the pool keeps of each target the Hub holds, by
	// its key, to send its nodes their Hub settings and to raise the
	// notice of a lost target.
	Targets map[string]Target
	// Protocol is the version of the Hub-agent protocol the Hub speaks,
	// as of its latest start.
	Protocol int
	// KeepHistory is the Hub's history setting, in days: zero for
	// DefaultKeepHistory, or KeepForever.
	KeepHistory int
	// PruneAt is when the Hub next prunes its history, or zero before it
	// first starts. It isn't stored: every start prunes.
	PruneAt time.Time
	// Alive is the latest time the Hub noted it was running, once every
	// AliveEvery, or zero before it first starts. The next start tells
	// from it how long the Hub was down.
	Alive time.Time
	// SignInsRefused holds when the Hub last wrote a refused sign-in for
	// each GitHub login, in lower case, for SignInRefusedEvery after it.
	SignInsRefused map[string]time.Time
}

// AliveEvery is how often the Hub notes that it's alive, waking to do so,
// so it knows to the minute how long it was down.
const AliveEvery = time.Minute

// A Target is what the pool keeps of one target. The Hub's own record of
// the target, with its GitHub connection, is the store's.
type Target struct {
	// Scope is what the target covers, which the node needs to tell its
	// own runs from others'.
	Scope node.Scope
	// RunnerApp is the target's runner version, as package runnerapp
	// last changed it, or zero until it has found one.
	RunnerApp runnerapp.Version
	// Lost is when the target became a lost target, or zero while the Hub
	// can see it on GitHub.
	Lost time.Time
}

// A Code is an enrollment code. Holding it is the approval for one
// machine to enroll: there's no queue of machines waiting in the Hub.
type Code struct {
	// ID names the code, as the Hub keys it and as code list and code
	// revoke show it: random, but not secret, so it enrolls nothing.
	ID string
	// Hash is the hash of the code's secret, which an enrollment is
	// matched by. The code the operator copies bundles the secret with
	// the Hub's URL and identity, and the core never sees any of them.
	Hash Hash
	// Made is when the Hub made it. It expires CodeLifetime later.
	Made time.Time
	// Used is the ID of the node that enrolled with it, or empty while
	// it's unused. A code enrolls one machine.
	Used string
	// Revoked reports that the operator revoked it before it was used.
	Revoked bool
}

// A Hash is the SHA-256 hash of an enrollment code's secret. The Hub
// keeps only the hash, so a copy of its database can't enroll a machine.
type Hash [sha256.Size]byte

// A Node is the Hub's record of one node.
type Node struct {
	// ID is the node's ID, which the Hub assigns when it enrolls: opaque
	// and permanent.
	ID string
	// Name is the node's name, which the Hub owns. It starts as the name
	// the machine asked for when it enrolled, and keeps to node.CheckName.
	Name string
	// Key is the node's public key, as it presented it when it enrolled.
	// The Hub checks each connection's signature against it, and a
	// tombstone keeps it.
	Key string
	// Target names the node's target, a Hub-owned setting, or is empty
	// if it has none.
	Target string
	// Connected reports that the node holds a live, authenticated
	// connection. A node that isn't connected shows as Last known, and
	// the Hub sends it nothing.
	Connected bool
	// ConnectedAt is when the Hub accepted the connection the node holds,
	// or zero if it isn't connected. An outbox entry that happened
	// before it is late.
	ConnectedAt time.Time
	// Dropped reports that the node's latest connection ended while the
	// Hub was running, so the gap until it reconnects is written to
	// history when it does. A connection that ends because the Hub goes
	// down leaves it unset: "Hub started" stands for every such gap.
	Dropped bool
	// Seen is the node's last seen: the last time the Hub heard anything
	// from it, pings included, or zero if it never has. A node that isn't
	// connected shows "Not connected since" it.
	Seen time.Time
	// Platform is the platform the node's agent stated when it last
	// connected, which picks the SHA-256 of its runner application: one
	// of runnerapp.Platforms, UnknownPlatform for any other, or empty if
	// it has never connected.
	Platform runnerapp.Platform
	// Mirror is the node as it last reported itself. Nothing else
	// changes it: the Hub never updates it ahead of a report.
	Mirror node.Node
	// Sent holds the IDs of the commands sent on the current connection
	// that haven't been replied to.
	Sent []string
	// NotConfirmed holds the IDs of the commands whose connection ended
	// before their reply. They show as "Not confirmed" until the node's
	// next report, and resending one with its ID is safe.
	NotConfirmed []string
	// Stored is the sequence number of the newest outbox entry the Hub
	// has stored.
	Stored int64
	// Removing reports that the node's removal has started, which can't
	// be undone.
	Removing bool
	// Remove is the Remove sent to the node, sent again, with the same
	// ID, on each connection that replaces the one it went out on.
	Remove node.Remove
	// Removed is when the node became a tombstone, or zero if it isn't
	// one. A tombstone's key is revoked for good and its name is freed,
	// and its history is kept for audit.
	Removed time.Time
	// Replaced holds when the node's connection was replaced by a newer
	// one, oldest first: the latest replacement, and those within
	// KeyInUseWindow before it.
	Replaced []time.Time
	// Refused is when the Hub began refusing the node's connections for
	// their protocol version, and Mismatch the versions, or zero while it
	// accepts them. WrongKey is when it began refusing connections that
	// name the node without proving its key, or zero. Each is an episode
	// that lasts until the Hub accepts a connection, and writes one entry,
	// not one per retry. Another mismatch starts a new version episode.
	Refused  time.Time
	Mismatch VersionMismatch
	WrongKey time.Time
	// KeyInUse is when the Hub raised the notice "This node's key is in
	// use on two machines", or zero if it hasn't. The notice stays: the
	// operator removes the node and enrolls each copy.
	KeyInUse time.Time
}

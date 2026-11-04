// Package registration is the pure core of the Hub's registration
// records: every runner the Hub mints, from before GitHub registers it
// until its registration is gone. The decisions behind it are in "Define
// node states and the drain/retirement contract"
// and the runner-names section of "Define the registration scope model
// and labels".
//
// It's the Hub's half of the pure core, beside the agent's half in
// package node. Everything here is a function of (records, event, now).
// The Hub applies an event for everything it learns from its nodes and
// from GitHub, saves the Records that come back, and only then carries
// out the effects, so no JIT config leaves the Hub before its record is
// saved. Nothing reads the clock, sleeps or does I/O, and randomness
// comes in with the events that need it.
package registration

import (
	"fmt"
	"time"
)

// Records are the Hub's records of the runners it minted, plus what
// minting needs to know of each node's connection. The zero Records hold
// nothing and no node is connected.
type Records struct {
	// Runners are in the order they were requested. Each name is unique,
	// ignoring case.
	Runners []Record
	// Connected holds each node that's connected, by node ID.
	Connected map[string]Connection
}

// A Connection is a node's connection to the Hub, as minting sees it.
type Connection struct {
	// Reconciled reports that the start-up reconcile has begun, which is
	// when the Hub starts minting for the node.
	Reconciled bool
	// Removing reports that the node is being removed, so the Hub mints
	// nothing more for it. It lasts while the node is connected: once
	// it isn't, it's tombstoned and never connects again.
	Removing bool
}

// A Record is the Hub's record of one runner it minted.
type Record struct {
	// Name is `<node>-<slot>-<4 random characters>`, where node is the
	// node's name when the runner was requested.
	Name string
	// Node is the ID of the node it was minted for, and Slot its slot.
	Node string
	Slot int
	// Target is the target it registers in, as the Hub keys it.
	Target string
	// ID is GitHub's id for the registration, or 0 until the Hub has it.
	ID    int64
	Phase Phase
	// Lost reports that its target is lost, so the Hub can no longer
	// delete it. It's dropped once its node no longer hosts it, and until
	// then it's named in the Hub's "removed" answer to the node, which
	// stops it itself.
	Lost bool
	// Revive is the phase a stale record goes back to if its target is
	// lost: for one that went stale because its node is being removed,
	// which a tombstone still is, the phase it would have now, Delivering
	// or Live, since its node may still host it. A lost target then keeps
	// it until its node's report shows it gone. For every other stale
	// record it's Waiting, and a lost target drops it.
	Revive Phase
	// Retiring reports that the Hub is retiring it, after an edit to its
	// node's labels or target.
	Retiring bool
	// Deleting reports that a DELETE is out, and Relay that the node
	// asked for it and waits for the result.
	Deleting, Relay bool
	// Attempts counts the Hub's own failed DELETEs, and RetryAt is when
	// to try again, or zero if no retry is due.
	Attempts int
	RetryAt  time.Time
}

// A Phase is how far the Hub is with a runner.
type Phase int

const (
	// Waiting is requested, and waits for its node's start-up reconcile
	// before it's minted.
	Waiting Phase = iota
	// Minting has asked GitHub for its JIT config.
	Minting
	// Abandoned was minting when the connection that asked for it ended,
	// so it's deleted, not granted, once GitHub answers.
	Abandoned
	// Delivering has been granted to its node, and is on its way: the
	// node hasn't reported it yet, on the connection it was granted on.
	Delivering
	// Live is in its node's latest report, or was granted on a connection
	// that has since ended. Either way the node's next report decides.
	Live
	// Stale is a registration whose node no longer hosts it, or whose
	// node is being removed. The Hub deletes it, retrying until GitHub
	// answers 204, or 404 while the Hub can see the target. One with no
	// ID waits for the periodic comparison to find it by name.
	Stale

	// dropped is a record on its way out of Records at the end of Apply.
	dropped Phase = -1
)

func (p Phase) String() string {
	switch p {
	case Waiting:
		return "waiting"
	case Minting:
		return "minting"
	case Abandoned:
		return "abandoned"
	case Delivering:
		return "delivering"
	case Live:
		return "live"
	case Stale:
		return "stale"
	}
	return fmt.Sprintf("Phase(%d)", int(p))
}

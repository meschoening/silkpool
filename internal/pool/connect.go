package pool

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/runnerapp"
)

// A node's connections that keep replacing each other raise the notice
// "This node's key is in use on two machines": KeyInUseReplacements
// replacements within KeyInUseWindow. A cloned machine's copies keep
// taking the connection back from each other, each within a reconnect's
// backoff of at most a minute, while a network change replaces a
// connection once.
const (
	KeyInUseWindow       = 5 * time.Minute
	KeyInUseReplacements = 3
)

// UnknownPlatform is the platform the Hub keeps for a node whose agent
// stated one Silkpool doesn't support, in place of whatever it stated.
// The connection is accepted all the same. Its runner version carries no
// SHA-256 for it, and the Hub shows it as unknown, from "Show a node's
// platform as unknown when the Hub doesn't know it".
const UnknownPlatform runnerapp.Platform = "unknown"

// known returns p if Silkpool supports it, and otherwise UnknownPlatform.
func known(p runnerapp.Platform) runnerapp.Platform {
	if !slices.Contains(runnerapp.Platforms, p) {
		return UnknownPlatform
	}
	return p
}

// A VersionMismatch refuses an agent whose protocol version the Hub
// doesn't accept. The Hub accepts its own version and the one before, so
// the Hub is upgraded first, then its nodes.
type VersionMismatch struct{ Agent, Hub int }

func (e VersionMismatch) Error() string {
	fix := "upgrade the agent"
	if e.Agent > e.Hub {
		fix = "upgrade the Hub"
	}
	return fmt.Sprintf("the agent's protocol version is %d and the Hub's is %d: %s", e.Agent, e.Hub, fix)
}

// connected accepts n's new connection, which replaces any it holds,
// unless n is a tombstone or the Hub refuses its version. An accepted
// connection ends a refusal episode, writes the gap since n's last one
// ended, if it ended while the Hub ran, and gets n's Hub settings.
func (a *applying) connected(n *Node, e Connected) {
	if !n.Removed.IsZero() {
		a.emit(AnswerRemoved{Node: n.ID})
		return
	}
	if err := a.p.accepts(e.Protocol); err != nil {
		a.emit(AnswerRefused{Node: n.ID, Reason: err})
		if m := err.(VersionMismatch); n.Mismatch != m {
			n.Refused, n.Mismatch = a.now, m
			a.store(Entry{Node: n.ID, At: a.now, What: ConnectionRefused{Reason: m}})
		}
		return
	}
	n.Refused, n.Mismatch, n.WrongKey = time.Time{}, VersionMismatch{}, time.Time{}
	if n.Dropped {
		a.store(Entry{Node: n.ID, At: a.now, What: ConnectionGap{Since: n.Seen}})
		n.Dropped = false
	}
	// A replaced connection's commands lose their replies, but the node
	// stays connected, and a removal goes on.
	replaced := n.Connected
	a.unconfirm(n)
	n.Connected, n.ConnectedAt, n.Seen, n.Platform = true, a.now, a.now, known(e.Platform)
	if replaced {
		a.replaced(n)
	}
	a.emit(Send{Node: n.ID, Message: a.p.settings(*n)})
	if replaced && n.Removing {
		a.send(n, n.Remove.ID, n.Remove)
	}
}

// dropped ends n's connection, keeping when it last heard the node as
// its last seen. The gap is written when n reconnects.
func (a *applying) dropped(n *Node, e Disconnected) {
	if !n.Connected {
		return
	}
	if e.Heard.After(n.Seen) {
		n.Seen = e.Heard
	}
	n.Dropped = !n.Removing
	a.disconnected(n)
}

// ErrWrongKey refuses a connection that names a node but doesn't prove
// itself with the node's key.
var ErrWrongKey = errors.New("the connection didn't prove itself with the node's key")

// keyRefused refuses a connection that named n without proving its key,
// and writes the refusal into history at the start of an episode.
func (a *applying) keyRefused(n *Node) {
	a.emit(AnswerRefused{Node: n.ID, Reason: ErrWrongKey})
	if n.WrongKey.IsZero() {
		n.WrongKey = a.now
		a.store(Entry{Node: n.ID, At: a.now, What: ConnectionRefused{Reason: ErrWrongKey}})
	}
}

// accepts returns the VersionMismatch that refuses an agent at protocol
// version agent, or nil.
func (p Pool) accepts(agent int) error {
	if agent != p.Protocol && agent != p.Protocol-1 {
		return VersionMismatch{Agent: agent, Hub: p.Protocol}
	}
	return nil
}

// replaced records that n's connection was replaced by a newer one, and
// raises the notice if replacements keep coming.
func (a *applying) replaced(n *Node) {
	n.Replaced = append(slices.DeleteFunc(slices.Clone(n.Replaced), func(t time.Time) bool {
		return a.now.Sub(t) >= KeyInUseWindow
	}), a.now)
	if len(n.Replaced) >= KeyInUseReplacements && n.KeyInUse.IsZero() {
		n.KeyInUse = a.now
	}
}

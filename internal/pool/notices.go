package pool

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// The Hub's own notices are in "Define Silkpool's events, audit trail,
// notices and redaction".
// They live on the Hub, while a node's own live on the node and reach
// the Hub in its mirror. Each is about a condition, so it can't be
// dismissed, and clears itself when the condition ends. Raising or
// clearing one writes nothing, except that a lost target's entries
// record it.

// A Notice is a message the Hub shows about something the operator may
// need to act on, beside a node or a target. It never changes a node's
// state.
type Notice struct {
	Kind NoticeKind
	// Node is the node it's about, or empty for a target's.
	Node string
	// Target is the target it's about, for NoticeTargetLost.
	Target string
	// Since is when the condition began, or zero if the Hub doesn't know.
	Since time.Time
	// Detail is the plain detail: the refusal, for NoticeVersionRefused.
	Detail string
}

// A NoticeKind is what one of the Hub's notices is about.
type NoticeKind int

const (
	// NoticeKeyInUse: the node's key is in use on two machines, whose
	// connections keep replacing each other. It clears only when the
	// node is removed: the operator removes it and enrolls each copy.
	NoticeKeyInUse NoticeKind = iota + 1
	// NoticeTargetLost: the Hub can no longer see the target on GitHub.
	// It clears when the Hub sees it again.
	NoticeTargetLost
	// NoticeVersionRefused: the Hub refuses the node's connections for
	// its agent's protocol version. It clears on the next connection the
	// Hub accepts.
	NoticeVersionRefused
	// NoticeNoTarget: the node has no target, so its slots get no
	// runners. It clears when the node is given one.
	NoticeNoTarget
)

func (k NoticeKind) String() string {
	switch k {
	case NoticeKeyInUse:
		return "key in use"
	case NoticeTargetLost:
		return "target lost"
	case NoticeVersionRefused:
		return "connection refused for version"
	case NoticeNoTarget:
		return "no target"
	}
	return fmt.Sprintf("NoticeKind(%d)", int(k))
}

// Condition reports whether a notice of kind k is about a condition, so
// it can't be dismissed. Every one of the Hub's own is.
func (k NoticeKind) Condition() bool { return true }

// Notices returns the Hub's notices, as p's state calls for them: each
// node's, in the order the nodes enrolled, then each lost target's, by
// key. A tombstone has none.
func (p Pool) Notices() []Notice {
	var notices []Notice
	for _, n := range p.Nodes {
		if !n.Removed.IsZero() {
			continue
		}
		if !n.KeyInUse.IsZero() {
			notices = append(notices, Notice{Kind: NoticeKeyInUse, Node: n.ID, Since: n.KeyInUse})
		}
		if !n.Refused.IsZero() {
			notices = append(notices, Notice{Kind: NoticeVersionRefused, Node: n.ID, Since: n.Refused, Detail: n.Mismatch.Error()})
		}
		if n.Target == "" {
			notices = append(notices, Notice{Kind: NoticeNoTarget, Node: n.ID})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(p.Targets)) {
		if lost := p.Targets[key].Lost; !lost.IsZero() {
			notices = append(notices, Notice{Kind: NoticeTargetLost, Target: key, Since: lost})
		}
	}
	return notices
}

// targetLost marks the target lost, and writes it, unless it's already
// lost. A target the pool doesn't hold is ignored.
func (a *applying) targetLost(key string) {
	t, ok := a.p.Targets[key]
	if !ok || !t.Lost.IsZero() {
		return
	}
	t.Lost = a.now
	a.putTarget(key, t)
	a.store(Entry{At: a.now, Origin: hubOrigin, What: LostTarget{Target: key}})
}

// targetRegained clears the target's lost mark, and writes it, if it was
// lost.
func (a *applying) targetRegained(key string) {
	t, ok := a.p.Targets[key]
	if !ok || t.Lost.IsZero() {
		return
	}
	t.Lost = time.Time{}
	a.putTarget(key, t)
	a.store(Entry{At: a.now, Origin: hubOrigin, What: RegainedTarget{Target: key}})
}

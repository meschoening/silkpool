package pool

import (
	"maps"
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/runnerapp"
)

// Apply applies e to p at now. It returns the Pool that results, which
// the Hub saves first, with the history entries and the prune it asks
// for, and the effects it must then carry out, in order, ending with a
// WakeAt once the Hub has started.
//
// Every Apply first drops the codes CodeRetention old or older, and the
// refused sign-ins that no longer hold back an entry. Then it notes the
// Hub is alive, if it last did AliveEvery ago, and prunes history, if
// it's due.
func (p Pool) Apply(e Event, now time.Time) (Pool, []Effect) {
	p.Nodes = slices.Clone(p.Nodes)
	if due := func(c Code) bool { return !now.Before(c.Made.Add(CodeRetention)) }; slices.ContainsFunc(p.Codes, due) {
		p.Codes = slices.DeleteFunc(slices.Clone(p.Codes), due)
	}
	p.forgetSignIns(now)
	a := &applying{p: &p, now: now}
	a.apply(e)
	if !p.Alive.IsZero() && !now.Before(p.Alive.Add(AliveEvery)) {
		p.Alive = now
	}
	a.prune()
	if at := p.wake(); !at.IsZero() {
		a.emit(WakeAt{At: at})
	}
	return p, a.effects
}

// apply applies e.
func (a *applying) apply(e Event) {
	p := a.p
	switch e := e.(type) {
	case Started:
		a.started(e)
		return
	case Tick:
		return
	case SignIn:
		a.store(Entry{At: a.now, Origin: node.Origin{Surface: node.Dashboard, Login: e.Login}, What: SignedIn{Address: e.Address}})
		return
	case RefuseSignIn:
		a.refuseSignIn(e)
		return
	case SetKeepHistory:
		a.setKeepHistory(e)
		return
	case MakeCode:
		p.Codes = append(slices.Clone(p.Codes), Code{ID: e.ID, Hash: e.Hash, Made: a.now})
		a.store(Entry{At: a.now, Origin: e.Origin, What: CodeMade{Code: e.ID}})
		return
	case RevokeCode:
		a.revoke(e)
		return
	case Enroll:
		a.enroll(e)
		return
	case RunnerAppChanged:
		a.runnerAppChanged(e)
		return
	case AddTarget:
		a.addTarget(e)
		return
	case DropTarget:
		a.dropTarget(e)
		return
	case TargetLost:
		a.targetLost(e.Target)
		return
	case TargetRegained:
		a.targetRegained(e.Target)
		return
	}
	n := a.find(e)
	if n == nil {
		return
	}
	switch e.(type) {
	case Reported, Replied, Delivered:
		if n.Connected {
			n.Seen = a.now
		}
	}
	switch e := e.(type) {
	case Connected:
		a.connected(n, e)
	case KeyRefused:
		a.keyRefused(n)
	case Disconnected:
		a.dropped(n, e)
	case Reported:
		if n.Connected {
			n.Mirror, n.NotConfirmed = e.Report, nil
		}
	case Issued:
		switch c := e.Command.(type) {
		case node.Command:
			if _, budget := c.Event.(availability.SetBudget); budget && n.mac() {
				a.emit(ChangeRefused{Reason: node.ErrNoBudget})
				break
			}
			a.send(n, c.ID, c)
		case node.Dismiss:
			a.send(n, c.ID, c)
		}
	case Replied:
		n.Sent = without(n.Sent, e.Reply.ID)
		n.NotConfirmed = without(n.NotConfirmed, e.Reply.ID)
	case Remove:
		a.remove(n, e)
	case Confirmed:
		if n.Removing {
			a.disconnected(n)
		}
	case Delivered:
		a.delivered(n, e)
	case Rename:
		a.rename(n, e)
	case Retarget:
		a.retarget(n, e)
	}
}

// wake returns when the Hub next has something to do: drop a code, note
// it's alive, or prune its history. It's zero before the Hub first
// starts, if it holds no code.
func (p Pool) wake() time.Time {
	var next time.Time
	later := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	for _, c := range p.Codes {
		later(c.Made.Add(CodeRetention))
	}
	if !p.Alive.IsZero() {
		later(p.Alive.Add(AliveEvery))
	}
	if !p.PruneAt.IsZero() {
		later(p.PruneAt)
	}
	return next
}

// started starts the Hub: every node's connection ended with the Hub,
// and a Hub that has run before writes how long it was down.
func (a *applying) started(e Started) {
	p := a.p
	p.Protocol = e.Protocol
	if !p.Alive.IsZero() {
		a.store(Entry{At: a.now, What: HubStarted{Alive: p.Alive}})
	}
	for i := range p.Nodes {
		n := &p.Nodes[i]
		// The Hub heard a connected node's pings until it went down.
		if n.Connected && p.Alive.After(n.Seen) {
			n.Seen = p.Alive
		}
		a.disconnected(n)
	}
	p.Alive, p.PruneAt = a.now, a.now
}

// applying is one Apply under way.
type applying struct {
	p       *Pool
	now     time.Time
	effects []Effect
}

func (a *applying) emit(e Effect) { a.effects = append(a.effects, e) }

// find returns the node e is about, or nil.
func (a *applying) find(e Event) *Node {
	var id string
	switch e := e.(type) {
	case Connected:
		id = e.Node
	case Disconnected:
		id = e.Node
	case KeyRefused:
		id = e.Node
	case Reported:
		id = e.Node
	case Issued:
		id = e.Node
	case Replied:
		id = e.Node
	case Remove:
		id = e.Node
	case Confirmed:
		id = e.Node
	case Delivered:
		id = e.Node
	case Rename:
		id = e.Node
	case Retarget:
		id = e.Node
	}
	for i := range a.p.Nodes {
		if n := &a.p.Nodes[i]; n.ID == id {
			return n
		}
	}
	return nil
}

// disconnected ends n's connection, if it has one. Its commands without
// a reply are no longer confirmed, and a removing node is tombstoned.
func (a *applying) disconnected(n *Node) {
	if !n.Connected {
		return
	}
	n.Connected, n.ConnectedAt = false, time.Time{}
	a.unconfirm(n)
	if n.Removing {
		a.tombstone(n)
	}
}

// unconfirm marks n's commands without a reply as not confirmed.
func (a *applying) unconfirm(n *Node) {
	n.NotConfirmed = append(slices.Clone(n.NotConfirmed), n.Sent...)
	n.Sent = nil
}

// mac reports whether n's agent last stated the macOS platform Silkpool
// supports. A Mac has no budget (ADR 0014), so the Hub never sends it the
// budget command. A node whose platform is unknown isn't one, whatever it
// stated: the Hub draws nothing from that text, and the node's own check
// refuses a budget on a Mac.
func (n Node) mac() bool { return n.Platform == runnerapp.OSXARM64 }

// send sends n a command, if it's connected, and otherwise refuses it.
func (a *applying) send(n *Node, id string, message node.Event) {
	if !n.Connected {
		a.emit(NotSent{Node: n.ID, ID: id})
		return
	}
	a.emit(Send{Node: n.ID, Message: message})
	n.Sent = append(without(n.Sent, id), id)
}

// remove starts n's removal: the Hub deletes its registrations, and
// sends Remove if it's connected, or tombstones it at once if it isn't.
func (a *applying) remove(n *Node, e Remove) {
	if n.Removing {
		return
	}
	n.Removing, n.Remove = true, node.Remove{ID: e.ID, Origin: e.Origin}
	a.store(Entry{Node: n.ID, At: a.now, Origin: e.Origin, What: RemovalStarted{}})
	a.emit(DeleteRegistrations{Node: n.ID})
	if n.Connected {
		a.send(n, e.ID, n.Remove)
	} else {
		a.tombstone(n)
	}
}

// tombstone removes n, which isn't connected, for good.
func (a *applying) tombstone(n *Node) {
	n.Removed = a.now
	a.store(Entry{Node: n.ID, At: a.now, Origin: hubOrigin, What: Tombstoned{}})
}

// delivered stores each of the entries the Hub hasn't stored yet, and
// acknowledges them all. Entries come in order and are delivered at
// least once, so one whose Seq is already stored is a duplicate.
func (a *applying) delivered(n *Node, e Delivered) {
	if !n.Connected {
		return
	}
	for _, entry := range e.Entries {
		if entry.Seq > n.Stored {
			a.emit(Store{Entry: n.fromOutbox(entry)})
			n.Stored = entry.Seq
			if r, ok := entry.What.(node.RunnerVersionRefused); ok {
				a.emit(RunnerVersionRefused{Node: n.ID, Target: n.Target, Version: r.Version})
			}
		}
	}
	a.emit(Acknowledge{Node: n.ID, Through: n.Stored})
}

// runnerAppChanged keeps the target's new runner version, and sends it
// to each of the target's connected nodes in their Hub settings.
func (a *applying) runnerAppChanged(e RunnerAppChanged) {
	t, ok := a.p.Targets[e.Target]
	if !ok {
		return
	}
	a.store(Entry{At: a.now, Origin: hubOrigin, What: RunnerVersionChanged{Target: e.Target, From: t.RunnerApp.Version, To: e.Version.Version}})
	t.RunnerApp = e.Version
	a.putTarget(e.Target, t)
	for _, n := range a.p.Nodes {
		if n.Connected && n.Target == e.Target {
			a.emit(Send{Node: n.ID, Message: a.p.settings(n)})
		}
	}
}

// addTarget keeps the target e adds, or its new scope, and writes it if
// it's new.
func (a *applying) addTarget(e AddTarget) {
	t, held := a.p.Targets[e.Target]
	t.Scope = e.Scope
	a.putTarget(e.Target, t)
	if !held {
		a.store(Entry{At: a.now, Origin: e.Origin, What: AddedTarget{Target: e.Target}})
	}
}

// dropTarget forgets the target e removes, and writes it, if the pool
// holds it.
func (a *applying) dropTarget(e DropTarget) {
	if _, ok := a.p.Targets[e.Target]; ok {
		a.p.Targets = maps.Clone(a.p.Targets)
		delete(a.p.Targets, e.Target)
		a.store(Entry{At: a.now, Origin: e.Origin, What: RemovedTarget{Target: e.Target}})
	}
}

// putTarget keeps t as the pool's target key.
func (a *applying) putTarget(key string, t Target) {
	a.p.Targets = maps.Clone(a.p.Targets)
	if a.p.Targets == nil {
		a.p.Targets = map[string]Target{}
	}
	a.p.Targets[key] = t
}

// settings returns n's Hub settings: its target, what the target covers,
// and the target's runner version with the SHA-256 for n's platform. A
// node without a target, or whose target the pool doesn't hold, gets
// settings that say so. A version has a SHA-256 for every platform
// Silkpool supports, so only a node whose platform is unknown gets no
// version: it has nothing to install, and its slots fail as with no
// install, as Runner application unavailable.
func (p Pool) settings(n Node) node.HubSettings {
	t, ok := p.Targets[n.Target]
	if !ok {
		return node.HubSettings{Target: n.Target}
	}
	s := node.HubSettings{Target: n.Target, Scope: t.Scope}
	if sum := t.RunnerApp.SHA256[n.Platform]; sum != "" {
		s.RunnerApp = node.RunnerApp{Version: t.RunnerApp.Version, SHA256: sum}
	}
	return s
}

// without returns ids without id.
func without(ids []string, id string) []string {
	return slices.DeleteFunc(slices.Clone(ids), func(s string) bool { return s == id })
}

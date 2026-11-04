package registration

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/node"
)

// Apply applies e to r at now. It returns the Records that result, which
// the Hub saves first, and the effects it must then carry out, in order,
// ending with a WakeAt if a retry is due later.
func (r Records) Apply(e Event, now time.Time) (Records, []Effect) {
	r.Runners = slices.Clone(r.Runners)
	r.Connected = maps.Clone(r.Connected)
	if r.Connected == nil {
		r.Connected = map[string]Connection{}
	}
	a := &applying{r: &r, now: now}
	a.record(e)
	a.settle()
	if at := r.deadline(); !at.IsZero() {
		a.emit(WakeAt{At: at})
	}
	r.Runners = slices.DeleteFunc(r.Runners, func(rec Record) bool { return rec.Phase == dropped })
	return r, a.effects
}

// applying is one Apply under way.
type applying struct {
	r       *Records
	now     time.Time
	effects []Effect
}

func (a *applying) emit(e Effect) { a.effects = append(a.effects, e) }

// find returns the record of runner, or nil.
func (a *applying) find(runner string) *Record {
	for i := range a.r.Runners {
		if rec := &a.r.Runners[i]; rec.Name == runner && rec.Phase != dropped {
			return rec
		}
	}
	return nil
}

// record records what e reports.
func (a *applying) record(e Event) {
	switch e := e.(type) {
	case Started:
		a.started()
	case Connected:
		removing := a.r.Connected[e.Node].Removing
		a.disconnected(e.Node)
		a.r.Connected[e.Node] = Connection{Removing: removing}
	case Disconnected:
		a.disconnected(e.Node)
	case Reported:
		a.reported(e)
	case RunnerRequested:
		a.requested(e)
	case Generated:
		if rec := a.find(e.Runner); rec != nil && (rec.Phase == Minting || rec.Phase == Abandoned) {
			a.generated(rec, e.ID)
		}
	case GenerateFailed:
		if rec := a.find(e.Runner); rec != nil && (rec.Phase == Minting || rec.Phase == Abandoned) {
			a.generateFailed(rec, e)
		}
	case RetireRequested:
		a.retireRequested(e)
	case RunnersOutdated:
		a.outdated(e.Node)
	case Deleted:
		if rec := a.find(e.Runner); rec != nil && rec.Deleting {
			a.deleted(rec, e)
		}
	case NameTaken:
		if rec := a.find(e.Runner); rec != nil && rec.Phase == Minting {
			rec.Name = a.name(rec.Name[:strings.LastIndexByte(rec.Name, '-')], e.Random)
			a.generate(rec)
		} else if rec != nil && rec.Phase == Abandoned {
			rec.Phase = dropped
		}
	case TargetLost:
		a.targetLost(e.Target)
	case Removing:
		a.removing(e.Node)
	case Listed:
		a.listed(e)
	}
}

// started recovers the records after the Hub starts: no node is
// connected, a mint whose answer was lost goes stale, and a DELETE whose
// answer was lost is retried.
func (a *applying) started() {
	for id := range a.r.Connected {
		a.disconnected(id)
	}
	for i := range a.r.Runners {
		rec := &a.r.Runners[i]
		if rec.Phase == Abandoned {
			a.stale(rec)
		}
		if rec.Deleting {
			rec.Deleting = false
			if rec.Phase == Stale || rec.Retiring {
				rec.RetryAt = a.now
			}
		}
	}
}

// disconnected ends node's connection, if it has one: what it asked for
// can no longer be handed over, and what was on its way waits for its
// next report.
func (a *applying) disconnected(node string) {
	delete(a.r.Connected, node)
	for i := range a.r.Runners {
		rec := &a.r.Runners[i]
		if rec.Node != node {
			continue
		}
		rec.Relay = false
		switch rec.Phase {
		case Waiting:
			rec.Phase = dropped
		case Minting:
			rec.Phase = Abandoned
		case Delivering:
			rec.Phase = Live
		}
		if rec.Revive == Delivering {
			rec.Revive = Live
		}
	}
}

// reported reconciles the node's records against its report: every
// registration it doesn't host, unless still on its way, goes stale.
// The first report on a connection starts its reconcile, and then the
// requests that waited for it are minted.
func (a *applying) reported(e Reported) {
	c, ok := a.r.Connected[e.Node]
	if !ok {
		return
	}
	var hosted []string
	for _, s := range e.Report.Slots {
		if s.Phase == node.Hosting {
			hosted = append(hosted, s.Runner.Name)
		}
	}
	for i := range a.r.Runners {
		rec := &a.r.Runners[i]
		if rec.Node != e.Node {
			continue
		}
		switch {
		case rec.Phase == Stale && rec.Revive == Delivering && slices.Contains(hosted, rec.Name):
			rec.Revive = Live
		case rec.Phase != Delivering && rec.Phase != Live:
		case slices.Contains(hosted, rec.Name):
			rec.Phase = Live
		case rec.Phase == Live:
			a.stale(rec)
		}
	}
	if c.Reconciled {
		return
	}
	c.Reconciled = true
	a.r.Connected[e.Node] = c
	a.stopLost(func(rec Record) bool { return rec.Node == e.Node && slices.Contains(hosted, rec.Name) })
	for i := range a.r.Runners {
		if rec := &a.r.Runners[i]; rec.Node == e.Node && rec.Phase == Waiting {
			a.generate(rec)
		}
	}
}

func (a *applying) requested(e RunnerRequested) {
	c, ok := a.r.Connected[e.Node]
	if !ok || c.Removing {
		return
	}
	prefix := e.NodeName + "-" + strconv.Itoa(e.Slot)
	a.r.Runners = append(a.r.Runners, Record{Name: a.name(prefix, e.Random), Node: e.Node, Slot: e.Slot, Target: e.Target})
	if c.Reconciled {
		a.generate(&a.r.Runners[len(a.r.Runners)-1])
	}
}

// retireRequested DELETEs the registration a node asked to retire, and
// relays the result. One the Hub holds no record of for the node is
// gone, since a record is dropped only once its registration is.
func (a *applying) retireRequested(e RetireRequested) {
	if _, ok := a.r.Connected[e.Node]; !ok {
		return
	}
	rec := a.find(e.Runner)
	if rec == nil || rec.Node != e.Node || rec.ID == 0 {
		a.emit(Retired{Node: e.Node, Runner: e.Runner, Status: 404, TargetVisible: true})
		return
	}
	rec.Relay = true
	a.delete(rec)
	if rec.Phase == Delivering && !rec.Lost {
		// It arrived, and the node turned it down without hosting it, so
		// the Hub finishes the DELETE itself.
		a.stale(rec)
	}
}

// generated records rec's id, and grants it if the connection that
// asked for it is still there and its target isn't lost.
func (a *applying) generated(rec *Record, id int64) {
	rec.ID = id
	switch {
	case rec.Phase == Abandoned:
		a.stale(rec)
	case rec.Lost:
		a.emit(GrantFailed{Node: rec.Node, Slot: rec.Slot, Reason: node.GitHubRefused})
		a.stale(rec)
	default:
		rec.Phase = Delivering
		a.emit(Granted{Node: rec.Node, Slot: rec.Slot, Runner: rec.Name})
		if rec.Retiring {
			a.delete(rec)
		}
	}
}

// generateFailed tells the node why its runner couldn't be minted. With
// no answer, or a 5xx, the registration may exist all the same, so the
// record goes stale for the comparison to find; otherwise it's dropped.
func (a *applying) generateFailed(rec *Record, e GenerateFailed) {
	unavailable := e.Status == 0 || e.Status == 429 || e.Status >= 500
	if rec.Phase == Minting {
		reason := node.GitHubRefused
		if unavailable {
			reason = node.GitHubUnavailable
		}
		a.emit(GrantFailed{Node: rec.Node, Slot: rec.Slot, Reason: reason, Detail: e.Detail})
	}
	if e.Status == 0 || e.Status >= 500 {
		a.stale(rec)
	} else {
		rec.Phase = dropped
	}
}

// outdated retires every runner of node in service, DELETE-first. One
// still being minted is retired as soon as it's granted.
func (a *applying) outdated(node string) {
	for i := range a.r.Runners {
		rec := &a.r.Runners[i]
		if rec.Node != node || rec.Lost || rec.Phase == Abandoned || rec.Phase == Stale || rec.Phase == dropped {
			continue
		}
		rec.Retiring, rec.Attempts = true, 0
		if rec.ID != 0 {
			a.delete(rec)
		}
	}
}

// stale marks rec's registration stale, and deletes it. One in a lost
// target can't be deleted, and its listener is gone, so it's dropped.
func (a *applying) stale(rec *Record) {
	if rec.Lost {
		rec.Phase = dropped
		return
	}
	rec.Phase, rec.Retiring, rec.Attempts = Stale, false, 0
	if rec.ID != 0 {
		a.delete(rec)
	}
}

// targetLost marks the target's records lost: the Hub sends no DELETE of
// its own for them again, has their nodes stop them, and drops each once
// its node no longer hosts it.
func (a *applying) targetLost(target string) {
	for i := range a.r.Runners {
		rec := &a.r.Runners[i]
		if rec.Target != target || rec.Phase == dropped {
			continue
		}
		rec.Lost, rec.Retiring, rec.RetryAt = true, false, time.Time{}
		switch {
		case rec.Phase == Stale && rec.Revive != Waiting:
			// Its node, being removed, may still host it, connected or
			// not, so its reports decide, as they would have.
			rec.Phase, rec.Revive = rec.Revive, Waiting
		case rec.Phase == Stale:
			rec.Phase = dropped
		}
	}
	a.stopLost(func(rec Record) bool { return rec.Target == target })
}

// LostRunners returns node's runners in lost targets that it may still
// host, in the order they were requested. The Hub can't delete them, so
// its "removed" answer names them, and the node stops them itself, as
// it does StopLostRunners. Their records stay until the node's report
// shows them gone, which a tombstone's never does.
func (r Records) LostRunners(node string) []string {
	var runners []string
	for _, rec := range r.Runners {
		if rec.Node == node && rec.Lost && (rec.Phase == Delivering || rec.Phase == Live) {
			runners = append(runners, rec.Name)
		}
	}
	return runners
}

// stopLost sends each connected node StopLostRunners for its lost
// runners that match and that it may host.
func (a *applying) stopLost(match func(Record) bool) {
	var nodes []string
	runners := map[string][]string{}
	for _, rec := range a.r.Runners {
		if _, ok := a.r.Connected[rec.Node]; !ok || !rec.Lost || !match(rec) ||
			rec.Phase != Delivering && rec.Phase != Live {
			continue
		}
		if runners[rec.Node] == nil {
			nodes = append(nodes, rec.Node)
		}
		runners[rec.Node] = append(runners[rec.Node], rec.Name)
	}
	for _, node := range nodes {
		a.emit(StopLostRunners{Node: node, Runners: runners[node]})
	}
}

// removing stops minting for node, and deletes every registration it
// has. One being minted is deleted once GitHub answers, never granted.
// A lost target's runner can't be deleted, and the node may still host
// it, connected or not, so its reports decide, as they did before: one
// on its way stays on its way. So do those of a runner whose target is
// lost later.
func (a *applying) removing(node string) {
	if c, ok := a.r.Connected[node]; ok {
		c.Removing = true
		a.r.Connected[node] = c
	}
	for i := range a.r.Runners {
		rec := &a.r.Runners[i]
		if rec.Node != node {
			continue
		}
		switch rec.Phase {
		case Waiting:
			rec.Phase = dropped
		case Minting:
			rec.Phase = Abandoned
		case Delivering, Live:
			if !rec.Lost {
				phase := rec.Phase
				a.stale(rec)
				rec.Revive = phase
			}
		}
	}
}

// listed compares the target's stale records with GitHub's list of its
// runners. A record with an id matches by id, and one without by name,
// ignoring case. Each match is deleted at once, and a stale record with
// no match is gone. Runners the Hub has no record of are never touched.
func (a *applying) listed(e Listed) {
	for i := range a.r.Runners {
		rec := &a.r.Runners[i]
		if rec.Target != e.Target || rec.Phase != Stale {
			continue
		}
		at := slices.IndexFunc(e.Runners, func(l Listing) bool {
			if rec.ID != 0 {
				return l.ID == rec.ID
			}
			return strings.EqualFold(l.Name, rec.Name)
		})
		switch {
		case at >= 0:
			rec.ID = e.Runners[at].ID
			a.delete(rec)
		case !rec.Deleting:
			rec.Phase = dropped
		}
	}
}

// delete DELETEs rec's registration, unless a DELETE is already out.
func (a *applying) delete(rec *Record) {
	if rec.Deleting {
		return
	}
	rec.Deleting, rec.RetryAt = true, time.Time{}
	a.emit(DeleteRunner{Runner: rec.Name, ID: rec.ID, Target: rec.Target})
}

// generate asks GitHub for rec's JIT config.
func (a *applying) generate(rec *Record) {
	rec.Phase = Minting
	a.emit(GenerateJITConfig{Node: rec.Node, Runner: rec.Name, Target: rec.Target})
}

// suffixChars are the characters of a name's random suffix, in order.
const suffixChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// name returns prefix and a suffix of 4 characters drawn from random,
// moved on to the next suffix while the name is held, ignoring case.
func (a *applying) name(prefix string, random uint32) string {
	const n = uint32(len(suffixChars))
	for v := random % (n * n * n * n); ; v = (v + 1) % (n * n * n * n) {
		suffix := []byte{suffixChars[v/(n*n*n)], suffixChars[v/(n*n)%n], suffixChars[v/n%n], suffixChars[v%n]}
		name := prefix + "-" + string(suffix)
		if !slices.ContainsFunc(a.r.Runners, func(rec Record) bool {
			return rec.Phase != dropped && strings.EqualFold(rec.Name, name)
		}) {
			return name
		}
	}
}

// deleted records the result of rec's DELETE. A registration is gone
// after a 204, or a 404 while the Hub can see the target. The node hears
// the result if it asked for it, and otherwise only a 204 of the Hub's
// own, as "stop runner X". The Hub retries its own DELETEs, except that
// a runner it's retiring that GitHub refuses (422) keeps its job. A lost
// target's runner the node turned down without hosting it can't be
// deleted, so it's gone once the node has its answer.
func (a *applying) deleted(rec *Record, e Deleted) {
	rec.Deleting = false
	own := rec.Phase == Stale || rec.Retiring
	relayed := rec.Relay
	if _, ok := a.r.Connected[rec.Node]; ok && (relayed || own && e.Status == 204) {
		a.emit(Retired{Node: rec.Node, Runner: rec.Name, Status: e.Status, TargetVisible: e.TargetVisible})
	}
	rec.Relay = false
	switch {
	case e.Status == 204 || e.Status == 404 && e.TargetVisible:
		rec.Phase = dropped
	case relayed && rec.Lost && rec.Phase == Delivering:
		rec.Phase = dropped
	case rec.Retiring && e.Status == 422:
		rec.Retiring = false
	case own:
		rec.Attempts++
		rec.RetryAt = a.now.Add(node.Backoff(rec.Attempts))
	}
}

// settle sends each DELETE whose retry is due.
func (a *applying) settle() {
	for i := range a.r.Runners {
		if rec := &a.r.Runners[i]; rec.retrying() && !a.now.Before(rec.RetryAt) {
			a.delete(rec)
		}
	}
}

// retrying reports whether rec has a retry of the Hub's own DELETE
// waiting.
func (rec Record) retrying() bool {
	return !rec.RetryAt.IsZero() && !rec.Deleting && rec.Phase != dropped
}

// deadline returns when the next retry is due, or zero if none is.
func (r Records) deadline() time.Time {
	var at time.Time
	for _, rec := range r.Runners {
		if rec.retrying() && (at.IsZero() || rec.RetryAt.Before(at)) {
			at = rec.RetryAt
		}
	}
	return at
}

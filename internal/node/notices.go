package node

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// A node's notices are in "Define Silkpool's events, audit trail,
// notices and redaction".
// They live on the node, and its reports mirror them to the Hub.

// ErrCondition refuses a dismissal of a notice about a condition, which
// clears itself when the condition ends.
var ErrCondition = errors.New("the notice is about a condition, and clears itself when the condition ends")

// A Notice is a message the node shows beside its state about something
// the operator may need to act on. It never changes the state. One about
// something that happened stays until it's dismissed, from any surface;
// one about a condition can't be dismissed, and clears itself when the
// condition ends. Raising one writes an outbox entry; clearing one
// doesn't.
type Notice struct {
	// ID is the sequence number of the outbox entry that raised it, so
	// it's unique over the node's life.
	ID     int64
	Kind   NoticeKind
	Raised time.Time
	// Slot is the slot it's about, or 0 if it's about the node.
	Slot int
	// Detail is the plain detail: the interrupted job's name, the failed
	// update's sentence, or the exposure check's path and fix. A
	// condition's detail follows the condition while it lasts.
	Detail string
	// Job is the interrupted job, with its run if the logs named one,
	// for NoticeInterruptedJob, so the tray can link the run without the
	// Hub.
	Job Job
}

// A NoticeKind is what a notice is about.
type NoticeKind int

const (
	// NoticeInterruptedJob: a job ended because its runner died while
	// holding it. It's about something that happened. A job Emergency
	// stop cancelled raises none, because the operator asked for it.
	NoticeInterruptedJob NoticeKind = iota + 1
	// NoticeUpdateFailed: updating the runner application fails while an
	// older install still works. It's a condition, which clears once the
	// update succeeds or the Hub moves on.
	NoticeUpdateFailed
	// NoticeSlotExposed: a slot's exposure check found something outside
	// the slot that it can open, while the node doesn't want the slot to
	// have a runner. It's a condition, which clears when a check passes
	// or the node wants the slot again, when the hit shows as the slot's
	// failure instead.
	NoticeSlotExposed
)

func (k NoticeKind) String() string {
	switch k {
	case NoticeInterruptedJob:
		return "interrupted job"
	case NoticeUpdateFailed:
		return "couldn't update the runner application"
	case NoticeSlotExposed:
		return "slot exposed"
	}
	return fmt.Sprintf("NoticeKind(%d)", int(k))
}

// Condition reports whether a notice of kind k is about a condition, so
// it can't be dismissed.
func (k NoticeKind) Condition() bool { return k != NoticeInterruptedJob }

// NoticeRaised records that the node raised a notice.
type NoticeRaised struct{ Notice Notice }

func (NoticeRaised) happening() {}

// raise raises notice, giving it its ID, and writes its entry.
func (a *applying) raise(notice Notice) {
	n := a.n
	notice.ID, notice.Raised = n.Seq+1, a.now
	n.Notices = append(n.Notices, notice)
	a.happened(NoticeRaised{Notice: notice})
}

// conditions returns the condition notices n's state calls for, without
// their IDs.
func (n Node) conditions() []Notice {
	var notices []Notice
	if d := n.updateFailed(); d != "" {
		notices = append(notices, Notice{Kind: NoticeUpdateFailed, Detail: d})
	}
	for _, s := range n.Slots {
		if s.Failure.Reason == SlotExposed && !n.wants(s) {
			notices = append(notices, Notice{Kind: NoticeSlotExposed, Slot: s.Number, Detail: s.Failure.Detail})
		}
	}
	return notices
}

// updateFailed returns the detail of the notice the node shows while
// updating its runner application fails and an older install still
// works, or "". A node with no install to fall back on shows the failure
// on its slots instead.
func (n Node) updateFailed() string {
	u := n.Update
	if n.Installed == "" || u.Failed == "" || u.Version != n.RunnerApp.Version {
		return ""
	}
	return fmt.Sprintf("Couldn't update the runner application to %s: %s. Retrying.", u.Version, u.Failed)
}

// settleNotices clears the condition notices whose conditions have
// ended, brings the details of the others up to date, and raises one for
// each condition that has begun.
func (a *applying) settleNotices() {
	n := a.n
	want := n.conditions()
	var kept []Notice
	for _, x := range n.Notices {
		if x.Kind.Condition() {
			i := slices.IndexFunc(want, func(c Notice) bool { return c.Kind == x.Kind && c.Slot == x.Slot })
			if i < 0 {
				continue
			}
			x.Detail = want[i].Detail
			want = slices.Delete(want, i, i+1)
		}
		kept = append(kept, x)
	}
	n.Notices = kept
	for _, c := range want {
		a.raise(c)
	}
}

// dismiss dismisses the notice e names: it's refused for a condition,
// and while the node is being removed, like every command but Emergency
// stop. A notice that's already gone, dismissed from another surface,
// has nothing left to dismiss.
func (a *applying) dismiss(e Dismiss) {
	n := a.n
	i := slices.IndexFunc(n.Notices, func(x Notice) bool { return x.ID == e.Notice })
	switch {
	case n.Removal.Started:
		a.replying.Refused = ErrRemoving
	case i < 0:
	case n.Notices[i].Kind.Condition():
		a.replying.Refused = ErrCondition
	default:
		n.Notices = slices.Delete(n.Notices, i, i+1)
	}
}

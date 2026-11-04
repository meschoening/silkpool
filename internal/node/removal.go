package node

import "github.com/meschoening/silkpool/internal/availability"

// remove checks the rules for e, and returns the availability event that
// starts the removal, or a Tick.
func (a *applying) remove(e Remove) availability.Event {
	n := a.n
	switch {
	case e.Local && e.Force:
		// Even during a removal, so a node whose Hub has gone can still
		// be wiped.
		a.startRemoval()
		n.Removal.Force = true
		return availability.EmergencyStop{}
	case n.Removal.Started:
		a.replying.Refused = ErrRemoving
	case e.Local && !n.Connected:
		a.replying.Refused = ErrHubUnreachable
	case e.Local:
		// The Hub answers by sending Remove back, and that's the one
		// replied to.
		a.replying = nil
		a.emit(RemoveAtHub{ID: e.ID, Origin: e.Origin})
	default:
		a.startRemoval()
		return availability.PauseIndefinitely{}
	}
	return availability.Tick{}
}

// startRemoval marks the node as being removed. The Hub mints nothing
// more for it, so a slot waiting for a registration stops waiting: one
// granted later is retired as a grant nobody asked for.
func (a *applying) startRemoval() {
	a.n.Removal.Started = true
	for i := range a.n.Slots {
		if s := &a.n.Slots[i]; s.Phase == Requesting {
			s.Phase = Free
		}
	}
}

// wipe wipes the node once its removal is done: every slot's account is
// deleted and the node may wipe. It may with Force, once it has heard
// "removed", or while connected once the Hub has stored its outbox, in
// which case it also confirms.
func (a *applying) wipe() {
	n := a.n
	if !n.Removal.Started {
		return
	}
	for _, s := range n.Slots {
		if s.Phase != Deleted {
			return
		}
	}
	confirm := n.Connected && len(n.Outbox) == 0
	if !n.Removal.Force && !n.Removal.Heard && !confirm {
		return
	}
	a.emit(Wipe{})
	if !n.Removal.Force && !n.Removal.Heard {
		a.emit(ConfirmRemoved{})
	}
	*n = Node{}
}

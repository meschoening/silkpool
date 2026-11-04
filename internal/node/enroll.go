package node

import "github.com/meschoening/silkpool/internal/availability"

// enroll applies e to a machine that isn't enrolled, and reports whether
// it's now enrolled. Commands but Enroll are refused, and everything else
// but the Hub's answer to Enroll is ignored.
func (a *applying) enroll(e Event) bool {
	switch e := e.(type) {
	case Enroll:
		if e.ID == "" {
			a.emit(Reply{At: a.now, Refused: ErrNoID})
			break
		}
		// The zone is checked now, so the Hub never saves a node the
		// agent would then turn down.
		if _, err := availability.New(e.Zone); err != nil {
			a.emit(Reply{ID: e.ID, At: a.now, Refused: err})
			break
		}
		a.emit(EnrollAtHub(e))
	case EnrollAccepted:
		n, err := New(e.Zone)
		if err != nil {
			// Only an answer that doesn't bring back the zone EnrollAtHub
			// carried gets here.
			a.emit(Reply{ID: e.ID, At: a.now, Refused: err})
			break
		}
		n.Target, n.Machine = e.Target, e.Machine
		n.Slots[0].Phase = Preparing
		*a.n = n
		a.replying = &replying{Reply: Reply{ID: e.ID, At: a.now}}
		a.emit(PrepareSlot{Slot: 1})
		return true
	case EnrollFailed:
		a.emit(Reply{ID: e.ID, At: a.now, Refused: e.Reason})
	default:
		a.notEnrolled(e)
	}
	return false
}

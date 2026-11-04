package node

import (
	"errors"
	"fmt"
	"slices"

	"github.com/meschoening/silkpool/internal/availability"
)

// A node's budget is in "Define how an operator bounds a node's slot
// resources",
// recorded as ADR 0014. Package availability keeps the budget the
// operator set, under the node's revision. The node checks it against
// its machine, and applies it to the slots: a CPU change and a raised
// memory budget at once, and a lowered memory budget once no slot holds
// a job, so a budget change never ends a running job.

// ErrNoBudget refuses a budget on macOS, which has none: launchd can't
// bound a job as a whole.
var ErrNoBudget = errors.New("macOS can't hold a node's slots to a memory or CPU budget. Use the slot limit to bound them")

// A Machine is what the node knows of the machine it runs on, which the
// agent tells it as it starts and as it enrolls: its size, which bounds
// a budget, and whether it's macOS, which can't hold one.
type Machine struct {
	// RAM is the machine's memory, in bytes, and CPUs its logical CPU
	// count.
	RAM  int64
	CPUs int
	// MacOS reports a Mac, which has no budget.
	MacOS bool
}

// budgets reports whether the node holds its slots to a budget on m: a
// machine whose size it knows, which isn't a Mac.
func (m Machine) budgets() bool { return !m.MacOS && m.RAM > 0 && m.CPUs > 0 }

// check refuses e on m: on a Mac, or for a half above the machine's RAM,
// in whole GB, or its logical CPU count.
func (m Machine) check(e availability.SetBudget) error {
	if m.MacOS {
		return ErrNoBudget
	}
	if most := m.RAM / availability.GB; e.SetMemory && e.Memory > most*availability.GB {
		return fmt.Errorf("%w: the memory budget can be at most %d GB, this machine's RAM", availability.ErrInvalid, most)
	}
	if e.SetCPUs && e.CPUs > m.CPUs {
		cpus := "CPUs"
		if m.CPUs == 1 {
			cpus = "CPU"
		}
		return fmt.Errorf("%w: the CPU budget can be at most %d %s, this machine's logical CPU count", availability.ErrInvalid, m.CPUs, cpus)
	}
	return nil
}

// ApplyBudget asks the agent to hold the slots to a budget, the whole of
// it: a half that's 0 is none. The agent applies one as it starts and
// as the node enrolls, and each time the budget in force changes.
type ApplyBudget availability.Budget

func (ApplyBudget) effect() {}

// PendingMemory returns the memory budget the operator set that's held
// until no slot holds a job, and true, while there's one.
func (n Node) PendingMemory() (int64, bool) {
	set := n.Availability.Budget.Memory
	return set, availability.Looser(n.InForce.Memory, set)
}

// holdsJob reports whether a slot's runner holds a job.
func (n Node) holdsJob() bool {
	return slices.ContainsFunc(n.Slots, func(s Slot) bool { return s.Phase == Hosting && s.Runner.Job != "" })
}

// budget brings the budget in force up to the one set: the CPUs at
// once, and memory at once unless it's lower than what's in force while
// a slot holds a job. Each change is one ApplyBudget, and so is the
// budget in force as the agent starts or the node enrolls, since a
// reboot loses it. A Mac, or a machine whose size the node doesn't know,
// gets none.
func (a *applying) budget(e Event) {
	n := a.n
	if !n.Machine.budgets() {
		return
	}
	set, next := n.Availability.Budget, n.InForce
	next.CPUs = set.CPUs
	if _, pending := n.PendingMemory(); !pending || !n.holdsJob() {
		next.Memory = set.Memory
	}
	_, started := e.(Started)
	_, enrolled := e.(EnrollAccepted)
	if next != n.InForce || started || enrolled {
		n.InForce = next
		a.emit(ApplyBudget(next))
	}
}

// outOfMemory marks the job s's runner ended with, which the OS ended a
// process of for memory, with the memory budget in force: 0 if the
// machine itself ran out.
func (a *applying) outOfMemory(s *Slot, runner string) {
	if s.Ended.Runner == runner && s.Ended.Name != "" {
		s.Ended.OutOfMemory, s.Ended.MemoryBudget = true, a.n.InForce.Memory
	}
}

package availability

import "fmt"

// A node's budget is in "Define how an operator bounds a node's slot
// resources",
// recorded as ADR 0014. It sits beside the slot limit, under the same
// revision: what the operator set, which package node applies to the
// slots, holding a lowered memory budget until no slot holds a job.

// GB is a gigabyte as a budget counts memory: 2³⁰ bytes.
const GB int64 = 1 << 30

// A Budget is the most memory and CPU a node's slots may use together.
// Each half is optional, and zero is none: a new node has neither.
type Budget struct {
	// Memory is in bytes, a whole number of GB.
	Memory int64
	// CPUs is a whole number of logical CPUs.
	CPUs int
}

// SetBudget sets either half of the budget, or both, or clears them: it
// sets memory to Memory if SetMemory, and the CPUs to CPUs if SetCPUs,
// where 0 clears the half, and leaves a half it doesn't set alone.
// Tightening a half, by setting it where it was unset or lowering it,
// always applies. Loosening one, by raising or clearing it, is refused
// unless Base is the node's current revision, as for the slot limit. The
// node checks the values against its machine before this package sees
// them.
type SetBudget struct {
	Base      int64
	SetMemory bool
	Memory    int64
	SetCPUs   bool
	CPUs      int
}

func (SetBudget) event() {}

// Looser reports whether a half of a budget at a is looser than one at
// b: none where b is set, or higher. 0 is none.
func Looser(a, b int64) bool { return b != 0 && (a == 0 || a > b) }

// apply returns b with e's halves set, or the reason e is refused: a
// value below 1, memory in part GB, or a loosening based on another
// revision than rev.
func (b Budget) apply(e SetBudget, rev int64) (Budget, error) {
	if e.SetMemory && (e.Memory < 0 || e.Memory%GB != 0) {
		return b, fmt.Errorf("%w: a memory budget is a whole number of GB, at least 1", ErrInvalid)
	}
	if e.SetCPUs && e.CPUs < 0 {
		return b, fmt.Errorf("%w: a CPU budget is a whole number of CPUs, at least 1", ErrInvalid)
	}
	next := b
	if e.SetMemory {
		next.Memory = e.Memory
	}
	if e.SetCPUs {
		next.CPUs = e.CPUs
	}
	loosens := Looser(next.Memory, b.Memory) || Looser(int64(next.CPUs), int64(b.CPUs))
	if loosens && e.Base != rev {
		return b, ErrStale
	}
	return next, nil
}

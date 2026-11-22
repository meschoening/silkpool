package winslot

import (
	"fmt"

	"github.com/meschoening/silkpool/internal/availability"
)

// NodeJobName is the node job's name: the named job object the agent
// keeps, open to SYSTEM alone, that every slot host nests its own job
// in, so that the node's budget bounds every slot together (ADR 0014).
const NodeJobName = `Global\silkpool-node`

// SlotJob is the name of the job object the slot host of the slot
// service named service holds its listener in. The host makes it, open
// to SYSTEM alone, nested in the node job, and it's gone once the host
// ends. The agent opens it by name to tell which slot a process is in.
func SlotJob(service string) string { return `Global\` + service }

// FullRate is a CPU rate that's the whole machine, in the job object's
// units: cycles per 10,000.
const FullRate = 10_000

// Limits are what the node job holds the slots to, in the job object's
// own terms.
type Limits struct {
	// Memory is the job memory limit in bytes, which counts the
	// memory every process in the job has committed, and so whatever
	// of it is paged out, or 0 for none.
	Memory uint64
	// CPURate is the hard cap on the CPU every process in the job uses
	// together, in cycles per 10,000 of the whole machine's, or 0 for
	// none.
	CPURate uint32
	// BelowNormal says the job holds every process in it to the
	// BELOW_NORMAL priority class, so the slots give way on CPU to the
	// machine's other work.
	BelowNormal bool
}

// LimitsFor returns the node job's limits for the budget b on a machine
// of cpus logical CPUs: a memory budget is the job memory limit, and a
// CPU budget of n CPUs is a hard cap of n/cpus of the machine, rounded
// down, so the slots never get more than their n CPUs, though never
// less than one cycle in 10,000. Whatever the budget, the job holds its
// processes below normal priority. A budget of more CPUs than the
// machine has, of a negative size, or for a machine of no CPUs is
// refused.
func LimitsFor(b availability.Budget, cpus int) (Limits, error) {
	l := Limits{BelowNormal: true}
	switch {
	case b.Memory < 0:
		return Limits{}, fmt.Errorf("a memory budget of %d bytes", b.Memory)
	case b.CPUs < 0:
		return Limits{}, fmt.Errorf("a CPU budget of %d CPUs", b.CPUs)
	case cpus < 1:
		return Limits{}, fmt.Errorf("a machine of %d CPUs", cpus)
	case b.CPUs > cpus:
		return Limits{}, fmt.Errorf("a CPU budget of %d CPUs on a machine of %d", b.CPUs, cpus)
	}
	l.Memory = uint64(b.Memory)
	if b.CPUs > 0 {
		l.CPURate = max(1, uint32(b.CPUs*FullRate/cpus))
	}
	return l, nil
}

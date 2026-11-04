package slot

import (
	"context"
	"fmt"
	"math"

	"github.com/godbus/dbus/v5"

	"github.com/meschoening/silkpool/internal/availability"
)

// SetBudget holds the slots to b, the node's budget, as ADR 0014 has it:
// runtime properties of their slice, set over D-Bus with
// SetUnitProperties, which outrank the operator's own drop-ins for the
// same settings and are gone after a reboot, so the agent sets them as
// it starts. A memory budget is MemoryMax=, with MemorySwapMax=0 so the
// slots can't swap past it, and none is infinity for both. A CPU budget
// is CPUQuota= at n × 100%, and none leaves it unset.
func (s *Slots) SetBudget(ctx context.Context, b availability.Budget) error {
	const infinity = uint64(math.MaxUint64)
	memory, swap, quota := infinity, infinity, infinity
	if b.Memory > 0 {
		memory, swap = uint64(b.Memory), 0
	}
	if b.CPUs > 0 {
		quota = uint64(b.CPUs) * 1_000_000 // µs per second: 1 s is one CPU
	}
	props := []property{
		{"MemoryMax", dbus.MakeVariant(memory)},
		{"MemorySwapMax", dbus.MakeVariant(swap)},
		{"CPUQuotaPerSecUSec", dbus.MakeVariant(quota)},
	}
	if err := s.manager(ctx, "SetUnitProperties", s.slice(), true, props).Err; err != nil {
		return fmt.Errorf("can't set the budget on %s: %w", s.slice(), err)
	}
	return nil
}

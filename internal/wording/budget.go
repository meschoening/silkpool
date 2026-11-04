package wording

import (
	"fmt"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
)

// The budget's words are from "Define how an operator bounds a node's
// slot resources":
// the Budget line the CLI's status and the dashboard's node page show,
// history's line for a change, and a job's line when it ran out of
// memory.

// Budget is the node's Budget line: each half, or none, and the memory
// still in force while a lowered memory budget is pending. "Budget: 4
// GB memory · 2 CPUs. Memory drops from 6 GB once no slot is running a
// job."
func (v View) Budget() string {
	n := v.Node
	if n.Machine.MacOS {
		return "Budget: not available on macOS"
	}
	line := "Budget: " + BudgetHalves(n.Availability.Budget)
	if _, pending := n.PendingMemory(); pending {
		if held := n.InForce.Memory; held == 0 {
			line += ". Memory stays unlimited until no slot is running a job."
		} else {
			line += fmt.Sprintf(". Memory drops from %s once no slot is running a job.", gbs(held))
		}
	}
	return line
}

// BudgetHalves is a budget in words: "4 GB memory · 2 CPUs", "4 GB
// memory · CPU not limited", or "none · slots may use the whole
// machine".
func BudgetHalves(b availability.Budget) string {
	if b == (availability.Budget{}) {
		return "none · slots may use the whole machine"
	}
	memory, cpus := "memory not limited", "CPU not limited"
	if b.Memory > 0 {
		memory = gbs(b.Memory) + " memory"
	}
	if b.CPUs > 0 {
		cpus = plural(b.CPUs, "CPU")
	}
	return memory + " · " + cpus
}

// gbs is a memory size in whole GB, as "4 GB".
func gbs(bytes int64) string { return fmt.Sprintf("%d GB", bytes/availability.GB) }

// BudgetSet is history's line for a budget change, from its command
// entry and the name of the node's machine: "Budget set to 4 GB memory ·
// 2 CPUs from the dashboard by meschoening".
func BudgetSet(c node.AppliedCommand, name string) string {
	to := BudgetHalves(c.Budget)
	if c.Budget == (availability.Budget{}) {
		to = "none"
	}
	return fmt.Sprintf("Budget set to %s %s", to, From(c.Origin, name))
}

// OutOfMemory is the line a job's entry adds when the OS ended one of
// its processes because memory ran out, or "" if it didn't: "Ran out of
// build-server's 4 GB memory budget", or, with no budget, "The OS ended a
// process because build-server ran out of memory". name is the node's
// machine's name.
func OutOfMemory(j node.Job, name string) string {
	switch {
	case !j.OutOfMemory:
		return ""
	case j.MemoryBudget > 0:
		return fmt.Sprintf("Ran out of %s's %s memory budget", name, gbs(j.MemoryBudget))
	}
	return fmt.Sprintf("The OS ended a process because %s ran out of memory", name)
}

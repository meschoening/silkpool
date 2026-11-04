package cli

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/wording"
)

// The budget is from "Define how an operator bounds a node's slot
// resources"
// and ADR 0014.

// budgetUsage is every form of budget.
const budgetUsage = `silkpool budget [--json]
       silkpool budget [--memory <n>GB|none] [--cpus <n>|none] [--revision N] [--json]
       silkpool budget --none [--revision N] [--json]`

// budgetJSON is a read of the budget as --json prints it: the settings,
// as slot-limit's read has them, with the budget in force, which differs
// while a lowered memory budget is pending, and the machine's size that
// bounds it.
type budgetJSON struct {
	settingsJSON
	InForce store.Budget  `json:"in_force"`
	Machine store.Machine `json:"machine"`
}

// runBudget shows the budget, or sets either half or clears it. Setting
// or lowering a half always applies; raising or clearing one is based on
// the revision the CLI reads, or the one --revision gives, as slot-limit
// does. A Mac has no budget.
func runBudget(ctx context.Context, s *session, args []string) int {
	fs := s.flags("budget", budgetUsage)
	rev := revisionFlag(fs)
	memoryText := fs.String("memory", "", "")
	cpusText := fs.String("cpus", "", "")
	none := fs.Bool("none", false, "")
	e := availability.SetBudget{}
	r, code := s.begin(ctx, func() (int, bool) {
		if _, code, ok := s.parse(fs, args, 0); !ok {
			return code, false
		}
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		switch {
		case *none && (set["memory"] || set["cpus"]):
			return s.usage(fs, "--none clears both halves of the budget, so it can't come with --memory or --cpus."), false
		case *none:
			e.SetMemory, e.SetCPUs = true, true
		case !set["memory"] && !set["cpus"] && set["revision"]:
			// --revision guards a change, and there's none to guard.
			return s.usage(fs, ""), false
		}
		if set["memory"] {
			gb, ok := parseMemory(*memoryText)
			if !ok {
				return s.usage(fs, fmt.Sprintf("--memory is a whole number of GB, such as 4GB, or none, and %q isn't one.", *memoryText)), false
			}
			e.SetMemory, e.Memory = true, gb*availability.GB
		}
		if set["cpus"] {
			cpus, ok := parseCPUs(*cpusText)
			if !ok {
				return s.usage(fs, fmt.Sprintf("--cpus is a whole number of CPUs, such as 2, or none, and %q isn't one.", *cpusText)), false
			}
			e.SetCPUs, e.CPUs = true, cpus
		}
		return ExitOK, true
	})
	if code != ExitOK {
		return code
	}
	if !e.SetMemory && !e.SetCPUs {
		s.printBudget(r)
		return ExitOK
	}
	if m := r.node.Machine; m.MacOS {
		return s.noBudget()
	} else if problem := tooBig(e, m); problem != "" {
		return s.usage(fs, problem)
	}
	b, given := base(r, *rev)
	e.Base = b
	_, _, code = s.command(ctx, r, e, given)
	if code != ExitOK || s.json {
		return code
	}
	// The reply carries the settings, not the budget in force, so read the
	// node again to say whether a lowered memory budget waits.
	after, code := s.readStatus(ctx)
	if code != ExitOK {
		return code
	}
	fmt.Fprintln(s.env.Stdout, after.view.Budget())
	return ExitOK
}

// printBudget prints the budget r read, or its JSON.
func (s *session) printBudget(r read) {
	if s.json {
		a, n := r.status.Node.Availability, r.status.Node
		s.printJSON(budgetJSON{settingsJSON: settingsJSON{Revision: a.Revision, Settings: a}, InForce: n.InForce, Machine: n.Machine})
		return
	}
	fmt.Fprintln(s.env.Stdout, r.view.Budget())
}

// noBudget refuses a budget on a Mac, which has none, in the decided
// words, and returns ExitRefused.
func (s *session) noBudget() int {
	if !s.json {
		fmt.Fprintln(s.env.Stderr, wording.Sentence(node.ErrNoBudget.Error()))
	}
	return s.refusedJSON(control.ErrorOf(node.ErrNoBudget))
}

// tooBig says why e asks for more than machine m has, or returns "".
func tooBig(e availability.SetBudget, m node.Machine) string {
	if most := m.RAM / availability.GB; e.SetMemory && e.Memory > most*availability.GB {
		return fmt.Sprintf("This machine has %d GB of RAM, so --memory can be from 1GB to %dGB, or none.", most, most)
	}
	if e.SetCPUs && e.CPUs > m.CPUs {
		cpus := "CPUs"
		if m.CPUs == 1 {
			cpus = "CPU"
		}
		return fmt.Sprintf("This machine has %d logical %s, so --cpus can be from 1 to %d, or none.", m.CPUs, cpus, m.CPUs)
	}
	return ""
}

// parseMemory parses --memory, as 4GB, in any case, or none, which is 0.
func parseMemory(text string) (int64, bool) {
	if strings.EqualFold(text, "none") {
		return 0, true
	}
	n, ok := strings.CutSuffix(strings.ToUpper(text), "GB")
	gb, err := strconv.ParseInt(n, 10, 64)
	if !ok || !digits(n) || err != nil || gb < 1 || gb > 1<<20 {
		return 0, false
	}
	return gb, true
}

// parseCPUs parses --cpus, as 2, or none, which is 0.
func parseCPUs(text string) (int, bool) {
	if strings.EqualFold(text, "none") {
		return 0, true
	}
	cpus, err := strconv.Atoi(text)
	if !digits(text) || err != nil || cpus < 1 {
		return 0, false
	}
	return cpus, true
}

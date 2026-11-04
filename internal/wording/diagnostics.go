package wording

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/store"
)

// The diagnostics report's words, which the node CLI prints and the
// dashboard's node page will show. No decision gives them, so they
// follow the CLI's status. Times are the report's own UTC stamps, since
// a report is passed on, to whoever helps with the machine.

// A Part is one labelled part of the diagnostics report: its label, as
// "Runner app", and its lines.
type Part struct {
	Label string
	Lines []string
}

// Diagnostics is the diagnostics report in words, part by part, in the
// order they're shown. The end of the agent's log follows them, under
// LogHeading.
func Diagnostics(r diagnostics.Report) []Part {
	m := r.Machine
	return []Part{
		{"Agent", []string{m.AgentVersion}},
		{"Machine", []string{fmt.Sprintf("%s · %s · %s · %s", m.Hostname, m.OSVersion, Arch(m.Arch), m.Zone)}},
		{"Time", []string{r.At}},
		{"Runner app", runnerApp(r.RunnerApp)},
		{"Slots", slotLines(r.Slots)},
		{"Exposure", exposure(r.Exposure)},
		{"Outbox", []string{outbox(r.Outbox)}},
		{"Free disk", []string{freeDisk(r.FreeDisk)}},
		{"Clock", []string{clock(r.Clock)}},
	}
}

// LogHeading heads the end of the agent's own log, of lines lines: "The
// agent's own log, its last 200 lines:".
func LogHeading(lines int) string {
	if lines == 0 {
		return "The agent's own log: no lines."
	}
	return fmt.Sprintf("The agent's own log, its last %s:", plural(lines, "line"))
}

// Arch is an architecture as the runner application names it: "x64"
// for amd64.
func Arch(goarch string) string {
	if goarch == "amd64" {
		return "x64"
	}
	return goarch
}

// runnerApp is each install and whose runners use it, the version the
// Hub wants, and an update under way: "2.338.2 installed, for new
// runners and slot 1's runner".
func runnerApp(app diagnostics.RunnerApp) []string {
	var lines []string
	for _, in := range app.Installs {
		var users []string
		if in.New {
			users = append(users, "new runners")
		}
		if len(in.Slots) > 0 {
			numbers := make([]string, len(in.Slots))
			for i, n := range in.Slots {
				numbers[i] = strconv.Itoa(n)
			}
			if len(in.Slots) == 1 {
				users = append(users, "slot "+numbers[0]+"'s runner")
			} else {
				users = append(users, "slots "+list(numbers)+"'s runners")
			}
		}
		lines = append(lines, in.Version+" installed, for "+list(users))
	}
	if len(lines) == 0 {
		lines = []string{"none installed"}
	}
	if v := app.Wanted.Version; v != "" {
		lines = append(lines, "the Hub wants "+v)
	} else {
		lines = append(lines, "the Hub hasn't named a version yet")
	}
	if u := app.Update; u.Version != "" {
		update := "Installing " + u.Version
		if u.Failed != "" {
			update += fmt.Sprintf(": %s, %s, next at %s", u.Failed, plural(u.Attempts, "attempt"), u.RetryAt)
		}
		lines = append(lines, update)
	}
	return lines
}

// slotLines is a line for each slot: its number, phase, runner and job,
// and its failure, with its detail.
func slotLines(slots []store.Slot) []string {
	var lines []string
	for _, s := range slots {
		line := fmt.Sprintf("%d  %s", s.Number, strings.ReplaceAll(s.Phase, "-", " "))
		if s.Runner.Name != "" {
			line += " · " + s.Runner.Name
		}
		if s.Runner.Job != "" {
			line += " · " + s.Runner.Job
		}
		if f := s.Failure; f.Reason != "" {
			why := f.Reason
			if r, err := store.ToReason(f.Reason); err == nil {
				why = r.String()
			}
			if f.Detail != "" {
				why += ": " + f.Detail
			}
			line += fmt.Sprintf(" · %s (%s since %s)", why, plural(f.Attempts, "attempt"), f.Since)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return []string{"none"}
	}
	return lines
}

// exposure is each slot's latest exposure check.
func exposure(checks []diagnostics.Check) []string {
	var lines []string
	for _, c := range checks {
		var line string
		switch c.Result {
		case diagnostics.Passed:
			line = fmt.Sprintf("Slot %d passed at %s", c.Slot, c.At)
		case diagnostics.Found:
			line = fmt.Sprintf("Slot %d found something at %s: %s", c.Slot, c.At, c.Detail)
		case diagnostics.Failed:
			line = fmt.Sprintf("Slot %d couldn't run at %s: %s", c.Slot, c.At, c.Detail)
		default:
			line = fmt.Sprintf("Slot %d not checked since the agent started", c.Slot)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return []string{"none"}
	}
	return lines
}

// outbox is how many entries the Hub hasn't stored, and when the oldest
// happened.
func outbox(o diagnostics.Outbox) string {
	if o.Entries == 0 || o.Oldest == nil {
		return "empty"
	}
	entries := "1 entry"
	if o.Entries != 1 {
		entries = fmt.Sprintf("%d entries", o.Entries)
	}
	return fmt.Sprintf("%s for the Hub, the oldest from %s", entries, o.Oldest.At)
}

// freeDisk is a size in bytes, in decimal units, or "unknown".
func freeDisk(n int64) string {
	switch {
	case n < 0:
		return "unknown"
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d bytes", n)
}

// clock is how far the machine's clock is from the Hub's, and when the
// agent measured it: "3 s ahead of the Hub's, measured as the agent
// connected at 2026-11-05T22:42:00Z".
func clock(c diagnostics.Clock) string {
	if c.Measured == "" {
		return "not measured since the agent started"
	}
	offset := c.Offset.Round(time.Second)
	way := "ahead of"
	if offset < 0 {
		way, offset = "behind", -offset
	}
	amount := fmt.Sprintf("%d s", int(offset/time.Second))
	switch minutes, seconds := int(offset/time.Minute), int(offset%time.Minute/time.Second); {
	case offset == 0:
		return "in step with the Hub's to the second, measured as the agent connected at " + c.Measured
	case offset >= time.Hour:
		amount = Duration(offset)
	case minutes > 0 && seconds == 0:
		amount = fmt.Sprintf("%d min", minutes)
	case minutes > 0:
		amount = fmt.Sprintf("%d min %d s", minutes, seconds)
	}
	return fmt.Sprintf("%s %s the Hub's, measured as the agent connected at %s", amount, way, c.Measured)
}

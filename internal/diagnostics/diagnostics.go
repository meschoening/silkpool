// Package diagnostics is a node's diagnostics report: a fixed, redacted
// snapshot of its software, slots and recent failures, which the node
// CLI prints and the Hub may ask a connected node for. The decisions
// behind it are the Diagnostics and Redaction sections of "Define
// Silkpool's events, audit trail, notices and redaction",
// and the command allowlist of "Define node enrollment, removal,
// identity and Hub-agent transport":
// the report is fixed, so the Hub's request for it names no file or
// path.
//
// The agent assembles the report with Of, from its node and what it
// reads of the machine. It holds:
//
//   - the machine, with the agent's and the OS's versions;
//   - the runner application's installs, and an update under way;
//   - each slot, with its failure's detail;
//   - each slot's latest exposure check;
//   - the outbox's length and its oldest entry;
//   - the free disk space;
//   - the clock's offset from the Hub;
//   - the last LogLines lines of the agent's own log.
//
// It never holds a copied runner log, because a job can write those, so
// they stay on the machine for an administrator. And it carries no
// secret: the node holds none, since each is a secret.Value the agent
// keeps beside it, and Of puts every text in the report through
// secret.Scrub.
//
// The package is pure: it never reads the clock, sleeps, or does I/O.
package diagnostics

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/store"
)

// ErrNotConnected refuses an ask for the report of a node that isn't
// connected, and fails one whose connection ended before the node
// answered: only a connected node can be asked.
var ErrNotConnected = errors.New("the node isn't connected, so it can't be asked for its diagnostics report")

// LogLines is how many lines of the agent's own log the report keeps,
// and LineLimit how long each may be, in bytes: a longer one is cut,
// ending in "…". Even escaped as JSON, the lines then fit well inside one
// message of the Hub-agent connection (8 MB) and of the control channel
// (4 MB).
const (
	LogLines  = 200
	LineLimit = 2048
)

// Report is the diagnostics report. Its JSON is what the Hub receives,
// and the `diagnostics --json` contract, which changes only by adding
// fields. Times are UTC stamps, as package store writes them.
type Report struct {
	// At is when the agent made it, by the machine's clock.
	At      string  `json:"at"`
	Machine Machine `json:"machine"`
	// RunnerApp is the runner application on the node.
	RunnerApp RunnerApp `json:"runner_app"`
	// Slots are the node's slots, each with its failure and its detail.
	Slots []store.Slot `json:"slots"`
	// Exposure is each slot's latest exposure check, in slot order.
	Exposure []Check `json:"exposure"`
	Outbox   Outbox  `json:"outbox"`
	// FreeDisk is the free space, in bytes, where the agent keeps its
	// state, or -1 if it couldn't be read.
	FreeDisk int64 `json:"free_disk"`
	// Clock is how far the machine's clock is from the Hub's, or zero if
	// the agent hasn't measured it since it started.
	Clock Clock `json:"clock,omitzero"`
	// Log is the end of the agent's own log, oldest line first.
	Log []string `json:"log"`
}

// Machine is the machine the agent runs on, as it tells the Hub when it
// enrolls: the same fields as wire.Machine.
type Machine struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	OSVersion    string `json:"os_version"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
	// Zone is the OS's time zone, by its IANA name.
	Zone string `json:"zone"`
}

// RunnerApp is the runner application on the node: the release the Hub
// wants, the installs the node uses, and the install of the wanted
// release while it's under way.
type RunnerApp struct {
	Wanted   store.RunnerApp `json:"wanted,omitzero"`
	Installs []Install       `json:"installs"`
	Update   store.Update    `json:"update,omitzero"`
}

// An Install is an install of the runner application that the node
// uses: new runners start from it, or a runner started from it still
// runs.
type Install struct {
	Version string `json:"version"`
	// New reports that new runners start from it.
	New bool `json:"new,omitempty"`
	// Slots are the slots whose runners started from it.
	Slots []int `json:"slots,omitempty"`
}

// A Check is a slot's latest exposure check since the agent started:
// its Result, when it ran, and what it found, or why it couldn't run.
type Check struct {
	Slot   int    `json:"slot"`
	Result string `json:"result"`
	At     string `json:"at,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// The results of an exposure check.
const (
	// Passed found nothing outside the slot that it can open.
	Passed = "passed"
	// Found found something outside the slot that it can open: Detail
	// names it, and the fix. The slot fails closed.
	Found = "found"
	// Failed couldn't run, so the slot fails closed: Detail says why.
	Failed = "failed"
	// Unchecked hasn't run since the agent started.
	Unchecked = "unchecked"
)

// CheckOf returns the exposure check a slot's preparation reports, at
// the time it was applied. Every preparation runs the check, so Prepared
// is one that passed. A preparation that failed for want of the runner
// application says nothing of it.
func CheckOf(e node.Event, at time.Time) (Check, bool) {
	stamp := store.Stamp(at)
	switch e := e.(type) {
	case node.Prepared:
		return Check{Slot: e.Slot, Result: Passed, At: stamp}, true
	case node.PrepareFailed:
		switch e.Reason {
		case node.SlotExposed:
			return Check{Slot: e.Slot, Result: Found, At: stamp, Detail: e.Detail}, true
		case node.SlotCantStart:
			return Check{Slot: e.Slot, Result: Failed, At: stamp, Detail: e.Detail}, true
		}
	}
	return Check{}, false
}

// Outbox is the node's outbox: how many entries the Hub hasn't stored,
// and the oldest of them.
type Outbox struct {
	Entries int          `json:"entries"`
	Oldest  *store.Entry `json:"oldest,omitempty"`
}

// Clock is the machine's clock's offset from the Hub's: how far ahead
// of it, or behind it when negative, as the agent measured it when it
// last connected to the Hub, at Measured.
type Clock struct {
	Offset   time.Duration `json:"offset"`
	Measured string        `json:"measured"`
}

// Sources are what the report is made of: what the agent holds, and
// what it read of the machine.
type Sources struct {
	// At is when the agent makes the report.
	At      time.Time
	Machine Machine
	Node    node.Node
	// Checks are the latest exposure checks of the slots, since the agent
	// started, as CheckOf made them.
	Checks []Check
	// FreeDisk is the free space where the agent keeps its state, or -1
	// if it couldn't be read.
	FreeDisk int64
	Clock    Clock
	// Log is the end of the agent's own log, oldest line first.
	Log []string
}

// Of returns the report s makes, scrubbed. It fails only if the node
// holds a value with no stored form.
func Of(s Sources) (Report, error) {
	n := s.Node
	stored, err := store.FromNode(n)
	if err != nil {
		return Report{}, err
	}
	r := Report{
		At:        store.Stamp(s.At),
		Machine:   s.Machine,
		RunnerApp: RunnerApp{Wanted: stored.RunnerApp, Installs: installs(n), Update: stored.Update},
		Slots:     stored.Slots,
		Exposure:  checks(n, s.Checks),
		Outbox:    Outbox{Entries: len(n.Outbox)},
		FreeDisk:  s.FreeDisk,
		Clock:     s.Clock,
		Log:       tail(s.Log),
	}
	if r.Slots == nil {
		r.Slots = []store.Slot{}
	}
	if len(stored.Outbox) > 0 {
		r.Outbox.Oldest = &stored.Outbox[0]
	}
	scrub(reflect.ValueOf(&r).Elem())
	return r, nil
}

// installs returns the installs n uses: the one new runners start from
// first, then the others in the order of the slots that use them.
func installs(n node.Node) []Install {
	out := []Install{}
	if n.Installed != "" {
		out = append(out, Install{Version: n.Installed, New: true})
	}
	for _, s := range n.Slots {
		v := s.Runner.Version
		if s.Phase != node.Hosting || v == "" {
			continue
		}
		i := slices.IndexFunc(out, func(in Install) bool { return in.Version == v })
		if i < 0 {
			out, i = append(out, Install{Version: v}), len(out)
		}
		out[i].Slots = append(out[i].Slots, s.Number)
	}
	return out
}

// checks returns the latest exposure check of each of n's slots, in slot
// order.
func checks(n node.Node, latest []Check) []Check {
	out := []Check{}
	for _, s := range n.Slots {
		c := Check{Slot: s.Number, Result: Unchecked}
		if i := slices.IndexFunc(latest, func(c Check) bool { return c.Slot == s.Number }); i >= 0 {
			c = latest[i]
		}
		out = append(out, c)
	}
	return out
}

// tail returns the last LogLines lines of log, each cut to LineLimit.
// The lines are scrubbed together first, so a private key block spread
// over lines is found, and the cut can't leave part of a secret behind.
func tail(log []string) []string {
	log = log[max(len(log)-LogLines, 0):]
	if len(log) == 0 {
		return []string{}
	}
	lines := strings.Split(secret.Scrub(strings.Join(log, "\n")), "\n")
	for i, l := range lines {
		lines[i] = cut(l)
	}
	return lines
}

// cut returns line cut to LineLimit bytes, ending in "…", at a
// character's start.
func cut(line string) string {
	if len(line) <= LineLimit {
		return line
	}
	end := LineLimit - len("…")
	for end > 0 && !utf8.RuneStart(line[end]) {
		end--
	}
	return line[:end] + "…"
}

// scrub puts every text v holds through secret.Scrub, in place, so
// whatever field the report gains is scrubbed as it is.
func scrub(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(secret.Scrub(v.String()))
		}
	case reflect.Struct:
		for i := range v.NumField() {
			scrub(v.Field(i))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			scrub(v.Elem())
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			scrub(v.Index(i))
		}
	}
}

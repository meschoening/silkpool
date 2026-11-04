package wording

import (
	"errors"
	"fmt"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
)

// The words of the dashboard's fleet pages, from variant A of
// "Prototype the Hub dashboard":
// the Nodes table's columns and the pool in a sentence, a node's verbs
// and its commands' states, the Hub's own notices, and Add node's
// commands, from "Define how each binary is packaged, installed,
// upgraded and signed per OS".

// For is how long the node has shown its state, as the Nodes table has
// it after the word: "for 31 min". The Hub knows only the node's last
// report, so it's read from what the report holds: the oldest job while
// Busy, the newest listener while Available, the first slot without a
// runner while Starting, the episode while Degraded, and the pause while
// paused. It's empty when the report doesn't say, as for a node that
// has been paused since it enrolled.
func (v View) For() string {
	n := v.Node
	var since time.Time
	earliest := func(t time.Time) {
		if !t.IsZero() && (since.IsZero() || t.Before(since)) {
			since = t
		}
	}
	switch v.Shown() {
	case node.Degraded:
		since = n.DegradedSince
	case node.Draining, node.Drained:
		since = n.IntentSet.At
	case node.Busy:
		for _, s := range n.Slots {
			if s.Phase == node.Hosting && s.Runner.Job != "" {
				earliest(s.Runner.JobSince)
			}
		}
	case node.Available:
		for _, s := range n.Slots {
			if r := s.Runner; s.Phase == node.Hosting && r.Listening && r.Job == "" && r.Started.After(since) {
				since = r.Started
			}
		}
	case node.Starting:
		for _, s := range n.Slots {
			earliest(s.RunnerlessSince)
		}
	}
	if since.IsZero() {
		return ""
	}
	return "for " + Duration(v.Clock.Now.Sub(since))
}

// LastKnown qualifies the state of a node that isn't connected, with how
// long ago it was last seen: "Last known · 2 h ago".
func (c Clock) LastKnown(seen time.Time) string {
	return "Last known · " + Duration(c.Now.Sub(seen)) + " ago"
}

// Doing is what a node is doing, in the Nodes table's one line: the jobs
// it runs, then any line that follows them, or else the line alone, in
// amber for a Degraded reason.
type Doing struct {
	Jobs  []string
	Line  string
	Amber bool
}

// Doing says what the node is doing, in one line.
func (v View) Doing() Doing {
	n := v.Node
	if n.Removal.Started {
		return Doing{Line: v.Lead()}
	}
	st := v.Shown()
	if st == node.Degraded {
		title, _ := Reason(n.DegradedSlots()[0].Failure)
		return Doing{Line: title, Amber: true}
	}
	if jobs := v.RunningJobs(); len(jobs) > 0 {
		d := Doing{Jobs: jobs}
		if st == node.Draining {
			d.Line = "then paused " + v.until(n.Availability)
		}
		return d
	}
	listening, idle := 0, 0
	for _, s := range n.Slots {
		r := s.Runner
		switch {
		case s.Phase != node.Hosting:
		case r.Listening && r.Retirement == node.NotRetiring:
			listening++
		default:
			idle++
		}
	}
	switch st {
	case node.Drained:
		return Doing{Line: "Paused " + v.until(n.Availability)}
	case node.Draining:
		if idle > 0 {
			return Doing{Line: "Retiring " + plural(idle, "idle runner")}
		}
		return Doing{Line: "Cleaning up its slots"}
	case node.Busy:
		return Doing{Line: "Getting ready for the next job"}
	case node.Available:
		return Doing{Line: "Listening on " + plural(listening, "slot")}
	}
	return Doing{Line: "Getting a runner ready…"}
}

// Pool sums up the pool in a sentence or two, as the Nodes page's header
// has it: the jobs running and the slots ready on the nodes that are
// connected, then who's paused.
func Pool(views []View) string {
	jobs, ready := 0, 0
	var paused []View
	for _, v := range views {
		n := v.Node
		if n.Availability.Intent != availability.OnDuty && !n.Removal.Started {
			paused = append(paused, v)
		}
		if !n.Connected {
			continue
		}
		for _, s := range n.Slots {
			r := s.Runner
			switch {
			case s.Phase != node.Hosting:
			case r.Job != "":
				jobs++
			case r.Listening && r.Retirement == node.NotRetiring && n.Availability.Intent == availability.OnDuty:
				ready++
			}
		}
	}
	s := "No slot is taking jobs."
	switch {
	case jobs > 0 && ready > 0:
		s = fmt.Sprintf("Running %s, with %s ready for more.", plural(jobs, "job"), plural(ready, "slot"))
	case jobs > 0:
		s = fmt.Sprintf("Running %s.", plural(jobs, "job"))
	case ready > 0:
		s = fmt.Sprintf("Ready for jobs on %s.", plural(ready, "slot"))
	}
	switch {
	case len(paused) == 1:
		s += fmt.Sprintf(" %s is paused %s.", paused[0].Name, paused[0].until(paused[0].Node.Availability))
	case len(paused) > 1:
		s += fmt.Sprintf(" %d nodes are paused.", len(paused))
	}
	return s
}

// A Verb is one of a node's command buttons: the command it sends, by
// its kind, its label, and whether it's the primary one.
type Verb struct {
	Kind    string
	Label   string
	Primary bool
}

// Verbs are the node's command buttons, as the tray has them, the
// primary first: Pause until its resume time or Pause indefinitely
// while On duty, and Resume now, then Pause indefinitely or Resume at
// its resume time instead, while paused.
func (v View) Verbs() []Verb {
	a := v.Node.Availability
	next := v.Clock.When(a.AutoResume.Next(v.Clock.Now))
	switch {
	case a.Intent == availability.OnDuty && a.AutoResume.On:
		return []Verb{{"pause", "Pause until " + next, true}, {"pause-indefinitely", "Pause indefinitely", false}}
	case a.Intent == availability.OnDuty:
		return []Verb{{"pause-indefinitely", "Pause", true}}
	case a.Intent == availability.Paused:
		return []Verb{{"resume", "Resume now", true}, {"pause-indefinitely", "Pause indefinitely", false}}
	case a.AutoResume.On:
		return []Verb{{"resume", "Resume now", true}, {"pause", "Resume at " + next + " instead", false}}
	}
	return []Verb{{"resume", "Resume now", true}}
}

// sending is what each kind of command does on the node, while it's
// sent.
var sending = map[string]string{
	"pause":              "Pausing",
	"pause-indefinitely": "Pausing",
	"resume":             "Resuming",
	"stop":               "Stopping",
	"slot-limit":         "Changing the slot limit",
	"auto-resume":        "Saving auto-resume",
	"dismiss":            "Dismissing the notice",
}

// Sending says a command of kind is sent, until the node replies:
// "Pausing on build-pc…", or "Setting build-pc's budget…".
func (v View) Sending(kind string) string {
	if kind == "budget" {
		return fmt.Sprintf("Setting %s's budget…", v.Name)
	}
	return fmt.Sprintf("%s on %s…", sending[kind], v.Name)
}

// Applied says the node applied a command, which the dashboard shows
// for a few seconds.
func (v View) Applied() string { return fmt.Sprintf("Applied on %s.", v.Name) }

// RefusedBy says the node refused a command, and why. A command based
// on what the page showed, which has changed since, says so.
func (v View) RefusedBy(err error) string {
	if errors.Is(err, availability.ErrStale) {
		return fmt.Sprintf("Refused by %s: it changed since this page showed it. Check it, then try again.", v.Name)
	}
	return fmt.Sprintf("Refused by %s: %s", v.Name, Sentence(err.Error()))
}

// NotConfirmed says a command's connection dropped before its reply.
func (v View) NotConfirmed() string {
	return fmt.Sprintf("Not confirmed: the connection dropped before %s replied. This settles when it reconnects.", v.Name)
}

// Confirmed says the node's report after it reconnected holds the reply
// to a command that wasn't confirmed.
func (v View) Confirmed() string {
	return fmt.Sprintf("Confirmed by %s's report after it reconnected.", v.Name)
}

// NotApplied says the node's report after it reconnected doesn't hold
// the reply to a command that wasn't confirmed: it never got it.
func (v View) NotApplied() string {
	return fmt.Sprintf("Not applied: %s's report after it reconnected doesn't hold it. Send it again if you still want it.", v.Name)
}

// CantSend says why a node's command controls are disabled while it
// isn't connected.
func (v View) CantSend() string {
	return fmt.Sprintf("Not connected, so commands can't be sent. They come back when %s reconnects.", v.Name)
}

// KeyInUse is the Hub's notice that a node's key is in use on two
// machines, whose connections keep replacing each other.
func KeyInUse(name string) NoticeText {
	return NoticeText{
		Text:   fmt.Sprintf("This node's key is in use on two machines. Usually a cloned disk. Remove %s, then enroll each machine with its own code.", name),
		Clears: fmt.Sprintf("Clears once %s is removed.", name),
	}
}

// NoTarget is the Hub's notice that a node has no target.
func NoTarget(name string) NoticeText {
	return NoticeText{
		Text:   fmt.Sprintf("No target. %s can't resume until you choose where its runners register.", name),
		Clears: "Clears once it has a target.",
	}
}

// VersionRefused is the Hub's notice that it refuses a node's
// connections for their protocol version, since since, and why.
func VersionRefused(c Clock, since time.Time, why string) NoticeText {
	return NoticeText{
		Text:   fmt.Sprintf("The Hub has refused its connections since %s, as %s.", c.When(since), why),
		Clears: "Clears once the Hub accepts its connection.",
	}
}

// TargetLost says the Hub can't see a target on GitHub, since since.
func TargetLost(c Clock, since time.Time) string {
	return fmt.Sprintf("Lost since %s: the Hub can't see it on GitHub. Its runners stop until the App's installation covers it again.", c.When(since))
}

// UnknownRunsOn is what a node's page says in place of its runs-on line
// while its platform is unknown.
const UnknownRunsOn = "It has no runs-on line while its platform is unknown: the Hub mints it no runners."

// osNames are the OSes' names, by their runner labels.
var osNames = map[string]string{"linux": "Linux", "macos": "macOS", "windows": "Windows"}

// releases is where Silkpool's releases are downloaded from.
const releases = "https://github.com/meschoening/silkpool/releases/download/v"

// where says where each OS runs Add node's commands, by its runner
// label.
var where = map[string]string{
	"linux":   "In a shell",
	"macos":   "In Terminal",
	"windows": "In PowerShell, run as administrator",
}

// Install is the one command that installs Silkpool on a machine of os,
// as runner labels name it, pinned to the Hub's version, and enrolls it
// with code.
func Install(os, version, code string) EnrollCommand {
	if os == "windows" {
		return EnrollCommand{OSes: "Windows", Where: where[os],
			Command: fmt.Sprintf("& ([scriptblock]::Create((irm %s%s/install.ps1))) -Code %s", releases, version, code)}
	}
	return EnrollCommand{OSes: osNames[os], Where: where[os],
		Command: fmt.Sprintf("curl -fsSL %s%s/install.sh | sudo sh -s -- %s", releases, version, code)}
}

// Enroll is the command that enrolls a machine of os that has Silkpool
// installed already, with code.
func Enroll(os, code string) EnrollCommand {
	for _, c := range EnrollCommands(code) {
		if (os == "windows") == (c.OSes == "Windows") {
			c.Where = where[os]
			return c
		}
	}
	return EnrollCommand{}
}

// DraftPreview says what saving the auto-resume draft d would do.
func (v View) DraftPreview(d availability.AutoResume) string {
	a := v.Node.Availability
	switch {
	case !d.On && a.Intent == availability.OnDuty:
		return "Pausing now would pause until you resume it."
	case !d.On && a.Intent == availability.Paused:
		return "Saving turns this pause into Paused indefinitely."
	case !d.On:
		return "Off, so a pause lasts until you resume it."
	case a.Intent == availability.OnDuty:
		return fmt.Sprintf("Pausing now would resume %s.", v.Clock.When(d.Next(v.Clock.Now)))
	case a.Intent == availability.Paused:
		return fmt.Sprintf("Resumes %s.", v.Clock.When(d.Next(v.Clock.Now)))
	}
	return fmt.Sprintf("It doesn't end a pause indefinitely. Pausing with it on would resume %s.", v.Clock.When(d.Next(v.Clock.Now)))
}

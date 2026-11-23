package wording

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
)

// The words of the Hub's history, as the dashboard's Activity lists it,
// from variant A of "Prototype the Hub dashboard" and the kinds of entry
// of "Define Silkpool's events, audit trail, notices and redaction":
// each line has its time, what happened, and its origin. Only a Degraded
// episode's start is amber, and only Emergency stop is red.

// A HistoryLine is one entry of history in words.
type HistoryLine struct {
	// Time is when it happened, on its day: "4:42 PM".
	Time string
	// Text is what happened: "Ran build (linux) #262 in 12 min ·
	// Failed".
	Text string
	// From is where it started, "from the dashboard by meschoening", or
	// empty for what has no origin, or says who in its Text.
	From string
	// Detail is a line that follows, such as what a Degraded slot saw, or
	// empty.
	Detail string
	// Link is a job's run on GitHub, or empty.
	Link string
	// Tone is "amber" for a Degraded episode's start, "red" for
	// Emergency stop, or empty.
	Tone string
	// Late says why the entry carries the ↓ late mark, or is empty for
	// one that isn't late.
	Late string
}

// History is entry e in words. name is the name of the entry's node, or
// empty for an entry about the Hub.
func (c Clock) History(e pool.Entry, name string) HistoryLine {
	l := HistoryLine{Time: c.TimeOfDay(e.At), From: From(e.Origin, name)}
	if e.Late {
		l.Late = fmt.Sprintf("Reported late, after %s reconnected", name)
	}
	switch x := e.What.(type) {
	case node.AppliedCommand:
		l.Text = command(x)
		if _, ok := x.Event.(availability.EmergencyStop); ok {
			l.Tone = "red"
		}
	case node.Job:
		l.Text, l.Detail, l.Link = job(x), OutOfMemory(x, name), x.Run.Link()
	case node.DegradedBegan:
		title, _ := Reason(node.Failure{Reason: x.Reason})
		l.Text, l.Detail, l.Tone = "Degraded: "+title, Sentence(x.Detail), "amber"
		if x.Slot > 0 {
			l.Text = fmt.Sprintf("Degraded on slot %d: %s", x.Slot, title)
		}
	case node.DegradedEnded:
		d := Duration(e.At.Sub(x.Began))
		switch x.Why {
		case node.EndedByPause:
			l.Text = fmt.Sprintf("Degraded for %s, until paused", d)
		case node.EndedByRemoval:
			l.Text = fmt.Sprintf("Degraded for %s, until removed", d)
		default:
			l.Text = "Recovered after " + d
		}
	case node.NoticeRaised:
		l.Text, l.Detail = notice(x.Notice)
	case node.RunnerVersionRefused:
		l.Text = fmt.Sprintf("GitHub refused runner version %s", x.Version)
		if x.Slot > 0 {
			l.Text += fmt.Sprintf(" on slot %d", x.Slot)
		}
	case pool.ConnectionGap:
		l.Text = "Not connected " + c.span(x.Since, e.At)
	case pool.HubStarted:
		l.Text = "Hub started, after being down " + Duration(e.At.Sub(x.Alive))
	case pool.ConnectionRefused:
		l.Text = "Refused a connection"
		switch {
		case errors.Is(x.Reason, pool.ErrWrongKey):
			l.Text += ": it didn't prove itself with the node's key"
		case x.Reason != nil:
			l.Text += ": " + x.Reason.Error()
		}
	case pool.Enrollment:
		l.Text = fmt.Sprintf("Enrolled as %s, with no target", x.Name)
		if x.Target != "" {
			l.Text = fmt.Sprintf("Enrolled as %s, with the target %s", x.Name, Quote(x.Target))
		}
	case pool.RemovalStarted:
		l.Text = "Removal started"
	case pool.Tombstoned:
		l.Text = "Removed from the Hub"
	case pool.RunnerVersionChanged:
		l.Text = fmt.Sprintf("Runner version for %s set to %s", Quote(x.Target), x.To)
		if x.From != "" {
			l.Text = fmt.Sprintf("Runner version for %s moved from %s to %s", Quote(x.Target), x.From, x.To)
		}
	case pool.LostTarget:
		l.Text = fmt.Sprintf("Lost the target %s: the Hub can't see it on GitHub", Quote(x.Target))
	case pool.RegainedTarget:
		l.Text = fmt.Sprintf("Regained the target %s: the Hub can see it on GitHub again", Quote(x.Target))
	case pool.CodeMade:
		l.Text = "Made enrollment code " + x.Code
	case pool.CodeRevoked:
		l.Text = "Revoked enrollment code " + x.Code
	case pool.AddedTarget:
		l.Text = "Added the target " + Quote(x.Target)
	case pool.RemovedTarget:
		l.Text = "Removed the target " + Quote(x.Target)
	case pool.Renamed:
		l.Text = fmt.Sprintf("Renamed from %s to %s", x.From, x.To)
	case pool.Retargeted:
		l.Text = "Target set to " + Quote(x.To)
		if x.From != "" {
			l.Text = fmt.Sprintf("Target changed from %s to %s", Quote(x.From), Quote(x.To))
		}
	case pool.LabelsChanged:
		l.Text = labels(x.From, x.To)
	case pool.PoolLabelChanged:
		l.Text = "Pool label set to " + x.To
		if x.From != "" {
			l.Text = fmt.Sprintf("Pool label changed from %s to %s", x.From, x.To)
		}
	case pool.GitHubConnectionAdded:
		l.Text = "Added the GitHub connection " + x.Account
	case pool.KeepHistoryChanged:
		l.Text = fmt.Sprintf("History kept %s, instead of %s", keeping(x.To), keeping(x.From))
	case pool.SignedIn:
		// Who signed in is the origin's login, which the text says.
		l.Text, l.From = fmt.Sprintf("%s signed in from %s", e.Origin.Login, x.Address), ""
	case pool.SignInRefused:
		l.Text, l.From = fmt.Sprintf("Refused a sign-in by %s from %s, who isn't this Hub's operator", x.Login, x.Address), ""
	default:
		l.Text = fmt.Sprintf("%T", e.What)
	}
	return l
}

// OpenGap is the line a node's history starts with while the node
// isn't connected: the gap the Hub writes once it reconnects, still
// open since it was last seen. "Not connected since 2:54 PM".
func (c Clock) OpenGap(seen time.Time) HistoryLine {
	return HistoryLine{Time: c.TimeOfDay(seen), Text: "Not connected since " + c.TimeOfDay(seen)}
}

// command is an applied command in words: "Paused indefinitely",
// "Emergency stop, cancelling build-windows #231".
func command(c node.AppliedCommand) string {
	switch x := c.Event.(type) {
	case availability.Pause:
		return "Paused until auto-resume"
	case availability.PauseIndefinitely:
		return "Paused indefinitely"
	case availability.Resume:
		if c.Origin.Surface == node.AutoResume {
			return "Back on duty"
		}
		return "Resumed"
	case availability.EmergencyStop:
		var jobs []string
		for _, j := range c.Cancelled {
			jobs = append(jobs, j.Job)
		}
		if len(jobs) == 0 {
			return "Emergency stop, with no job running"
		}
		return "Emergency stop, cancelling " + list(jobs)
	case availability.EditAutoResume:
		return "Auto-resume changed: " + AutoResume(x.To)
	case availability.TurnOffAutoResume:
		return "Auto-resume turned off"
	case availability.SetSlotLimit:
		return fmt.Sprintf("Slot limit set to %d", x.To)
	case availability.SetBudget:
		return "Budget set to " + budgetTo(c.Budget)
	}
	return fmt.Sprintf("Applied %T", c.Event)
}

// job is a job's entry in words: "Ran build (linux) #262 in 12 min ·
// Failed", or "Job build (linux) #262 was interrupted after 12 min". A
// job's name keeps its own case.
func job(j node.Job) string {
	name := j.Name
	if name == "" {
		name = "a job"
	}
	if j.Run.Number > 0 {
		name = fmt.Sprintf("%s #%d", name, j.Run.Number)
	}
	d := Duration(j.Ended.Sub(j.Started))
	switch j.Outcome {
	case node.JobInterrupted:
		return fmt.Sprintf("Job %s was interrupted after %s", name, d)
	case node.JobCancelled:
		return fmt.Sprintf("Job %s was cancelled by Emergency stop after %s", name, d)
	}
	s := fmt.Sprintf("Ran %s in %s", name, d)
	if r := results[j.Result]; r != "" {
		s += " · " + r
	}
	return s
}

// results are GitHub's results for a job, in words.
var results = map[node.Result]string{
	node.ResultSucceeded:           "Succeeded",
	node.ResultSucceededWithIssues: "Succeeded with issues",
	node.ResultFailed:              "Failed",
	node.ResultCanceled:            "Cancelled",
	node.ResultSkipped:             "Skipped",
	node.ResultAbandoned:           "Abandoned",
}

// notice is a notice a node raised, in words, and its detail.
func notice(n node.Notice) (text, detail string) {
	switch n.Kind {
	case node.NoticeInterruptedJob:
		name := n.Job.Name
		if name == "" {
			name = n.Detail
		}
		if n.Job.Run.Number > 0 {
			name = fmt.Sprintf("%s #%d", name, n.Job.Run.Number)
		}
		return fmt.Sprintf("Notice: %s was interrupted", name), ""
	case node.NoticeUpdateFailed:
		// Its detail says it all: "Couldn't update the runner
		// application to 2.338.2: no space left on device. Retrying."
		return "Notice: " + Sentence(n.Detail), ""
	case node.NoticeSlotExposed:
		return fmt.Sprintf("Notice: slot %d can see files outside its own", n.Slot), Sentence(n.Detail)
	}
	return "Notice: " + n.Kind.String(), Sentence(n.Detail)
}

// labels is a change of a node's custom labels in words: "Added the
// label gpu", "Added the labels a and b, and removed the label c".
func labels(from, to []string) string {
	var added, removed []string
	for _, l := range to {
		if !slices.Contains(from, l) {
			added = append(added, l)
		}
	}
	for _, l := range from {
		if !slices.Contains(to, l) {
			removed = append(removed, l)
		}
	}
	some := func(verb string, ls []string) string {
		if len(ls) == 1 {
			return verb + " the label " + ls[0]
		}
		return verb + " the labels " + list(ls)
	}
	switch {
	case len(added) > 0 && len(removed) > 0:
		return some("Added", added) + ", and " + some("removed", removed)
	case len(added) > 0:
		return some("Added", added)
	case len(removed) > 0:
		return some("Removed", removed)
	}
	return "Labels set to " + list(to)
}

// keeping is how long history is kept, in days, in words: "for 90
// days", or "forever".
func keeping(days int) string {
	if days == pool.KeepForever {
		return "forever"
	}
	return "for " + plural(days, "day")
}

// span is the time from one time to another, as a connection gap has
// it: "2:42–2:56 PM" within a day, or "from yesterday 11:40 PM to 2:56
// AM" across one.
func (c Clock) span(from, to time.Time) string {
	a, b := c.TimeOfDay(from), c.TimeOfDay(to)
	if c.day(from) != c.day(to) {
		return fmt.Sprintf("from %s to %s", c.When(from), b)
	}
	if ia, ib := strings.IndexByte(a, ' '), strings.IndexByte(b, ' '); ia > 0 && ib > 0 && a[ia:] == b[ib:] {
		a = a[:ia]
	}
	return a + "–" + b
}

// Day is the day of t, as Activity heads the entries of each day:
// "Today", "Yesterday", or a date, "Mon 2 Nov", with its year when it
// isn't this year's.
func (c Clock) Day(t time.Time) string {
	z := c.zone()
	switch c.day(t) {
	case "":
		return "Today"
	case "yesterday":
		return "Yesterday"
	}
	if t.In(z).Year() != c.Now.In(z).Year() {
		return t.In(z).Format("Mon 2 Jan 2006")
	}
	return t.In(z).Format("Mon 2 Jan")
}

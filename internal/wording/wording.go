// Package wording writes Silkpool's interface words: state words and
// sentences, slot rows, Degraded reasons, notices, "from where" lines
// and the confirmations. The node CLI is its first user, and the tray
// app and the Hub's dashboard reuse it, so their words can't drift, as
// "Choose the desktop shell and dashboard frontend stack"
// decided. The words are the tray prototype's, from "Prototype the tray
// and menu-bar panel",
// and the CLI's, from "Name the CLI and define its commands".
//
// Everything here is plain text: no glyphs, color or layout, which each
// surface adds. Times arrive structured, and each surface keeps them
// current itself.
package wording

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
)

// A View is a node as the words describe it.
type View struct {
	Node node.Node
	// Name is the machine's name, as "build-pc" in "build-pc stays paused".
	Name  string
	Clock Clock
	// HubSince is when the node last connected to its Hub or lost its
	// connection.
	HubSince time.Time
	// Smooth asks for the gap between jobs to be smoothed, as the tray app
	// and the dashboard show a node (see Shown). The CLI shows the state
	// as it is.
	Smooth bool
}

// held is a job a slot's runner holds.
type held struct {
	slot int
	job  string
}

// jobs returns the jobs the node's runners hold, and the ones of those
// Emergency stop is cancelling, in slot order.
func (v View) jobs() (running, cancelling []held) {
	for _, s := range v.Node.Slots {
		r := s.Runner
		switch {
		case s.Phase != node.Hosting || r.Job == "":
		case r.Retirement == node.Cancelling || r.Retirement == node.StoppingLost:
			cancelling = append(cancelling, held{s.Number, r.Job})
		default:
			running = append(running, held{s.Number, r.Job})
		}
	}
	return running, cancelling
}

// RunningJobs names the jobs the node's runners hold, in slot order.
func (v View) RunningJobs() []string {
	running, cancelling := v.jobs()
	var names []string
	for _, h := range slices.Concat(running, cancelling) {
		names = append(names, h.job)
	}
	return names
}

// State is the node's state word, as it's shown: Drained, Draining,
// Starting, Available, Busy or Degraded.
func (v View) State() string { return v.Shown().String() }

// Amber reports that the node's state is shown in amber: it's Degraded.
func (v View) Amber() bool { return v.Shown() == node.Degraded }

// SmoothFor is how long a node keeps showing Busy after a job ends,
// while its next runner registers, before it shows Starting.
const SmoothFor = 10 * time.Second

// Shown is the node's state as the words show it. With Smooth, a node
// Starting because a job just ended keeps showing Busy for SmoothFor, as
// the tray's "Starting between jobs: smoothed" has it: a wanted slot
// went without a runner less than SmoothFor ago, after the intent was
// set, and not because its listener died. The slot rows always show
// what's really happening.
func (v View) Shown() node.State {
	n := v.Node
	st := n.State()
	if !v.Smooth || st != node.Starting || n.Removal.Started {
		return st
	}
	for _, s := range n.Slots {
		gap := v.Clock.Now.Sub(s.RunnerlessSince)
		if s.Number <= n.Availability.SlotLimit && s.RunnerlessSince.After(n.IntentSet.At) && gap >= 0 && gap < SmoothFor && s.Failure.Reason != node.RunnerOffline {
			return node.Busy
		}
	}
	return st
}

// until is how long a pause lasts, as "until Fri 7:30 AM" or "until you
// resume it".
func (v View) until(a availability.Node) string {
	if a.Intent == availability.Paused && !a.ResumeAt.IsZero() {
		return "until " + v.Clock.When(a.ResumeAt)
	}
	return "until you resume it"
}

// Lead is the one sentence that says what the node is doing, counting
// its jobs. Job names are in the slot rows, not here.
func (v View) Lead() string {
	n := v.Node
	running, cancelling := v.jobs()
	listening := 0
	for _, s := range n.Slots {
		if s.Phase == node.Hosting && s.Runner.Listening && s.Runner.Job == "" && s.Runner.Retirement == node.NotRetiring {
			listening++
		}
	}
	limit := n.Availability.SlotLimit
	then := "then paused " + v.until(n.Availability)
	if n.Removal.Started {
		then = "then it leaves the Hub"
	}
	switch v.Shown() {
	case node.Available:
		if limit > 1 && listening < limit {
			return fmt.Sprintf("Ready for jobs on %d of %d slots.", listening, limit)
		}
		return "Ready for jobs."
	case node.Busy:
		if len(running) == 0 {
			return "Getting ready for the next job."
		}
		if listening > 0 {
			return fmt.Sprintf("Running %s. Ready for %d more.", plural(len(running), "job"), listening)
		}
		return fmt.Sprintf("Running %s.", plural(len(running), "job"))
	case node.Starting:
		if limit > 1 {
			return "Getting runners ready for jobs."
		}
		return "Getting a runner ready for jobs."
	case node.Degraded:
		title, _ := Reason(n.DegradedSlots()[0].Failure)
		return Sentence(title)
	case node.Draining:
		idle := 0
		for _, s := range n.Slots {
			if s.Phase == node.Hosting && s.Runner.Job == "" {
				idle++
			}
		}
		switch {
		case len(cancelling) > 0:
			return fmt.Sprintf("Cancelling %s, %s.", plural(len(cancelling), "job"), then)
		case len(running) > 0:
			return fmt.Sprintf("Finishing %s, %s.", plural(len(running), "job"), then)
		case idle > 0 && !n.Connected:
			return fmt.Sprintf("Waiting for the Hub to retire %s, %s.", plural(idle, "idle runner"), then)
		case idle > 0:
			return fmt.Sprintf("Retiring %s, %s.", plural(idle, "idle runner"), then)
		case n.Removal.Started:
			return "Cleaning up its slots, then it leaves the Hub."
		}
		return fmt.Sprintf("Cleaning up its slots, %s.", then)
	}
	if n.Removal.Started {
		return "Leaving the Hub."
	}
	return Sentence("Paused " + v.until(n.Availability))
}

// Details are the lines that follow the sentence: a Degraded reason's
// detail and the jobs still running, or a newcomer's first steps.
func (v View) Details() []string {
	n := v.Node
	var lines []string
	switch v.Shown() {
	case node.Degraded:
		if _, detail := Reason(n.DegradedSlots()[0].Failure); detail != "" {
			lines = append(lines, detail)
		}
		if running, _ := v.jobs(); len(running) > 0 {
			lines = append(lines, fmt.Sprintf("Still running %s.", plural(len(running), "job")))
		}
	case node.Drained:
		if n.IntentSet.What == nil && !n.Removal.Started {
			lines = append(lines, "Newly enrolled. Check auto-resume and the slot limit first.")
		}
	}
	return lines
}

// From is where a command came from, for "Paused from build-pc's command
// line by max". name is the node's machine's name.
func From(o node.Origin, name string) string {
	by := func(s, who string) string {
		if who == "" {
			return s
		}
		return s + " by " + who
	}
	switch o.Surface {
	case node.CommandLine:
		return by(fmt.Sprintf("from %s's command line", name), o.Account)
	case node.Tray:
		return by(fmt.Sprintf("from %s's tray", name), o.Account)
	case node.Dashboard:
		return by("from the dashboard", o.Login)
	case node.HubCommandLine:
		return by("from the Hub's command line", o.Account)
	case node.AutoResume:
		return "by auto-resume"
	case node.Hub:
		return "by the Hub"
	}
	return ""
}

// Provenance says who set the node's intent, and when: while paused,
// and for an hour after it was resumed. It's empty otherwise.
func (v View) Provenance() string {
	n := v.Node
	set := n.IntentSet
	c, ok := set.What.(node.AppliedCommand)
	if n.Availability.Intent == availability.OnDuty {
		if !ok || v.Clock.Now.Sub(set.At) > time.Hour {
			return ""
		}
		if c.Origin.Surface == node.AutoResume {
			return fmt.Sprintf("Back on duty %s, by auto-resume.", v.Clock.At(set.At))
		}
		return fmt.Sprintf("Resumed %s %s.", From(c.Origin, v.Name), v.Clock.At(set.At))
	}
	if n.Removal.Started {
		return ""
	}
	if !ok {
		return "Paused since it was enrolled."
	}
	from := From(c.Origin, v.Name)
	switch c.Event.(type) {
	case availability.EmergencyStop:
		var jobs []string
		for _, j := range c.Cancelled {
			jobs = append(jobs, j.Job)
		}
		if len(jobs) > 0 {
			return fmt.Sprintf("Stopped now %s %s, cancelling %s.", from, v.Clock.At(set.At), list(jobs))
		}
		return fmt.Sprintf("Stopped now %s %s.", from, v.Clock.At(set.At))
	case availability.TurnOffAutoResume, availability.EditAutoResume:
		return fmt.Sprintf("Paused indefinitely when auto-resume was turned off %s %s.", from, v.Clock.At(set.At))
	}
	if n.State() == node.Draining {
		return fmt.Sprintf("Pause requested %s %s.", from, v.Clock.At(set.At))
	}
	return fmt.Sprintf("Paused %s %s.", from, v.Clock.At(set.At))
}

// Reason is a slot's failure's title, as "Hub unreachable", and its
// detail: what the agent saw, or what to check.
func Reason(f node.Failure) (title, detail string) {
	if f.Reason == 0 {
		return "", ""
	}
	detail = Sentence(f.Detail)
	if detail == "" {
		detail = reasonDetails[f.Reason]
	}
	return f.Reason.String(), detail
}

// reasonDetails say what to check for a failure that came with no
// detail of its own.
var reasonDetails = map[node.Reason]string{
	node.HubUnreachable:       "Silkpool keeps trying. Jobs already running aren't affected.",
	node.GitHubRefused:        "Check that the Hub's GitHub App is installed on the target, with the permissions it asks for.",
	node.GitHubUnavailable:    "GitHub's API is failing or limiting the Hub's requests. Silkpool keeps trying.",
	node.RunnerOffline:        "The runner's listener exited or didn't connect to GitHub.",
	node.RunnerAppUnavailable: "The runner application couldn't be downloaded or verified, or GitHub no longer accepts its version.",
	node.SlotCantStart:        "The slot couldn't be started or reset.",
	node.SlotExposed:          "The slot's account can open something outside the slot.",
	node.VersionMismatch:      "Upgrade Silkpool here or on the Hub, so that their versions match.",
	node.HubIdentityChanged:   "The Hub presented a key other than the one this machine pinned when it enrolled.",
	node.WrongNodeKey:         "The Hub holds a different key for this node.",
}

// A Row is a slot's row: its number, what it's doing, how long for, and
// a second line with its repository and qualifiers, or its failure.
type Row struct {
	Slot  int
	Main  string
	Right string
	Sub   string
	// Amber reports that the row shows a failure past its grace.
	Amber bool
}

// Rows are the node's slot rows. While the node is paused, free slots
// leave the list.
func (v View) Rows() []Row {
	n := v.Node
	var rows []Row
	for _, s := range n.Slots {
		wanted := n.Availability.Intent == availability.OnDuty && s.Number <= n.Availability.SlotLimit
		if s.Phase == node.Free && !wanted && s.Failure.Reason == 0 {
			continue
		}
		rows = append(rows, v.row(s, wanted))
	}
	return rows
}

func (v View) row(s node.Slot, wanted bool) Row {
	n, r := v.Node, s.Runner
	row := Row{Slot: s.Number}
	retry := func() string {
		if s.Failure.RetryAt.After(v.Clock.Now) {
			return "next try " + Soon(s.Failure.RetryAt.Sub(v.Clock.Now))
		}
		return "trying again now"
	}
	failing := func() {
		title, _ := Reason(s.Failure)
		if s.Degraded {
			row.Main, row.Amber = "No runner", true
			row.Sub = fmt.Sprintf("Attempt %d · %s", s.Failure.Attempts, retry())
			return
		}
		if title != "" {
			row.Sub = title + " · " + retry()
		}
	}
	closing := s.Number > n.Availability.SlotLimit && n.Availability.Intent == availability.OnDuty
	switch s.Phase {
	case node.Hosting:
		switch {
		case r.Job != "" && (r.Retirement == node.Cancelling || r.Retirement == node.StoppingLost):
			row.Main = "Cancelling " + r.Job
			row.Sub = v.repo()
		case r.Job != "":
			row.Main = r.Job
			row.Right = Duration(v.Clock.Now.Sub(r.JobSince))
			subs := []string{v.repo()}
			if closing || r.Retirement == node.ClosingAfterJob && !wanted && n.Availability.Intent == availability.OnDuty {
				subs = append(subs, "slot closes after this job")
			}
			row.Sub = strings.Join(slices.DeleteFunc(subs, func(s string) bool { return s == "" }), " · ")
		case r.Retirement == node.Retiring && !n.Connected:
			row.Main = "Idle runner, waiting for the Hub"
		case r.Retirement != node.NotRetiring && closing:
			row.Main = "Closing this slot…"
		case r.Retirement != node.NotRetiring:
			row.Main = "Retiring the idle runner…"
		case r.Listening:
			row.Main = "Listening for a job"
		default:
			row.Main = "Getting a runner ready…"
			failing()
		}
	case node.Free, node.Preparing, node.Requesting:
		row.Main = "Getting a runner ready…"
		if s.Ended.Name != "" {
			row.Main = "Getting the next runner ready…"
		}
		if !wanted {
			row.Main = "No runner"
		}
		failing()
		if s.Failure.Reason == node.SlotExposed && !wanted {
			row.Sub = s.Failure.Reason.String()
		}
	case node.Resetting:
		row.Main = "Resetting the slot…"
	case node.RetryingReset:
		row.Main = "Resetting the slot…"
		row.Sub = "Couldn't reset it · " + retry()
		row.Amber = s.Degraded
	case node.DeletingAccount:
		row.Main = "Deleting the slot's account…"
	case node.RetryingDelete:
		row.Main = "Deleting the slot's account…"
		row.Sub = "Couldn't delete it · " + retry()
	case node.Deleted:
		row.Main = "Slot account deleted"
	}
	return row
}

// repo is the repository the node's target is, or "" for an
// organization's.
func (v View) repo() string {
	if sc := v.Node.Scope; sc.Repo != "" {
		return sc.Owner + "/" + sc.Repo
	}
	return ""
}

// A NoticeText is a notice in words: what it says, and for one about a
// condition, the line that says when it clears.
type NoticeText struct {
	ID   int64
	Text string
	// Clears says when a notice about a condition clears, or is empty
	// for one about something that happened, which can be dismissed.
	Clears string
	Amber  bool
	// Link is the interrupted job's run, if the node knows it.
	Link string
}

// Notices are the node's notices in words, oldest first.
func (v View) Notices() []NoticeText {
	var out []NoticeText
	for _, x := range v.Node.Notices {
		t := NoticeText{ID: x.ID}
		switch x.Kind {
		case node.NoticeInterruptedJob:
			name := x.Job.Name
			if name == "" {
				name = x.Detail
			}
			if x.Job.Run.Number > 0 {
				name = fmt.Sprintf("%s #%d", name, x.Job.Run.Number)
			}
			t.Text = fmt.Sprintf("%s was interrupted when %s restarted %s. GitHub marks it failed; re-run it there if you need it.", name, v.Name, v.Clock.At(x.Raised))
			t.Link = x.Job.Run.Link()
		case node.NoticeUpdateFailed:
			t.Text = Sentence(x.Detail)
			t.Clears = "Clears when the update succeeds, or the Hub moves to another version."
		case node.NoticeSlotExposed:
			t.Text = Sentence(fmt.Sprintf("Slot %d can see files outside its own: %s", x.Slot, strings.TrimSuffix(x.Detail, ".")))
			t.Clears = "Clears when the slot's exposure check passes, or when the slot is wanted again and shows the hit itself."
			t.Amber = true
		default:
			t.Text = Sentence(x.Detail)
		}
		out = append(out, t)
	}
	return out
}

// Dismissible returns the IDs of the notices about something that
// happened, which Dismiss clears.
func (v View) Dismissible() []int64 {
	var ids []int64
	for _, x := range v.Node.Notices {
		if !x.Kind.Condition() {
			ids = append(ids, x.ID)
		}
	}
	return ids
}

// AutoResume is the auto-resume settings in one line: "On · 7:30 AM ·
// Mon–Fri · America/Chicago", or "Off · keeps 8:00 AM, Every day for
// later".
func AutoResume(a availability.AutoResume) string {
	zone := ""
	if a.Zone != nil {
		zone = a.Zone.String()
	}
	if !a.On {
		return fmt.Sprintf("Off · keeps %s, %s, %s for later", Hour(a.Time), Days(a.Days), zone)
	}
	return fmt.Sprintf("On · %s · %s · %s", Hour(a.Time), Days(a.Days), zone)
}

// AutoResumePreview says what auto-resume means now: what pausing now
// would do, or when a pause ends.
func (v View) AutoResumePreview() string {
	a := v.Node.Availability
	switch {
	case !a.AutoResume.On && a.Intent == availability.OnDuty:
		return "Pausing now would pause until you resume it."
	case !a.AutoResume.On:
		return "Off, so a pause lasts until you resume it."
	case a.Intent == availability.OnDuty:
		return fmt.Sprintf("Pausing now would resume %s.", v.Clock.When(a.AutoResume.Next(v.Clock.Now)))
	case a.Intent == availability.Paused:
		return fmt.Sprintf("Resumes %s, in %s.", v.Clock.When(a.ResumeAt), Duration(a.ResumeAt.Sub(v.Clock.Now)))
	}
	return fmt.Sprintf("It doesn't end a pause indefinitely. Pausing with it on would resume %s.", v.Clock.When(a.AutoResume.Next(v.Clock.Now)))
}

// SlotLimit is the slot limit and the range it can be set in: "Slot
// limit 2 (1 to 64)."
func SlotLimit(limit int) string {
	return fmt.Sprintf("Slot limit %d (1 to %d).", limit, availability.MaxSlotLimit)
}

// SlotLimitSet says what setting the slot limit did, from v, the node
// before, and a, the settings the command left: "Slot limit 3. Getting a
// runner ready for slot 3.", or "Slot limit 1. Slot 2 closes after
// build-windows." While paused, the change waits for a resume instead.
func (v View) SlotLimitSet(a availability.Node) string {
	before := v.Node.Availability.SlotLimit
	if a.SlotLimit == before {
		return fmt.Sprintf("The slot limit was already %d.", a.SlotLimit)
	}
	out := []string{fmt.Sprintf("Slot limit %d.", a.SlotLimit)}
	switch {
	case a.Intent != availability.OnDuty:
		out = append(out, "The new slot limit takes effect when you resume.")
	case a.SlotLimit == before+1:
		out = append(out, fmt.Sprintf("Getting a runner ready for slot %d.", a.SlotLimit))
	case a.SlotLimit == before+2:
		out = append(out, fmt.Sprintf("Getting runners ready for slots %d and %d.", before+1, a.SlotLimit))
	case a.SlotLimit > before:
		out = append(out, fmt.Sprintf("Getting runners ready for slots %d to %d.", before+1, a.SlotLimit))
	default:
		for _, s := range v.Node.Slots {
			r := s.Runner
			switch {
			case s.Number <= a.SlotLimit || s.Phase == node.Free:
			case s.Phase == node.Hosting && r.Job != "" && r.Retirement != node.Cancelling && r.Retirement != node.StoppingLost:
				out = append(out, fmt.Sprintf("Slot %d closes after %s.", s.Number, r.Job))
			default:
				out = append(out, fmt.Sprintf("Closing slot %d.", s.Number))
			}
		}
	}
	return strings.Join(out, " ")
}

// Hub is the Hub line: its URL and whether the node is connected, or how
// long it hasn't been, with what that means.
func (v View) Hub(url string) string {
	if v.Node.Connected {
		return url + " · Connected"
	}
	return fmt.Sprintf("%s · not connected for %s. Running jobs carry on. A finished runner can't be replaced until it's back.",
		url, Duration(v.Clock.Now.Sub(v.HubSince)))
}

// Paused says how long a pause lasts, from the settings a command left:
// "Paused until Fri 7:30 AM." or "Paused until you resume it.", adding
// that auto-resume is off when that's why.
func (v View) Paused(a availability.Node) string {
	s := Sentence("Paused " + v.until(a))
	if a.Intent == availability.PausedIndefinitely && !a.AutoResume.On {
		s += " Auto-resume is off."
	}
	return s
}

// StopConfirm is Emergency stop's confirmation, naming the jobs it
// cancels, and saying so when there are none.
func (v View) StopConfirm() string {
	jobs := v.RunningJobs()
	if len(jobs) == 0 {
		return fmt.Sprintf("No job is running, so Stop now cancels nothing, and %s stays paused until you resume it.", v.Name)
	}
	them := "it"
	if len(jobs) > 1 {
		them = "them"
	}
	return fmt.Sprintf("Stop now cancels %s. GitHub marks %s failed, and %s stays paused until you resume it.", list(jobs), them, v.Name)
}

// StopQuestion asks for Emergency stop's confirmation.
func (v View) StopQuestion() string {
	if len(v.RunningJobs()) == 0 {
		return fmt.Sprintf("Stop %s?", v.Name)
	}
	return fmt.Sprintf("Stop and cancel on %s?", v.Name)
}

// Stopped says what Emergency stop did, from the jobs it cancelled.
func (v View) Stopped(cancelled []string) string {
	if len(cancelled) == 0 {
		return fmt.Sprintf("Stopped now. No job was running. %s stays paused until you resume it.", v.Name)
	}
	return fmt.Sprintf("Stopped now, cancelling %s. %s stays paused until you resume it.", list(cancelled), v.Name)
}

// RemoveConfirm is removal's confirmation.
func (v View) RemoveConfirm() string {
	jobs := v.RunningJobs()
	first := fmt.Sprintf("%s has no job running.", v.Name)
	if len(jobs) > 0 {
		first = fmt.Sprintf("%s finishes %s first; removing never cancels a job.", v.Name, list(jobs))
	}
	return first + " Then its slot accounts are deleted and it leaves the Hub for good. Enrolling it again needs a new code."
}

// RemoveQuestion asks for removal's confirmation.
func (v View) RemoveQuestion() string { return fmt.Sprintf("Remove %s from the Hub?", v.Name) }

// ForceRemoveConfirm is the confirmation of a removal without the Hub,
// which lists what it does here and says the Hub's record stays.
func (v View) ForceRemoveConfirm() []string {
	stop := "Stop now, which cancels no job, since none is running."
	if jobs := v.RunningJobs(); len(jobs) > 0 {
		stop = fmt.Sprintf("Stop now, which cancels %s.", list(jobs))
	}
	return []string{
		fmt.Sprintf("remove --force removes %s here, without the Hub. It will:", v.Name),
		"  " + stop,
		"  Reset every slot, and delete its slot accounts.",
		"  Wipe its node key and its Hub's address.",
		fmt.Sprintf("The Hub keeps its record of %s: remove it in the Hub too. Enrolling again needs a new code.", v.Name),
	}
}

// ForceRemoveQuestion asks for a forced removal's confirmation.
func (v View) ForceRemoveQuestion() string {
	return fmt.Sprintf("Remove %s without the Hub?", v.Name)
}

// intent is the intent as a sentence ends with it: "on duty", "paused
// until Fri 7:30 AM", "paused until you resume it".
func (v View) intent(a availability.Node) string {
	if a.Intent == availability.OnDuty {
		return "on duty"
	}
	return "paused " + v.until(a)
}

// Settings sums up the settings a command saw: the intent, auto-resume,
// the slot limit and, but on a Mac, the budget.
func (v View) Settings(a availability.Node) []string {
	lines := []string{
		fmt.Sprintf("%s is %s.", v.Name, v.intent(a)),
		Sentence("auto-resume is " + AutoResume(a.AutoResume)),
		fmt.Sprintf("The slot limit is %d.", a.SlotLimit),
	}
	if !v.Node.Machine.MacOS {
		lines = append(lines, fmt.Sprintf("The budget is %s.", BudgetHalves(a.Budget)))
	}
	return lines
}

// IntentNow says what the intent is after a change: "build-pc is now paused
// until you resume it."
func (v View) IntentNow(a availability.Node) string {
	return fmt.Sprintf("%s is now %s.", v.Name, v.intent(a))
}

// Changed says what changed between two readings of the settings, for a
// command refused because it was based on the earlier one.
func (v View) Changed(before, after availability.Node) []string {
	var lines []string
	if before.Intent != after.Intent || !before.ResumeAt.Equal(after.ResumeAt) {
		lines = append(lines, v.IntentNow(after))
	}
	if AutoResume(before.AutoResume) != AutoResume(after.AutoResume) {
		lines = append(lines, Sentence("auto-resume is now "+AutoResume(after.AutoResume)))
	}
	if before.SlotLimit != after.SlotLimit {
		lines = append(lines, fmt.Sprintf("The slot limit is now %d.", after.SlotLimit))
	}
	if before.Budget != after.Budget {
		lines = append(lines, fmt.Sprintf("The budget is now %s.", BudgetHalves(after.Budget)))
	}
	if len(lines) == 0 {
		lines = append(lines, "It was changed and changed back.")
	}
	return lines
}

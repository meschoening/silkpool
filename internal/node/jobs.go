package node

import (
	"fmt"
	"strings"
	"time"
)

// What the node records of each job is in "Define Silkpool's events,
// audit trail, notices and redaction".
// A job is recorded once, when it ends, after the slot's reset has
// copied its runner's logs, so its entry can carry the run they name.

// A Job is one job a runner held, as the node records it when the job
// ends.
type Job struct {
	// Name is the job's name, from the listener's "Running job:" line.
	Name   string
	Slot   int
	Runner string
	// Started is when the node heard the listener report the job, and
	// Ended when it heard the listener exit, or found it dead.
	Started, Ended time.Time
	Outcome        Outcome
	// Result is GitHub's result, from the listener's own line, for a
	// finished job whose listener said, or empty. Interrupted and
	// cancelled jobs keep Silkpool's outcome: GitHub only calls them
	// Failed.
	Result Result
	// Run is the job's workflow run, as the copied Worker log names it,
	// if it's a run of the node's target. Otherwise it's zero.
	Run Run
	// OutOfMemory reports that the OS ended a process of the job's runner
	// because memory ran out, and MemoryBudget is the memory budget in
	// force then, in bytes: what the slots ran out of, or 0 if they had
	// none and the machine ran out. Because the budget is shared, the
	// process may not have been the one using the most.
	OutOfMemory  bool
	MemoryBudget int64
}

// An Outcome is how a job ended, as the node saw it.
type Outcome int

const (
	// JobFinished ran to its end.
	JobFinished Outcome = iota + 1
	// JobInterrupted ended because its runner died while holding it.
	JobInterrupted
	// JobCancelled was cancelled by Emergency stop.
	JobCancelled
)

func (o Outcome) String() string {
	switch o {
	case JobFinished:
		return "finished"
	case JobInterrupted:
		return "interrupted"
	case JobCancelled:
		return "cancelled"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// A Result is GitHub's result for a job, as the listener's line
// `Job <name> completed with result: <result>` gives it.
type Result string

// The results the listener reports.
const (
	ResultSucceeded           Result = "Succeeded"
	ResultSucceededWithIssues Result = "SucceededWithIssues"
	ResultFailed              Result = "Failed"
	ResultCanceled            Result = "Canceled"
	ResultSkipped             Result = "Skipped"
	ResultAbandoned           Result = "Abandoned"
)

// known reports whether r is one of the results the listener reports.
// A job can write the listener's line itself, so nothing else is kept.
func (r Result) known() bool {
	switch r {
	case ResultSucceeded, ResultSucceededWithIssues, ResultFailed, ResultCanceled, ResultSkipped, ResultAbandoned:
		return true
	}
	return false
}

// A Run is a workflow run, from the `github` context the Worker log
// dumps. The zero Run is none.
type Run struct {
	// Repository is the run's repository, as owner/name.
	Repository string
	// ID is the run's numeric ID, and Number its number in the
	// repository, as in "#262".
	ID, Number int64
}

// Link returns the run's page on GitHub, built from its repository and
// numeric ID alone, or "" for the zero Run. It links the run, not the
// job: GitHub documents no way from a runner to its job.
func (r Run) Link() string {
	if r == (Run{}) {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/actions/runs/%d", r.Repository, r.ID)
}

// A Scope is what a target covers, which tells the node's own runs from
// others': one repository, or every repository of an organization. The
// zero Scope covers nothing.
type Scope struct {
	// Owner is the repository's owner, or the organization.
	Owner string
	// Repo is the repository's name, or empty for an organization
	// target.
	Repo string
}

// covers reports whether s covers repository, given as owner/name,
// ignoring case as GitHub does. A repository whose name GitHub wouldn't
// accept is covered by nothing, so a forged log can't steer a link
// anywhere but to a run of the node's own target.
func (s Scope) covers(repository string) bool {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || s.Owner == "" || !strings.EqualFold(owner, s.Owner) || !validName(owner, "-") ||
		!validName(name, "-._") || name == "." || name == ".." {
		return false
	}
	return s.Repo == "" || strings.EqualFold(name, s.Repo)
}

// validName reports whether name is made of ASCII letters, digits and
// the runes in extra, and isn't empty.
func validName(name, extra string) bool {
	for _, r := range name {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune(extra, r)) {
			return false
		}
	}
	return name != ""
}

// jobEnded keeps the job s's runner held, which has just ended, until
// the slot's reset has copied the runner's logs. A job whose listener
// reported its result finished, even if Emergency stop came after; one
// Emergency stop cancelled before that was cancelled; and otherwise a
// listener that exited cleanly finished its job, and one that didn't, or
// was found dead, interrupted it.
func (a *applying) jobEnded(s *Slot, clean bool) {
	r := s.Runner
	j := Job{Name: r.Job, Slot: s.Number, Runner: r.Name, Started: r.JobSince, Ended: a.now, Result: r.Result}
	switch {
	case r.Result != "":
		j.Outcome = JobFinished
	case r.Retirement == Cancelling:
		j.Outcome = JobCancelled
	case clean:
		j.Outcome = JobFinished
	default:
		j.Outcome = JobInterrupted
	}
	s.Ended = j
}

// jobRecorded records the job s's last runner held, with run if it's a
// run of the node's target, now that the slot's logs are copied or its
// reset has ended without the copy reporting. An interrupted job raises
// a notice, which carries the job and its run.
func (a *applying) jobRecorded(s *Slot, run Run) {
	j := s.Ended
	if j.Name == "" {
		return
	}
	s.Ended = Job{}
	if run.ID > 0 && a.n.Scope.covers(run.Repository) {
		j.Run = run
		j.Run.Number = max(run.Number, 0)
	}
	a.happened(j)
	if j.Outcome == JobInterrupted {
		a.raise(Notice{Kind: NoticeInterruptedJob, Slot: j.Slot, Detail: j.Name, Job: j})
	}
}

package node

import (
	"time"

	"github.com/meschoening/silkpool/internal/availability"
)

// An Event is something the agent learns: a command, the clock moving
// on, its own start, a message from the Hub, or news of a slot or a
// runner. Events about a slot or a runner that no longer match what's
// under way are ignored.
type Event interface{ event() }

// Command is a command from any origin: an availability event other
// than Tick, applied under that package's rules and the node's own. The
// node applies commands one at a time, in the order they arrive, and
// answers each with a Reply once the node that results is saved.
type Command struct {
	// ID is set where the command starts, and makes it idempotent: a
	// duplicate within KeepReplies gets the original reply and changes
	// nothing. Any base revision is the event's own.
	ID     string
	Origin Origin
	Event  availability.Event
}

// Remove starts the node's removal, which can't be undone: the node is
// Paused indefinitely, every command but Emergency stop is refused, and
// once every slot is reset and its account deleted, the node wipes its
// Hub state. It's answered with a Reply, like a Command.
type Remove struct {
	ID     string
	Origin Origin
	// Local reports that it's the node's own remove, run by an
	// administrator, rather than the Hub's command. Without Force it's
	// refused while the node isn't connected, and otherwise asks the Hub
	// with RemoveAtHub, so the Hub starts the removal by sending Remove
	// back with the same ID and origin.
	Local bool
	// Force removes the node without the Hub: an Emergency stop, then
	// the wipe once every slot is cleaned up, reporting what couldn't be.
	// The Hub keeps its record of the node. Force needs Local.
	Force bool
}

// Dismiss dismisses a notice about something that happened, from any
// surface: one dismissal clears it everywhere, as the node's report
// carries its notices. It's answered with a Reply, like a Command, and
// refused for a notice about a condition. It isn't history.
type Dismiss struct {
	ID     string
	Origin Origin
	// Notice is the notice's ID.
	Notice int64
}

// Enroll is the machine's own enroll, run by an administrator with an
// enrollment code. It's refused while the machine is enrolled, and when
// Zone can't be used, before the Hub hears of it. Otherwise the node
// asks the agent to enroll it at the Hub with EnrollAtHub, and the reply
// waits for the answer: EnrollAccepted, or EnrollFailed, in which case
// nothing is saved. The code, the name and the node key are the
// agent's: the core never sees them.
type Enroll struct {
	ID     string
	Origin Origin
	// Zone is the OS's time zone, loaded by name when the enroll arrives.
	Zone *time.Location
}

// EnrollAccepted tells a machine that isn't enrolled that the Hub
// accepted its Enroll. It becomes a node as New makes it, in Zone, the
// zone EnrollAtHub carried, with Target, the Hub's default target.
// Enrolling also checks its slot, which creates the slot's account and
// runs the exposure check: a hit doesn't undo the enrollment, but shows
// on the slot.
type EnrollAccepted struct {
	ID     string
	Zone   *time.Location
	Target string
	// Machine is what the agent knows of the machine, as Started has it.
	Machine Machine
}

// EnrollFailed tells a machine that isn't enrolled that its Enroll
// failed, naming the rule: the Hub refused it, or the agent couldn't
// reach the Hub, or the Hub's key didn't match the code's pin. Nothing is
// saved.
type EnrollFailed struct {
	ID     string
	Reason error
}

// HubSettings relays the node's Hub-owned settings, which aren't
// commands and carry no revision. Target names the node's target, or is
// empty if it has none, and Scope is what it covers. RunnerApp is the
// runner application its target runs, with its SHA-256 for the node's
// platform: while it isn't installed, the node asks for its install with
// InstallRunnerApp.
type HubSettings struct {
	Target    string
	Scope     Scope
	RunnerApp RunnerApp
}

// InstallReady reports that the verified install of a version of the
// runner application is ready, extracted under a temporary name and
// renamed into place. Unless the Hub has since moved on to another
// version, it's the install new runners start from: every idle runner on
// another version is retired DELETE-first, and its slot's next runner
// starts from this one. A runner holding a job finishes it, and a 422
// leaves a runner to its one job. An install the node doesn't use is
// deleted.
type InstallReady struct{ Version string }

// InstallFailed reports that the install of a version of the runner
// application failed, naming why: a full disk, a network error, or a hash
// mismatch. It's retried from FirstInstallRetry to MaxInstallRetry. While
// an older install still works, new runners keep starting from it, and
// the node shows a NoticeUpdateFailed. Without one, the slots waiting on
// it fail with PrepareFailed.
type InstallFailed struct{ Version, Reason string }

// Tick tells the node that the clock reads now. The agent applies one at
// every WakeAt.
type Tick struct{}

// Started tells the node that the agent has just started, and comes
// before any command. Alive names the runners whose listeners are still
// running; a runner missing from it died, holding a job if it had one.
// Installs are the versions of the verified installs of the runner
// application the agent finds: new runners start from the newest, and
// any other that no live runner uses is deleted.
// Machine is what the agent knows of the machine, which bounds a budget.
// Just before Started, the agent may apply what a dead runner's log
// tells it, such as a JobCompleted and a ListenerExited with code 0 for
// one that finished its job while the agent was down.
type Started struct {
	Alive, Installs []string
	Machine         Machine
}

// Connected tells the node it has just become connected to its Hub, so
// whatever waits on the Hub is tried again now, and the outbox is
// delivered again.
type Connected struct{}

// Disconnected tells the node its connection to the Hub has ended.
type Disconnected struct{}

// Removed tells the node that the Hub answered its connection with
// "removed": the Hub has removed it for good. The node removes itself as
// a Remove would, and then wipes without confirming.
type Removed struct{}

// Acknowledged tells the node the Hub has stored every outbox entry up
// to and including Through.
type Acknowledged struct{ Through int64 }

// Prepared reports that a slot passed its checks and can get a runner.
type Prepared struct{ Slot int }

// PrepareFailed reports that a slot failed its checks: Reason is
// SlotExposed, RunnerAppUnavailable or SlotCantStart.
type PrepareFailed struct {
	Slot   int
	Reason Reason
	Detail string
}

// Granted reports that the Hub granted a slot a runner, and its JIT
// config, which the agent keeps.
type Granted struct {
	Slot   int
	Runner string
}

// GrantFailed reports that the Hub couldn't grant a slot a runner:
// Reason is GitHubRefused or GitHubUnavailable. The agent applies one
// itself when the request can't reach the Hub: with HubUnreachable, or,
// while the Hub refuses the agent's connection, VersionMismatch,
// HubIdentityChanged or WrongNodeKey.
type GrantFailed struct {
	Slot   int
	Reason Reason
	Detail string
}

// StartFailed reports that a runner's listener couldn't be started at
// all: Reason is RunnerAppUnavailable or SlotCantStart.
type StartFailed struct {
	Runner string
	Reason Reason
	Detail string
}

// Listening reports that a runner's listener is listening for a job.
type Listening struct{ Runner string }

// JobStarted reports that a runner is running a job.
type JobStarted struct{ Runner, Job string }

// WorkerStarted reports that the worker of a runner's job is up: a
// process among the runner's own runs the runner application's
// Runner.Worker. The agent looks for it from JobStarted until it's
// found or the listener exits, and reports it once, and again after
// the agent restarts for a live runner whose worker is up. Until it
// comes, a graceful stop of the runner holds its Interrupt, for at most
// WorkerWait.
type WorkerStarted struct{ Runner string }

// JobCompleted relays the listener's own line that a runner's job
// completed, `Job <name> completed with result: <result>`, with GitHub's
// result. A job can write the line itself, but only to change its own
// entry. A result the node doesn't know is ignored, and so is one that
// comes after Emergency stop cancelled the job.
type JobCompleted struct {
	Runner string
	Result Result
}

// ListenerExited reports that a runner's listener has exited with Code.
// ExitVersionRefused with no job means GitHub refused the runner's
// version: the slot fails with RunnerAppUnavailable, and the node tells
// the Hub through its outbox. OutOfMemory reports that the OS ended a
// process of the runner's because memory ran out, which the job it held
// records.
type ListenerExited struct {
	Runner      string
	Code        int
	OutOfMemory bool
}

// Retired relays the result of the Hub's DELETE of a runner's
// registration, whoever asked for it. Status is GitHub's HTTP status, or
// 0 if the Hub or GitHub couldn't be reached. TargetVisible reports that
// the Hub can still see the runner's target, without which a 404 doesn't
// mean the runner is gone.
type Retired struct {
	Runner        string
	Status        int
	TargetVisible bool
}

// LogsCopied reports that a slot's reset has copied its last runner's
// `_diag` logs, and Run is the workflow run the Worker log names, or zero
// if it names none or the copy failed. The reset copies them before
// anything else, and the agent applies LogsCopied on every attempt,
// before the attempt's SlotReset or ResetFailed. A job the slot's runner
// held is recorded then, or when the reset ends if the copy never
// reported.
type LogsCopied struct {
	Slot int
	Run  Run
}

// SlotReset reports that a slot has been reset.
type SlotReset struct{ Slot int }

// ResetFailed reports that a slot's reset failed.
type ResetFailed struct {
	Slot   int
	Detail string
}

// AccountDeleted reports that a slot's account has been deleted.
type AccountDeleted struct{ Slot int }

// DeleteFailed reports that a slot's account couldn't be deleted.
type DeleteFailed struct {
	Slot   int
	Detail string
}

// StopLostRunners relays the Hub's "stop a lost target's runners": the
// Hub can no longer see their target, so it can't delete their
// registrations. Each listener is interrupted, busy or not, and killed
// after KillAfter. The intent is left alone.
type StopLostRunners struct{ Runners []string }

func (Command) event()         {}
func (Remove) event()          {}
func (Dismiss) event()         {}
func (Enroll) event()          {}
func (EnrollAccepted) event()  {}
func (EnrollFailed) event()    {}
func (HubSettings) event()     {}
func (InstallReady) event()    {}
func (InstallFailed) event()   {}
func (Tick) event()            {}
func (Started) event()         {}
func (Connected) event()       {}
func (Disconnected) event()    {}
func (Removed) event()         {}
func (Acknowledged) event()    {}
func (Prepared) event()        {}
func (PrepareFailed) event()   {}
func (Granted) event()         {}
func (GrantFailed) event()     {}
func (StartFailed) event()     {}
func (Listening) event()       {}
func (JobStarted) event()      {}
func (WorkerStarted) event()   {}
func (JobCompleted) event()    {}
func (ListenerExited) event()  {}
func (Retired) event()         {}
func (LogsCopied) event()      {}
func (SlotReset) event()       {}
func (ResetFailed) event()     {}
func (AccountDeleted) event()  {}
func (DeleteFailed) event()    {}
func (StopLostRunners) event() {}

// An Effect is something Apply asks of the agent, beyond keeping the node
// it returns and reporting it to the Hub.
type Effect interface{ effect() }

// A Reply answers a Command, a Remove or a Dismiss, once the node that
// results is saved. Applied doesn't mean its effects are done: a drain shows in the
// node's reports.
type Reply struct {
	ID string
	// At is when the command was applied.
	At time.Time
	// Refused names the rule that refused the command, or is nil if it
	// applied. availability.ErrStale means it was based on an
	// out-of-date revision.
	Refused error
	// Settings are the node's settings after the command: with the new
	// revision if it applied, and with the current one if it was
	// refused.
	Settings availability.Node
}

// AutoResumed reports that a resume time ended a Paused intent.
type AutoResumed struct{ At time.Time }

// PrepareSlot asks the agent to check a slot before it gets a runner,
// including the exposure check run as its slot account, and to apply
// Prepared or PrepareFailed. A newly enrolled node asks for one too,
// while paused, and the agent creates the slot's account first. A slot
// needs an install of the runner application: while the node has none,
// its preparation waits for the first one, which the agent applies as
// InstallReady before Prepared, and a failed download is PrepareFailed
// with RunnerAppUnavailable. Meanwhile the slot stays preparing, and the
// node reads Starting, because nothing has failed. The agent prepares
// the slot from whichever install ends: one the node discards, because
// the Hub has moved on, gets the slot no runner. It's prepared again
// while the node's own install is under way, and fails with
// RunnerAppUnavailable otherwise.
type PrepareSlot struct{ Slot int }

// RequestRunner asks the agent to request a runner for a slot from the
// Hub, and to apply Granted or GrantFailed.
type RequestRunner struct{ Slot int }

// StartListener asks the agent to start a granted runner's listener in a
// slot, from a root linked to the install of Version, then to report it
// with Listening, JobStarted and ListenerExited, or with StartFailed.
type StartListener struct {
	Slot    int
	Runner  string
	Version string
}

// RetireRunner asks the agent to have the Hub delete a runner's
// registration, and to apply the result as Retired.
type RetireRunner struct {
	Slot   int
	Runner string
}

// Interrupt asks the agent to interrupt a runner's listener gracefully.
type Interrupt struct {
	Slot   int
	Runner string
}

// Kill asks the agent to kill a runner's listener.
type Kill struct {
	Slot   int
	Runner string
}

// ResetSlot asks the agent to reset a slot, first copying its last
// runner's logs and applying LogsCopied, then to apply SlotReset or
// ResetFailed.
type ResetSlot struct{ Slot int }

// Deliver asks the agent to send the Hub outbox entries, in order. The
// Hub answers with the sequence number it has stored through, which the
// agent applies as Acknowledged.
type Deliver struct{ Entries []Entry }

// RemoveAtHub asks the agent to pass the node's own remove to the Hub.
type RemoveAtHub struct {
	ID     string
	Origin Origin
}

// EnrollAtHub asks the agent to enroll the machine at the Hub its code
// names, and to apply the answer as EnrollAccepted or EnrollFailed.
// EnrollAccepted brings back Zone, the zone the node checked.
type EnrollAtHub struct {
	ID     string
	Origin Origin
	Zone   *time.Location
}

// InstallRunnerApp asks the agent to download the runner application's
// package of Version for its platform from actions/runner's releases on
// GitHub, from a URL it builds itself, check it against SHA256 before
// extracting anything, and install it. The download runs in the
// background, and the agent applies InstallReady or InstallFailed.
type InstallRunnerApp struct{ Version, SHA256 string }

// DeleteInstall asks the agent to delete the install of a version of the
// runner application, which no runner uses any more.
type DeleteInstall struct{ Version string }

// DeleteAccount asks the agent to delete a slot's account, and to apply
// AccountDeleted or DeleteFailed.
type DeleteAccount struct{ Slot int }

// NotCleanedUp reports, during a forced removal, that a slot couldn't be
// reset or its account deleted. The removal goes on regardless.
type NotCleanedUp struct {
	Slot   int
	Detail string
}

// Wipe asks the agent to wipe its Hub state: the node key, the Hub's
// URL and pin, the node ID, and the node itself, with every install of
// the runner application. Apply returns a node
// that isn't enrolled.
type Wipe struct{}

// ConfirmRemoved asks the agent to tell the Hub, after the wipe, that
// the node is removed.
type ConfirmRemoved struct{}

// WakeAt asks the agent to apply a Tick at At. It comes last, whenever
// the node has something to do later, and replaces any earlier WakeAt.
type WakeAt struct{ At time.Time }

func (Reply) effect()            {}
func (AutoResumed) effect()      {}
func (PrepareSlot) effect()      {}
func (RequestRunner) effect()    {}
func (StartListener) effect()    {}
func (RetireRunner) effect()     {}
func (Interrupt) effect()        {}
func (Kill) effect()             {}
func (ResetSlot) effect()        {}
func (Deliver) effect()          {}
func (RemoveAtHub) effect()      {}
func (EnrollAtHub) effect()      {}
func (InstallRunnerApp) effect() {}
func (DeleteInstall) effect()    {}
func (DeleteAccount) effect()    {}
func (NotCleanedUp) effect()     {}
func (Wipe) effect()             {}
func (ConfirmRemoved) effect()   {}
func (WakeAt) effect()           {}

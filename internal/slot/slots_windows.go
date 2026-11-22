package slot

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winexposure"
	"github.com/meschoening/silkpool/internal/winslot"
)

// Slots is a Windows machine's slots (ADRs 0005 and 0006, as #214
// amended them). Each slot's runner runs under its slot's service, in
// its slot host's job object (internal/winslot), as the machine's one
// service account (internal/winaccount), and outlives the agent. Its
// methods are safe for concurrent use, but not on the same slot.
type Slots struct {
	Layout
	// Jobs is the node job, as the agent keeps it, which the slots' budget
	// and their runners' memory go through, or nil while there's none:
	// then the slots hold to no budget, and their runners never ran out.
	Jobs Jobs
	// services is the service manager the slots' hosts run under
	// (winslot.SCM).
	services Hosts
	// machine is what the clean reads and changes of Windows (local).
	machine Machine
	// poll is how often the reset looks again at a host it ended, and at
	// a profile it waits for Windows to unload.
	poll time.Duration
	// hostWait is how long the reset waits for a slot's host to stop once
	// it's told to end its job, and profileWait how long the clean waits
	// for Windows to unload the account's profile.
	hostWait, profileWait time.Duration

	mu sync.Mutex
	// resets is closed when a slot's reset next finishes, or nil if
	// nobody waits for one.
	resets chan struct{}
	// account is held while the service account or its records change:
	// as a reset marks it for its clean, and throughout the clean.
	// Whoever holds use too takes use first.
	account sync.Mutex
	// use is held for reading while a runner starts or runs as the
	// service account, and for writing by the account-wide clean, which
	// kills every process of the account.
	use sync.RWMutex
	// clock is the time, or nil for time.Now, and cleaning stands in for
	// the account-wide clean's steps, or is nil for them. Only tests set
	// them.
	clock    func() time.Time
	cleaning func(context.Context) error

	// surveying is held while the shared places are worked out, so that
	// surveys take turns. resurveying is set while a survey a check
	// started runs, and surveys waits for those.
	surveying   sync.Mutex
	resurveying atomic.Bool
	surveys     sync.WaitGroup
	// view, logon and profile stand in for what the exposure check
	// reads of the machine, its logon as the service account, and where
	// the account's profile is, or are nil for the machine's own. Only
	// tests set them.
	view    func() (winexposure.View, error)
	logon   func(account, sid string) (windows.Token, error)
	profile func(sid string) (string, error)
	// scope, if set, is the folders a survey walks instead of the
	// machine's (View.Request), so a privileged test walks a scoped root
	// on the VM rather than the whole system drive. Only tests set it.
	scope []string
	// keep makes or checks the service account (winaccount.Keep, on
	// machine), and programs reads what runs in a slot's job
	// (winslot.Programs), or are nil for those. Only tests set them.
	keep     func() error
	programs func(job string) ([]string, error)
	// pruning keeps the prunes of the slot hosts' copies to one at a
	// time.
	pruning sync.Mutex
}

// Hosts is what the slots read and ask of their services: winslot.SCM
// on a node. Its State, Program and SetProgram are winslot.Services, for
// the rules that move a service between the slot hosts' copies.
type Hosts interface {
	winslot.Services
	// Create makes the slot service name, running program in slot-host
	// mode as LocalSystem.
	Create(name, program string) error
	// Start starts the service name's host with the runner spec.
	Start(name string, spec winslot.Spec) error
	// Stop tells the service name's host to stop its listener gracefully
	// (winslot.ControlCtrlC), and Kill to end its listener's job at once
	// (winslot.ControlKill).
	Stop(name string) error
	Kill(name string) error
	// Exit returns how the service name's last host ended, once it has
	// stopped: the listener's exit code, or the host's own failure.
	Exit(name string) (uint32, error)
	// Delete deletes the service name, if it's there.
	Delete(name string) error
}

// Jobs is what the slots ask of the node job the agent keeps
// (winslot.NodeJob, and its Watch).
type Jobs interface {
	// SetBudget holds the slots to the node's budget.
	SetBudget(b availability.Budget) error
	// OutOfMemory reports whether a process in the slot job named job has
	// tried to commit memory past the budget since it was last asked, and
	// forgets it.
	OutOfMemory(job string) bool
}

// Machine is what the account-wide clean reads and changes of Windows:
// the account, through winaccount, and what the account leaves on the
// machine. Accounts are named by their SIDs, as strings.
type Machine interface {
	winaccount.Machine
	// Tasks returns the paths, such as \folder\name, of the scheduled
	// tasks that run as sid. EndTask ends the running instance of one,
	// through Task Scheduler, if it runs, and DeleteTask deletes one.
	Tasks(sid string) ([]string, error)
	EndTask(path string) error
	DeleteTask(path string) error
	// Pids returns the IDs of the live processes running as sid, but
	// this one, and Terminate ends one. A process that has ended already
	// is ended.
	Pids(sid string) ([]uint32, error)
	Terminate(pid uint32) error
	// ProfileLoaded reports whether Windows has sid's profile loaded.
	ProfileLoaded(sid string) (bool, error)
	// RecycleBins returns sid's own folders in the Recycle Bins of the
	// machine's fixed drives, $Recycle.Bin\<sid>, that are there.
	RecycleBins(sid string) ([]string, error)
	// Sweep empties the shared places of what sid owns
	// (winexposure.Sweep).
	Sweep(ctx context.Context, sid string, places []winexposure.Place) error
}

// Bounds of the reset and the clean on a node.
const (
	// hostWait is how long a slot's host may take to stop once told to
	// end its job: it waits up to 30 s for the job to empty.
	hostWait = time.Minute
	// profileWait is how long Windows may take to unload the account's
	// profile once nothing of the account runs.
	profileWait = time.Minute
)

// Open returns the slots l lays out, on this machine.
func Open(l Layout) *Slots {
	return &Slots{
		Layout:      l,
		services:    winslot.SCM{},
		machine:     local{},
		poll:        200 * time.Millisecond,
		hostWait:    hostWait,
		profileWait: profileWait,
	}
}

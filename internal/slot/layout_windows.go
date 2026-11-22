package slot

import (
	"path/filepath"
	"strconv"

	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winslot"
)

// A Layout is where a Windows node keeps its slots' folders, its state
// and its runners' logs, and the names of the service account and the
// slots' services. Tests use their own, so that what they make doesn't
// meet another test's.
type Layout struct {
	// Account is the machine's service account, which every slot runs as
	// (#214): winaccount.Service on a node.
	Account winaccount.Spec
	// Root is Silkpool's folder, C:\ProgramData\Silkpool on a node. Slot
	// n's folder is Root\slots\n, and the agent's state is Root\state,
	// which only SYSTEM and Administrators may open.
	Root string
	// Logs is the agent's log folder. Each runner's logs go to its folder
	// there, Logs\runners\<runner>. No slot may see it.
	Logs string
	// Service starts the name of each slot's service: slot n's is
	// Service followed by n (ServiceName).
	Service string
	// NodeJob names the node job the agent keeps, which each slot host
	// nests its slot's job in (winslot.NodeJobName on a node).
	NodeJob string
	// Program is the slot hosts' copy of silkpool.exe for the agent's
	// version, Root\slot-host\<version>\silkpool.exe on a node
	// (winslot.Copies), which each slot's service is moved to between
	// runners.
	Program string
}

// Folder is slot n's folder, which the agent owns, holding its runner
// root and its temporary folder.
func (l Layout) Folder(n int) string { return filepath.Join(l.Root, "slots", strconv.Itoa(n)) }

// RunnerRoot is the root slot n's runners run from, one at a time,
// which the service account owns (runnerinstall.Link). The runner writes
// its settings, credentials, _diag logs and _work folder there.
func (l Layout) RunnerRoot(n int) string { return filepath.Join(l.Folder(n), "runner") }

// TempDir is slot n's own temporary folder, its runners' TEMP and TMP,
// which the reset deletes with the runner root (#214).
func (l Layout) TempDir(n int) string { return filepath.Join(l.Folder(n), "tmp") }

// RunnerLogs is the folder in the agent's logs that keeps a runner's
// listener output and the copies of its _diag logs.
func (l Layout) RunnerLogs(runner string) string { return filepath.Join(l.Logs, "runners", runner) }

// ServiceName is the name of slot n's service, whose slot host runs its
// runners (internal/winslot).
func (l Layout) ServiceName(n int) string { return l.Service + strconv.Itoa(n) }

// ListenerLog is the file in the agent's logs a runner's listener writes
// its output to, through its slot host, and that keeps it once the
// listener has ended.
func (l Layout) ListenerLog(runner string) string {
	return filepath.Join(l.RunnerLogs(runner), "listener.log")
}

// LiveLog is the file runner's listener in slot n writes as it runs,
// which on Windows is its ListenerLog.
func (l Layout) LiveLog(_ int, runner string) string { return l.ListenerLog(runner) }

// Installs is the folder in the agent's state that holds the runner
// application's installs (runnerinstall.Installs.Dir), on the same
// volume as the slots' folders, whose runner roots are hardlinked to
// them.
func (l Layout) Installs() string { return filepath.Join(l.state(), "runner") }

// Copies is the folder of the slot hosts' copies of silkpool.exe, one
// per version, Root\slot-host on a node: the folder two above Program.
func (l Layout) Copies() winslot.Copies {
	return winslot.Copies{Dir: filepath.Dir(filepath.Dir(l.Program))}
}

// state is the agent's state folder.
func (l Layout) state() string { return filepath.Join(l.Root, "state") }

// accountRecord is the file in the agent's state that records the
// service account's SID (winaccount.Keep).
func (l Layout) accountRecord() string { return filepath.Join(l.state(), winaccount.RecordFile) }

// cleanDue is the record of when the service account's clean fell due
// (dirtyFile), in the agent's state, which no slot may open.
func (l Layout) cleanDue() string { return filepath.Join(l.state(), dirtyFile) }

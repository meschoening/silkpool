package slot

import (
	"fmt"
	"path/filepath"
	"strconv"
)

// A Layout is where a Mac keeps its slots' folders, its runners' logs
// and its slots' launchd jobs, and the names it gives its service account
// and its jobs. Installed is the one the package installs. Tests use their own,
// so that what they make doesn't meet another test's.
type Layout struct {
	// Name is the name of the Mac's one service account, _silkpool,
	// which every slot's runners run as, and which its own group shares.
	Name string
	// Label starts the label of every slot's launchd job,
	// io.github.meschoening.silkpool.slot.N.
	Label string
	// Root is Silkpool's folder. The service account's home is
	// Root/home, slot N's folders are in Root/slots/N, and the account's
	// ledger and the slots' launchd jobs' plists are in Root/state, which
	// only root may open. The jobs write their output to files in
	// Root/output, which the service account may enter but not list.
	// Root and Root/slots must stay open to every account, so that the
	// service account reaches its folders.
	Root string
	// Logs is the agent's log folder. Each runner's listener output is
	// copied to its folder there, Logs/runners/<runner>, once its job has
	// ended. No slot may see it.
	Logs string
	// Launcher is the silkpool program, which starts each listener in its
	// launch mode (Launch).
	Launcher string
	// Domain is the launchd domain the slots' jobs load in: system, for
	// jobs that run as the service account.
	Domain string
	// FullDiskAccess is set where the agent runs with Full Disk Access, as
	// an installed Mac's must, which slot jobs share (#306): a job may then
	// have scheduled work with cron or at, so the clean fails closed on a
	// spool macOS refuses it, rather than counting it as nothing to remove
	// (#229). Tests whose programs can't be given it leave it unset.
	FullDiskAccess bool
}

// Installed is the layout of an installed Mac, from "Define how each
// binary is packaged, installed, upgraded and signed per OS" and ADR
// 0013.
var Installed = Layout{
	Name:           "_silkpool",
	Label:          "io.github.meschoening.silkpool",
	Root:           "/var/db/silkpool",
	Logs:           "/Library/Logs/Silkpool",
	Launcher:       "/usr/local/bin/silkpool",
	Domain:         "system",
	FullDiskAccess: true,
}

// Folder is slot n's folder, which root owns and holds its runner root,
// its temporary folder and Silkpool's records of the slot.
func (l Layout) Folder(n int) string { return filepath.Join(l.Root, "slots", strconv.Itoa(n)) }

// Home is the service account's home, which it owns, mode 0700, and
// every slot shares. The account's record names it, and launchd sets HOME
// to it, which the keychain's search list needs.
func (l Layout) Home() string { return filepath.Join(l.Root, "home") }

// RunnerRoot is the root slot n's runners run from, one at a time. The
// service account owns it, mode 0700. The runner writes its settings,
// credentials, `_diag` logs and `_work` folder there.
func (l Layout) RunnerRoot(n int) string { return filepath.Join(l.Folder(n), "runner") }

// TempDir is slot n's own temporary folder, its runners' TMPDIR, which
// the service account owns, mode 0700, so that what one slot's job leaves
// there is the slot's alone to delete (ADR 0006).
func (l Layout) TempDir(n int) string { return filepath.Join(l.Folder(n), "tmp") }

// Installs is the folder of root's alone that holds the runner
// application's installs, Root/runner/<version>, as Linux has it, which
// the operator chose for the Mac on 2026-11-09. A slot reaches an install only
// through its runner root's clones of it, so no slot may see the folder.
func (l Layout) Installs() string { return filepath.Join(l.Root, "runner") }

// RunnerLogs is the folder in the agent's logs that keeps a runner's
// listener output.
func (l Layout) RunnerLogs(runner string) string { return filepath.Join(l.Logs, "runners", runner) }

// ListenerLog is the file in the agent's logs that keeps a runner's
// listener output, copied from its job's output file.
func (l Layout) ListenerLog(runner string) string {
	return filepath.Join(l.RunnerLogs(runner), "listener.log")
}

// JobLabel is the label of slot n's launchd job.
func (l Layout) JobLabel(n int) string { return fmt.Sprintf("%s.slot.%d", l.Label, n) }

// jobs is the folder of root's alone that holds the slots' jobs' plists,
// and the exposure check's.
func (l Layout) jobs() string { return filepath.Join(l.state(), "launchd") }

// plist is the path of slot n's job's plist.
func (l Layout) plist(n int) string { return filepath.Join(l.jobs(), l.JobLabel(n)+".plist") }

// checkLabel is the label of the exposure check's launchd job, one for
// the machine, since every slot runs as its one service account.
func (l Layout) checkLabel() string { return l.Label + ".check" }

// state is the folder of root's alone that holds the agent's state.
func (l Layout) state() string { return filepath.Join(l.Root, "state") }

// ledger is the service account's ledger (roleaccount.Ledger), in the
// agent's state.
func (l Layout) ledger() string { return filepath.Join(l.state(), "service-account") }

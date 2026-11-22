package slot

import (
	"path/filepath"
	"strconv"
)

// A Layout is where a Linux node keeps its service account's home, its
// slots' folders and its runners' logs, and the names it gives the
// service account and runner units. Installed is the one the packages
// install. Tests use their own, so that what they make doesn't meet
// another test's.
type Layout struct {
	// Name is the name of the machine's service account, which every slot
	// runs as (#214), and starts the name of every runner unit,
	// silkpool-slotN-<runner>.service.
	Name string
	// Root is Silkpool's state folder. The service account's home is
	// Root/home, slot N's folder is Root/slots/N, and everything else in
	// Root is state that no slot may see. Root and Root/slots must stay
	// open to every account, so that the service account reaches its
	// home and the slots' folders.
	Root string
	// Logs is the agent's log folder. Each runner's listener output goes
	// to its folder there, Logs/runners/<runner>. No slot may see it.
	Logs string
	// Launcher is the silkpool program, which starts each listener in its
	// launch mode (Launch).
	Launcher string
	// Hidden lists more paths no slot may see: the control socket's
	// folder and, on a Hub host, the Hub's state and socket folders.
	Hidden []string
	// Slice is the slice every unit of the slots runs in, which holds them
	// to the node's budget (ADR 0014), or empty for silkpool.slice, the
	// one the packages install.
	Slice string
}

// slice is the slice the slots' units run in.
func (l Layout) slice() string {
	if l.Slice == "" {
		return "silkpool.slice"
	}
	return l.Slice
}

// Installed is the layout of an installed node, from "Define how each
// binary is packaged, installed, upgraded and signed per OS".
var Installed = Layout{
	Name:     "silkpool",
	Root:     "/var/lib/silkpool",
	Logs:     "/var/log/silkpool",
	Launcher: "/usr/bin/silkpool",
	Hidden:   []string{"/run/silkpool", "/var/lib/silkpool-hub", "/run/silkpool-hub"},
	Slice:    "silkpool.slice",
}

// Account is the name of the machine's service account, which every
// slot runs as: Name, such as silkpool.
func (l Layout) Account() string { return l.Name }

// Home is the service account's home, which it owns, mode 0700. Every
// slot shares it, so it's emptied only once every slot is idle.
func (l Layout) Home() string { return filepath.Join(l.Root, "home") }

// Folder is slot n's folder, which root owns and holds its runner root.
func (l Layout) Folder(n int) string { return filepath.Join(l.Root, "slots", strconv.Itoa(n)) }

// RunnerRoot is the root slot n's runners run from, one at a time. The
// service account owns it, mode 0700. The runner writes its settings,
// credentials, `_diag` logs and `_work` folder there.
func (l Layout) RunnerRoot(n int) string { return filepath.Join(l.Folder(n), "runner") }

// RunnerLogs is the folder in the agent's logs that keeps a runner's
// listener output.
func (l Layout) RunnerLogs(runner string) string { return filepath.Join(l.Logs, "runners", runner) }

// ListenerLog is the file a runner's listener writes its output to.
func (l Layout) ListenerLog(runner string) string {
	return filepath.Join(l.RunnerLogs(runner), "listener.log")
}

// LiveLog is the file runner's listener in slot n writes as it runs,
// which on Linux is its ListenerLog.
func (l Layout) LiveLog(_ int, runner string) string { return l.ListenerLog(runner) }

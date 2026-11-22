package slot

import "errors"

// ErrGone is Wait's error for a runner whose unit or job is gone: it
// ended, and systemd unloaded its unit with its exit code while nobody
// was waiting; on macOS, its slot's job isn't loaded or is another
// runner's; on Windows, its slot's service is gone, or its slot's last
// runner is another.
var ErrGone = errors.New("the runner's unit or job is gone")

// An Exit is how a unit's main process ended.
type Exit struct {
	// Code is its exit code, or 128 plus the signal that ended it, as a
	// shell counts it: 137 for SIGKILL. On Windows it's the listener's
	// exit code, as its slot host stopped with it.
	Code int
	// OutOfMemory reports that the kernel's OOM killer ended a process
	// of the unit while it ran, as systemd counts from the unit's
	// memory.events. systemd keeps the count after the unit's cgroup is
	// gone, while the unit stays loaded, so a kill just before the unit
	// ended, the listener's own included, still counts. On Windows it
	// reports that a process in the slot's job tried to commit memory past
	// the node's budget while it ran (winslot.Watch).
	OutOfMemory bool
}

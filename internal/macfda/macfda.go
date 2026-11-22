// Package macfda tells whether the agent has macOS's Full Disk Access,
// which its clean needs, as decided on "A Mac slot's clean can't empty
// trustd's TemporaryItems in the account's /var/folders" (#306). macOS's
// privacy protection, TCC, refuses a process without it even as root,
// with EPERM, the TemporaryItems folders in an account's /var/folders
// among much else, and a job's trustd makes one there for the service
// account. A clean that can't empty it fails closed, so without Full Disk
// Access a Mac node stops taking jobs after such a job. So before each
// runner, the agent asks whether it has Full Disk Access, and while it
// hasn't, the slot can't start, which the node shows as a Degraded
// reason, with what the operator does about it.
//
// macOS has no call that asks, so the agent opens a file TCC guards
// with Full Disk Access, which root may read otherwise: the system's own
// TCC database, Probe, which every Mac has. On the reference Mac (macOS
// 27.2.1), a root LaunchDaemon without Full Disk Access was refused it
// with EPERM, as it was every account's TemporaryItems folder, and root
// with Full Disk Access opened both.
package macfda

import (
	"errors"
	"fmt"
	"syscall"
)

// Probe is the file the agent opens, for reading, to tell whether it has
// Full Disk Access.
const Probe = "/Library/Application Support/com.apple.TCC/TCC.db"

// FromOpen returns whether the process has Full Disk Access, from what
// opening Probe for reading gave it: nil means it has, and EPERM, TCC's
// refusal, that it hasn't. Anything else can't tell, and is an error.
func FromOpen(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EPERM):
		return false, nil
	}
	return false, fmt.Errorf("can't tell whether Silkpool has Full Disk Access: %w", err)
}

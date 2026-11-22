//go:build linux || darwin

package agentos

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/meschoening/silkpool/internal/agent"
	"github.com/meschoening/silkpool/internal/node"
)

// slotsOS is what the OS needs of a Linux or macOS node's slots to link
// a runner's root: Owner is the service account's UID and primary
// group, which the root's links to its install are given.
type slotsOS interface {
	Owner(ctx context.Context) (uid, gid int, err error)
}

// installsOS is the installs' Link on Linux and macOS, which gives a
// runner's root to the account uid and gid.
type installsOS interface {
	Link(version, root string, uid, gid int) error
}

// link builds the runner's root from links to its install, the service
// account's. It returns the reason a failure fails the runner with.
func (o *OS) link(e agent.Listen) (node.Reason, error) {
	uid, gid, err := o.c.Slots.Owner(o.ctx)
	if err != nil {
		return node.SlotCantStart, fmt.Errorf("can't find the service account: %w", err)
	}
	if err := o.c.Installs.Link(e.Version, o.c.Slots.RunnerRoot(e.Slot), uid, gid); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return node.RunnerAppUnavailable, err
		}
		return node.SlotCantStart, err
	}
	return 0, nil
}

// openLog opens a listener's log at path to read it, never through a
// link.
func openLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// createLog makes, or empties, the file at path for a log's scrubbed
// copy, root's alone, never through a link.
func createLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
}

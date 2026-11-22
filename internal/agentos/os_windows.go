package agentos

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/agent"
	"github.com/meschoening/silkpool/internal/node"
)

// slotsOS is what the OS needs of a Windows node's slots to link a
// runner's root: nothing more, since the installs know the service
// account's SID.
type slotsOS interface{}

// installsOS is the installs' Link on Windows, which gives a runner's
// root to the machine's service account (runnerinstall.Installs.Link).
type installsOS interface {
	Link(version, root string) error
}

// link builds the runner's root from links to its install, the service
// account's. It returns the reason a failure fails the runner with.
func (o *OS) link(e agent.Listen) (node.Reason, error) {
	if err := o.c.Installs.Link(e.Version, o.c.Slots.RunnerRoot(e.Slot)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return node.RunnerAppUnavailable, err
		}
		return node.SlotCantStart, err
	}
	return 0, nil
}

// openLog opens a listener's log at path to read it, never through a
// link, while its slot host may still be writing it.
func openLog(path string) (*os.File, error) {
	return openPlain(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
}

// createLog makes, or empties, the file at path for a log's scrubbed
// copy, never through a link. It inherits the agent's logs' descriptor:
// SYSTEM's and Administrators' alone.
func createLog(path string) (*os.File, error) {
	return openPlain(path, windows.GENERIC_WRITE, windows.CREATE_ALWAYS)
}

// openPlain opens the file at path with access, as disposition has it,
// refusing a link, a junction or any other reparse point in its place.
// It shares it, as the slot host's own handle on a listener's log does.
func openPlain(path string, access, disposition uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("%s is a link or a folder, not a listener's log", path)
	}
	return os.NewFile(uintptr(h), path), nil
}

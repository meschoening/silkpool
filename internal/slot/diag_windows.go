package slot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/node"
)

// logOwner is the owner a log in _diag must have for the reset to copy
// it: the service account's SID.
type logOwner = *windows.SID

// copyLogs copies the _diag logs of slot n's last runner, as its record
// names it, into the runner's folder in the agent's logs, beside its
// listener's output (copyDiag), and returns the run its Worker log
// names. sid is the service account's, which must own each log. Then it
// prunes the runners' folders, as of now. Without a recorded runner or
// an account, there's nothing to copy.
func (s *Slots) copyLogs(n int, sid string, now time.Time) (node.Run, error) {
	runner := s.recordedRunner(n)
	if runner == "" || sid == "" {
		return node.Run{}, nil
	}
	owner, err := windows.StringToSid(sid)
	if err != nil {
		return node.Run{}, fmt.Errorf("the service account's SID %q: %w", sid, err)
	}
	run, err := copyDiag(s.RunnerRoot(n), s.RunnerLogs(runner), owner, diagCap)
	if err != nil {
		return node.Run{}, err
	}
	return run, s.pruneLogs(now)
}

// createCopy makes the file path, or empties it, for a log's copy. It's
// in the agent's logs, whose descriptor every file there inherits: the
// agent's and Administrators' alone.
func createCopy(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

// openDiag opens the _diag folder in the runner root root, following no
// link, or returns nil if either isn't there, is a link or a junction,
// or isn't a folder. It opens _diag by its name in the root's handle,
// so nothing can swap a link in between.
func openDiag(root string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	r, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if notThere(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("can't open %s: %w", root, err)
	}
	defer windows.CloseHandle(r)
	if ok, err := plainFolder(r); err != nil || !ok {
		return nil, err
	}
	path := filepath.Join(root, "_diag")
	d, err := openAt(r, "_diag", windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_DIRECTORY_FILE)
	if notThere(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("can't open %s: %w", path, err)
	}
	if ok, err := plainFolder(d); err != nil || !ok {
		windows.CloseHandle(d)
		return nil, err
	}
	return os.NewFile(uintptr(d), path), nil
}

// plainFolder reports whether h is a folder, not a link or a junction.
func plainFolder(h windows.Handle) (bool, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return false, err
	}
	return info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 &&
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0, nil
}

// notThere reports whether err says a name is missing, or isn't a
// folder where one was asked for: in a runner root, each is nothing to
// copy.
func notThere(err error) bool {
	for _, e := range []error{
		windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND, windows.ERROR_DIRECTORY,
		windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND,
		windows.STATUS_NOT_A_DIRECTORY, windows.STATUS_FILE_IS_A_DIRECTORY,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// openAt opens the entry name in the folder dir, with access, as
// NtCreateFile does with dir as its root: by name, following no link or
// junction there, which it opens itself. options are NtCreateFile's
// create options, such as FILE_DIRECTORY_FILE.
func openAt(dir windows.Handle, name string, access, options uint32) (windows.Handle, error) {
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// openLog opens the log name in the folder diag, following no link, and
// returns it with its size, or nil unless it's a plain file, with no
// other name, that owner owns: not a folder, a link, a junction or any
// other reparse point, nor a hardlink, which could be another's file.
func openLog(diag *os.File, name string, owner logOwner) (*os.File, int64, error) {
	h, err := openAt(windows.Handle(diag.Fd()), name, windows.FILE_GENERIC_READ, windows.FILE_NON_DIRECTORY_FILE)
	if notThere(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	ok, size, err := plainLog(h, owner)
	if err != nil || !ok {
		windows.CloseHandle(h)
		return nil, 0, err
	}
	return os.NewFile(uintptr(h), name), size, nil
}

// plainLog reports whether h is a plain file, with one name, that owner
// owns, and returns its size.
func plainLog(h windows.Handle, owner logOwner) (bool, int64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return false, 0, err
	}
	const notPlain = windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT | windows.FILE_ATTRIBUTE_DEVICE
	if info.FileAttributes&notPlain != 0 || info.NumberOfLinks != 1 {
		return false, 0, nil
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, 0, err
	}
	o, _, err := sd.Owner()
	if err != nil {
		return false, 0, err
	}
	if o == nil || !o.Equals(owner) {
		return false, 0, nil
	}
	return true, int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow), nil
}

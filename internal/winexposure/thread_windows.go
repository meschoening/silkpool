package winexposure

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procNtQueryObject = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryObject")

// shareAll lets others read, write and delete what's open, so a look
// never gets in a job's way.
const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// An Account is whom the survey and the read check act as.
type Account struct {
	// Token is a token of the account's: the S4U logon's, as the slot
	// host's listener runs with it (winslot.LogonToken), or, in tests,
	// the test's own.
	Token windows.Token
	// SID is the account's. The walk leaves the folders it owns, and the
	// sweep deletes only what it owns.
	SID *windows.SID
}

// errStuck is a thread that couldn't stop acting as someone else, which
// is never given back to Go's scheduler.
var errStuck = errors.New("the thread couldn't stop acting as someone else")

// A thread is an OS thread of its own that acts, in turn, as this
// process, through a copy of its token with SeBackupPrivilege and
// SeRestorePrivilege enabled on the thread alone, if the process holds
// them, and as the account, through an impersonation copy of its token.
// LocalSystem holds both privileges, so as itself it can list, read and
// delete anything; the account's token decides only what the account
// may do (ADR 0018).
type thread struct {
	self, account windows.Token
}

// onThread runs f on a thread of its own, whose tokens it closes after.
// account is 0 for a thread that only acts as itself. A thread that
// can't stop acting as someone else is left locked, so Go ends it once
// f's goroutine has.
func onThread(account windows.Token, f func(*thread) error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		err := act(account, f)
		if !errors.Is(err, errStuck) {
			runtime.UnlockOSThread()
		}
		done <- err
	}()
	return <-done
}

func act(account windows.Token, f func(*thread) error) (err error) {
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		return fmt.Errorf("acting as itself on a thread of its own: %w", err)
	}
	defer func() {
		if e := windows.RevertToSelf(); e != nil {
			err = errors.Join(err, fmt.Errorf("%w: %w", errStuck, e))
		}
	}()
	var t thread
	defer func() {
		for _, tok := range []windows.Token{t.self, t.account} {
			if tok != 0 {
				tok.Close()
			}
		}
	}()
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_IMPERSONATE, true, &t.self); err != nil {
		return fmt.Errorf("opening its own thread's token: %w", err)
	}
	for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege"} {
		if err := enable(t.self, name); err != nil {
			return err
		}
	}
	if account != 0 {
		if err := windows.DuplicateTokenEx(account, windows.TOKEN_IMPERSONATE|windows.TOKEN_QUERY, nil,
			windows.SecurityImpersonation, windows.TokenImpersonation, &t.account); err != nil {
			return fmt.Errorf("copying the account's token to act as it: %w", err)
		}
	}
	return f(&t)
}

// enable enables the privilege name in tok, if tok holds it. Holding it
// isn't checked: without it, LocalSystem's look is only as wide as its
// DACLs let it be, and the lanes' standard account holds neither.
func enable(tok windows.Token, name string) error {
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
		return fmt.Errorf("looking up %s: %w", name, err)
	}
	privs := windows.Tokenprivileges{PrivilegeCount: 1}
	privs.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	if err := windows.AdjustTokenPrivileges(tok, false, &privs, 0, nil, nil); err != nil {
		return fmt.Errorf("enabling %s on its own thread: %w", name, err)
	}
	return nil
}

// asSelf makes the thread act as this process.
func (t *thread) asSelf() error {
	if err := windows.SetThreadToken(nil, t.self); err != nil {
		return fmt.Errorf("acting as itself again: %w", err)
	}
	return nil
}

// asAccount makes the thread act as the account.
func (t *thread) asAccount() error {
	if err := windows.SetThreadToken(nil, t.account); err != nil {
		return fmt.Errorf("acting as the account: %w", err)
	}
	if asAccountHook != nil {
		asAccountHook()
	}
	return nil
}

// asAccountHook, which tests set, runs each time the thread starts
// acting as the account.
var asAccountHook func()

// granted returns what the account may do to what h is open on: h is
// opened again, by itself, as the account, asking for the most the
// account may have, and Windows' own access check, with the account's
// groups, privileges and integrity, says what that is. Access denied is
// nothing granted. The thread acts as itself again before it returns.
//
// A folder a program holds open without letting others delete it, as
// Steam holds its own folder as its working folder, can't be opened so
// whenever the account may delete it: the sharing check runs against
// all it may do. Then granted asks for each right the survey and the
// read check count on its own instead (asked), and counts one that
// still meets a sharing violation as granted, since nothing shows it
// isn't. A file held so stays a sharing violation, which the read check
// counts as a file it couldn't look at.
func (t *thread) granted(h windows.Handle) (access uint32, err error) {
	access, err = t.open(h, windows.MAXIMUM_ALLOWED)
	if !isAny(err, windows.ERROR_SHARING_VIOLATION, windows.STATUS_SHARING_VIOLATION) {
		return access, err
	}
	if id, idErr := identify(h); idErr != nil || !id.folder() {
		return 0, err
	}
	access = 0
	for _, right := range asked {
		got, err := t.open(h, right)
		switch {
		case isAny(err, windows.ERROR_SHARING_VIOLATION, windows.STATUS_SHARING_VIOLATION):
			access |= right
		case err != nil:
			return 0, err
		default:
			access |= got & right
		}
	}
	return access, nil
}

// asked are the rights granted asks for one by one: those the survey and
// the read check count.
var asked = []uint32{fileReadData, fileAddFile, fileAddSubdirectory, fileDeleteChild, writeDAC, writeOwner}

// open opens what h is open on again, by itself, as the account, asking
// for access, and returns what it was granted; access denied is nothing.
func (t *thread) open(h windows.Handle, access uint32) (uint32, error) {
	if err := t.asAccount(); err != nil {
		return 0, err
	}
	r, err := reopen(h, access, 0)
	if back := t.asSelf(); back != nil {
		if r != 0 {
			windows.CloseHandle(r)
		}
		return 0, back
	}
	if denied(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(r)
	return grantedAccess(r)
}

// objectBasicInformation is OBJECT_BASIC_INFORMATION.
type objectBasicInformation struct {
	Attributes, GrantedAccess, HandleCount, PointerCount uint32
	Reserved                                             [10]uint32
}

// grantedAccess returns the access h was opened with.
func grantedAccess(h windows.Handle) (uint32, error) {
	var info objectBasicInformation
	r, _, _ := procNtQueryObject.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	if r != 0 {
		return 0, fmt.Errorf("reading a handle's access: %w", windows.NTStatus(r))
	}
	return info.GrantedAccess, nil
}

// reopen opens what h is open on again, by itself, with access, as the
// thread now acts: NtCreateFile with h as its root and no name, which
// Windows checks as a new open of the same file or folder, wherever its
// path leads now. options are NtCreateFile's create options.
func reopen(h windows.Handle, access, options uint32) (windows.Handle, error) {
	var name windows.NTUnicodeString
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: h, ObjectName: &name}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var r windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	if options&windows.FILE_SYNCHRONOUS_IO_NONALERT != 0 {
		access |= windows.SYNCHRONIZE
	}
	err := windows.NtCreateFile(&r, access, &oa, &iosb, nil, 0, shareAll, windows.FILE_OPEN,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if err != nil {
		return 0, err
	}
	return r, nil
}

// openAt opens the entry name in the folder dir with access, as the
// thread now acts, as NtCreateFile does with dir as its root: by name,
// opening a reparse point there itself rather than following it, and for
// backup, which grants more only while SeBackupPrivilege or
// SeRestorePrivilege is enabled. options are NtCreateFile's create
// options, such as FILE_DIRECTORY_FILE.
func openAt(dir windows.Handle, name string, access, options uint32) (windows.Handle, error) {
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, 0, shareAll, windows.FILE_OPEN,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// openPath opens the absolute path with access, as the thread now acts,
// opening a reparse point at its end itself rather than following it,
// and for backup, which a folder needs to be opened at all. Through the
// \\?\ prefix, a path past MAX_PATH opens too.
func openPath(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(long(path))
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access|windows.SYNCHRONIZE, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// long returns path with the \\?\ prefix, which takes it as it is, past
// MAX_PATH, unless it has one or is a UNC path.
func long(path string) string {
	if len(path) >= 2 && path[0] == '\\' && path[1] == '\\' {
		return path
	}
	return `\\?\` + path
}

// fileIDInfo is FILE_ID_INFO.
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// An identity is what a handle is open on: its volume, its file ID, and
// its attributes.
type identity struct {
	volume uint64
	id     string
	attrs  uint32
}

// reparse reports whether it's a reparse point: a junction, a symbolic
// link, a mount point or any other.
func (i identity) reparse() bool { return i.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 }

// folder reports whether it's a folder, and no reparse point.
func (i identity) folder() bool { return i.attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && !i.reparse() }

// identify reads what h is open on: its 128-bit file ID where the file
// system has one, as NTFS and ReFS do, or its 64-bit file index.
func identify(h windows.Handle) (identity, error) {
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bi); err != nil {
		return identity{}, err
	}
	var fi fileIDInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileIdInfo, (*byte)(unsafe.Pointer(&fi)), uint32(unsafe.Sizeof(fi))); err == nil {
		return identity{volume: fi.VolumeSerialNumber, id: fmt.Sprintf("%x", fi.FileID), attrs: bi.FileAttributes}, nil
	}
	return identity{volume: uint64(bi.VolumeSerialNumber), id: fmt.Sprintf("%08x%08x", bi.FileIndexHigh, bi.FileIndexLow), attrs: bi.FileAttributes}, nil
}

// ownerOf returns the SID of h's owner, or nil if it has none, as on a
// drive with no permissions. h must be open with READ_CONTROL.
func ownerOf(h windows.Handle) (*windows.SID, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	o, _, err := sd.Owner()
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, nil
	}
	return o.Copy()
}

// gone reports whether err says a name is missing, is being deleted, or
// is no longer a folder where one was asked for.
func gone(err error) bool {
	return isAny(err,
		windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND, windows.ERROR_DIRECTORY, windows.ERROR_INVALID_NAME,
		windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND, windows.STATUS_DELETE_PENDING,
		windows.STATUS_NOT_A_DIRECTORY, windows.STATUS_OBJECT_NAME_INVALID)
}

// denied reports whether err is access denied.
func denied(err error) bool {
	return isAny(err, windows.ERROR_ACCESS_DENIED, windows.STATUS_ACCESS_DENIED)
}

// blocked reports whether err says something couldn't be opened for
// now, or at all, but not that it's missing: access denied, a sharing
// violation, or a file Windows can't reach, such as a cloud file's
// placeholder.
func blocked(err error) bool {
	return denied(err) || isAny(err, windows.ERROR_SHARING_VIOLATION, windows.STATUS_SHARING_VIOLATION,
		windows.ERROR_CANT_ACCESS_FILE, windows.STATUS_CANNOT_DELETE)
}

func isAny(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}

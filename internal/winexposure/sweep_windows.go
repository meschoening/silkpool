package winexposure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// lookAccess is what the sweep first opens an entry with, to read its
// owner and attributes: none of it is shared access, so a file another
// program holds open without sharing still opens. sweepAccess is what
// it opens one the account owns with again, to delete it.
const (
	lookAccess  = windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES
	sweepAccess = windows.DELETE | windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES
)

// windowsKept are the files Windows keeps at the top of a drive and
// holds open so that no one may open them at all, not even to read their
// owner: its page, swap, hibernation and dump files. The read check never
// looks at them either (SystemFolders).
var windowsKept = []string{"pagefile.sys", "swapfile.sys", "hiberfil.sys", "DumpStack.log.tmp"}

// keptByWindows reports whether the entry name at the top of a place,
// whose open to read its owner failed with err, is one of windowsKept
// that Windows holds: a sharing violation on that open. Only such an
// entry the sweep leaves without knowing whose it is.
func keptByWindows(name string, err error) bool {
	return containsFold(windowsKept, name) && isAny(err, windows.ERROR_SHARING_VIOLATION, windows.STATUS_SHARING_VIOLATION)
}

// openEntry opens an entry to read its owner: openAt, unless a test
// gives its own.
var openEntry = openAt

// openOwned opens the entry name in the folder dir, and returns it open
// for deleting if sid owns it, or 0 if it's another's or gone. It reads
// the owner before it asks for DELETE, so another's file, held open
// without delete sharing, is left without a sharing violation. An entry
// whose open to read its owner fails, but with an error skip accepts,
// counts as another's; skip may be nil.
func openOwned(dir windows.Handle, name, path string, sid *windows.SID, skip func(error) bool) (windows.Handle, bool, error) {
	c, err := openEntry(dir, name, lookAccess, 0)
	if gone(err) {
		return 0, false, nil
	}
	if err != nil && skip != nil && skip(err) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(c)
	mine, err := owned(c, sid)
	if err != nil {
		return 0, false, &os.PathError{Op: "read the owner of", Path: path, Err: err}
	}
	if !mine {
		return 0, true, nil
	}
	d, err := reopen(c, sweepAccess, windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if gone(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return d, false, nil
}

// Sweep empties each of places, as the latest survey of the account sid
// found them, of what sid owns. Every slot's jobs run as the account, so
// what it owns there may be any slot's: the caller sweeps only once every
// slot is idle (#214).
//
// It acts as LocalSystem, with SeBackupPrivilege and SeRestorePrivilege
// enabled on its own thread, so a DACL the account set on what it made
// doesn't stop it. It deletes each entry sid owns at the top of a place,
// whole, but never follows a reparse point: a junction or a link the
// account made is deleted itself, and what it leads to is left. It
// sweeps only the very folder the survey found, by its volume and file
// ID: one put in its stead since is left. Inside a folder sid owns, it
// deletes only what sid owns too: an entry sid doesn't own, another
// account may have made, or a job may have moved or linked there, so
// it's left, and fails the sweep, naming it (ADRs 0017 and 0018).
// Entries sid doesn't own at the top of a place are other accounts', and
// left, and so are the files Windows holds open at a drive's top, which
// no one may open to read their owner (keptByWindows). After a failure it sweeps the other places, and returns every
// failure.
func Sweep(ctx context.Context, sid *windows.SID, places []Place) error {
	return onThread(0, func(*thread) error {
		var errs []error
		for _, p := range places {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			if err := sweepPlace(p, sid); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}

// sweepPlace deletes what sid owns at the top of the place p, if the
// folder at its path is still the one the survey found.
func sweepPlace(p Place, sid *windows.SID) error {
	h, err := openPath(p.Path, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES)
	if gone(err) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: p.Path, Err: err}
	}
	f := os.NewFile(uintptr(h), p.Path)
	defer f.Close()
	id, err := identify(h)
	if err != nil {
		return &os.PathError{Op: "read", Path: p.Path, Err: err}
	}
	if !id.folder() || id.volume != p.Volume || id.id != p.ID {
		return nil
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return &os.PathError{Op: "list", Path: p.Path, Err: err}
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		path := Join(p.Path, name)
		c, _, err := openOwned(h, name, path, sid, func(err error) bool { return keptByWindows(name, err) })
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if c == 0 {
			continue
		}
		if err := remove(c, path, sid); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// owned reports whether sid owns what h is open on.
func owned(h windows.Handle, sid *windows.SID) (bool, error) {
	o, err := ownerOf(h)
	return err == nil && o != nil && o.Equals(sid), err
}

// remove deletes what h is open on, at path, which sid owns, and closes
// h. A folder that's no reparse point it empties first, of what sid owns,
// and keeps, failing, if anything else is in it.
func remove(h windows.Handle, path string, sid *windows.SID) error {
	defer windows.CloseHandle(h)
	id, err := identify(h)
	if err != nil {
		return &os.PathError{Op: "read", Path: path, Err: err}
	}
	if id.folder() {
		if err := empty(h, path, sid); err != nil {
			return err
		}
	}
	if err := deleteOpen(h); err != nil && !gone(err) {
		return &os.PathError{Op: "delete", Path: path, Err: err}
	}
	return nil
}

// empty deletes what's in the folder h is open on, at path, which sid
// owns. It leaves what sid doesn't own, failing, naming each.
func empty(h windows.Handle, path string, sid *windows.SID) error {
	l, err := reopen(h, windows.FILE_LIST_DIRECTORY, windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(l), path)
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return &os.PathError{Op: "list", Path: path, Err: err}
	}
	var errs []error
	for _, e := range entries {
		p := Join(path, e.Name())
		c, foreign, err := openOwned(l, e.Name(), p, sid, nil)
		switch {
		case err != nil:
			errs = append(errs, err)
		case foreign:
			errs = append(errs, fmt.Errorf("%s isn't the service account's, though the folder it's in is, so the sweep left it", p))
		case c != 0:
			if err := remove(c, p, sid); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// fileDispositionInfoEx is FILE_DISPOSITION_INFO_EX, and
// fileDispositionInfo FILE_DISPOSITION_INFO.
type (
	fileDispositionInfoEx struct{ Flags uint32 }
	fileDispositionInfo   struct{ DeleteFile bool }
)

// fileBasicInfo is FILE_BASIC_INFO.
type fileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	FileAttributes                                          uint32
	_                                                       uint32
}

// deleteOpen deletes what h is open on, read-only or not, as its name
// goes at once, as POSIX deletes do. Where the file system has no POSIX
// deletes, as FAT hasn't, it clears the read-only attribute and deletes
// it the older way, as h closes.
func deleteOpen(h windows.Handle) error {
	ex := fileDispositionInfoEx{Flags: windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS |
		windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&ex)), uint32(unsafe.Sizeof(ex)))
	if !isAny(err, windows.ERROR_INVALID_PARAMETER, windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION) {
		return err
	}
	var basic fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return err
	}
	if basic.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		basic = fileBasicInfo{FileAttributes: basic.FileAttributes &^ windows.FILE_ATTRIBUTE_READONLY}
		if basic.FileAttributes == 0 {
			basic.FileAttributes = windows.FILE_ATTRIBUTE_NORMAL
		}
		if err := windows.SetFileInformationByHandle(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
			return err
		}
	}
	d := fileDispositionInfo{DeleteFile: true}
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&d)), uint32(unsafe.Sizeof(d)))
}

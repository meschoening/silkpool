package winexposure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// errNoDACL is a folder with no DACL, or a NULL one, which lets anyone
// do anything: a denial put in its stead would let no one do anything,
// so the guard leaves it.
var errNoDACL = errors.New("it has no DACL")

// put puts g's entry for sid first in the DACL of what h is open on
// (withGuard), unless it's first there already. h must be open with
// READ_CONTROL and WRITE_DAC. It sets that file or folder's DACL alone,
// through its handle, so nothing below it is changed, and no reparse
// point is followed.
func put(h windows.Handle, sid *windows.SID, g guard) error {
	return editDACL(h, func(acl []byte) ([]byte, bool, error) { return withGuard(acl, sidBytes(sid), g) })
}

// GuardReads puts the read guard for the account a's token on each of
// paths, the files and folders the read check found it could read
// (ADR 0020), telling note first. It acts as LocalSystem, with
// SeRestorePrivilege, opening each path as it is, never following a
// reparse point at its end: a reparse point, or a path that's gone or
// that it can't open, it leaves, and so does a file or folder with no
// DACL, which the read check then finds again.
func GuardReads(ctx context.Context, a Account, paths []string, note func(Place) error) error {
	user, err := tokenUser(a.Token)
	if err != nil {
		return err
	}
	return onThread(0, func(*thread) error {
		for _, p := range paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := guardRead(p, user, note); err != nil {
				return err
			}
		}
		return nil
	})
}

func guardRead(path string, user *windows.SID, note func(Place) error) error {
	h, err := openPath(path, windows.READ_CONTROL|windows.WRITE_DAC)
	switch {
	case gone(err), blocked(err):
		return nil
	case err != nil:
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	id, err := identify(h)
	if err != nil {
		return &os.PathError{Op: "read", Path: path, Err: err}
	}
	if id.reparse() {
		return nil
	}
	if err := note(Place{Path: path, Volume: id.volume, ID: id.id}); err != nil {
		return err
	}
	g := readGuardFile
	if id.folder() {
		g = readGuardFolder
	}
	return guardErr(put(h, user, g), path)
}

// GuardPlaces puts the write guard for the account a's token again on
// each of places the account can create entries in, the shared places in
// the folders of the OS's software a survey found, telling note first,
// as a check finds them:
// a program may have set its folder's DACL again since the survey
// guarded it (ADR 0020). It returns those the account can still create
// entries in, or that it couldn't show it can't: a place that's gone it
// leaves out, and one with another folder in its stead, by its volume
// serial number and file ID, it keeps, for the next survey to work out.
// With moved, the places are only ones guarded once, and a folder put in
// one's stead since, as an upgrade puts one, it keeps only if the
// account can create entries in it, or it can't tell, without guarding
// it: the path may lead elsewhere now, through a folder above put in
// another's stead, so only the next survey's walk guards it.
func GuardPlaces(ctx context.Context, a Account, places []Place, moved bool, note func(Place) error) ([]Place, error) {
	user, err := tokenUser(a.Token)
	if err != nil {
		return nil, err
	}
	var still []Place
	err = onThread(a.Token, func(t *thread) error {
		for _, p := range places {
			if err := ctx.Err(); err != nil {
				return err
			}
			kept, err := guardPlace(t, p, user, moved, note)
			if err != nil {
				return err
			}
			if kept {
				still = append(still, p)
			}
		}
		return nil
	})
	return still, err
}

func guardPlace(t *thread, p Place, user *windows.SID, moved bool, note func(Place) error) (bool, error) {
	h, err := openPath(p.Path, windows.FILE_READ_ATTRIBUTES)
	switch {
	case gone(err):
		return false, nil
	case blocked(err):
		return true, nil
	case err != nil:
		return false, &os.PathError{Op: "open", Path: p.Path, Err: err}
	}
	defer windows.CloseHandle(h)
	id, err := identify(h)
	if err != nil {
		return false, &os.PathError{Op: "read", Path: p.Path, Err: err}
	}
	if !id.folder() {
		return !moved, nil
	}
	if id.volume != p.Volume || id.id != p.ID {
		if !moved {
			return true, nil
		}
		granted, err := t.granted(h)
		return err != nil && !gone(err) || granted&creates != 0, nil
	}
	granted, err := t.granted(h)
	switch {
	case gone(err):
		return false, nil
	case blocked(err):
		return true, nil
	case err != nil:
		return false, &os.PathError{Op: "check the account's access to", Path: p.Path, Err: err}
	}
	if granted&creates == 0 {
		return false, nil
	}
	if err := note(Place{Path: p.Path, Volume: id.volume, ID: id.id}); err != nil {
		return false, err
	}
	// The very folder, opened again to set its DACL.
	g, err := reopen(h, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_DIRECTORY_FILE)
	switch {
	case gone(err):
		return false, nil
	case blocked(err):
		return true, nil
	case err != nil:
		return false, &os.PathError{Op: "open", Path: p.Path, Err: err}
	}
	err = guardErr(put(g, user, writeGuard), p.Path)
	windows.CloseHandle(g)
	if err != nil {
		return false, err
	}
	granted, err = t.granted(h)
	switch {
	case gone(err):
		return false, nil
	case blocked(err):
		return true, nil
	case err != nil:
		return false, &os.PathError{Op: "check the account's access to", Path: p.Path, Err: err}
	}
	return granted&creates != 0, nil
}

// guardErr is err, the error of putting a guard on path, but nil for
// one that has no DACL to put it in, or whose DACL can't take it: the
// check finds it again.
func guardErr(err error, path string) error {
	if err == nil || errors.Is(err, errNoDACL) || errors.Is(err, errBadACL) {
		return nil
	}
	return &os.PathError{Op: "deny the account access to", Path: path, Err: err}
}

// tokenUser is the SID of t's user, whom a guard denies.
func tokenUser(t windows.Token) (*windows.SID, error) {
	u, err := t.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("reading the account's token: %w", err)
	}
	return u.User.Sid, nil
}

// editDACL sets the DACL of what h is open on to edit's, if it says to,
// keeping whether it's protected from inheritance and whether it was
// inherited automatically as they were. Windows keeps the latter only
// if it's asked to (SE_DACL_AUTO_INHERIT_REQ), which leaves the DACL's
// inherited entries as they are.
func editDACL(h windows.Handle, edit func(acl []byte) ([]byte, bool, error)) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || err == nil && dacl == nil {
		return errNoDACL
	}
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	old := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), 8)
	acl, changed, err := edit(unsafe.Slice((*byte)(unsafe.Pointer(dacl)), aclSize(old)))
	if err != nil || !changed {
		return err
	}
	// The ACL must be DWORD-aligned, which a slice of uint32s is.
	buf := make([]uint32, (len(acl)+3)/4)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), len(acl)), acl)
	next, err := windows.NewSecurityDescriptor()
	if err != nil {
		return err
	}
	if err := next.SetDACL((*windows.ACL)(unsafe.Pointer(&buf[0])), true, false); err != nil {
		return err
	}
	kept := windows.SECURITY_DESCRIPTOR_CONTROL(windows.SE_DACL_PROTECTED | windows.SE_DACL_AUTO_INHERITED)
	set := control & kept
	if set&windows.SE_DACL_AUTO_INHERITED != 0 {
		set |= windows.SE_DACL_AUTO_INHERIT_REQ
	}
	if err := next.SetControl(kept|windows.SE_DACL_AUTO_INHERIT_REQ, set); err != nil {
		return err
	}
	err = windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION, next)
	runtime.KeepAlive(buf)
	return err
}

// sidBytes is sid, as its bytes.
func sidBytes(sid *windows.SID) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(sid)), sid.Len())
}

// Unguard takes each guard's entry for sid away from each of places, as
// surveys and checks guarded them, leaving every other entry as it was:
// the uninstall does it before it deletes the account. A file or folder
// that's gone, or another one in its place, by its volume serial number
// and file ID, it leaves. It acts as LocalSystem, with
// SeRestorePrivilege, opening each folder as it is, never following a
// reparse point at its end. Copies of the entry that files and folders
// made in a guarded folder since inherited are left: they deny an
// account that's then deleted, whose SID Windows never gives again.
func Unguard(ctx context.Context, sid *windows.SID, places []Place) error {
	return onThread(0, func(*thread) error {
		for _, p := range places {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := unguard(p, sid); err != nil {
				return err
			}
		}
		return nil
	})
}

func unguard(p Place, sid *windows.SID) error {
	h, err := openPath(p.Path, windows.READ_CONTROL|windows.WRITE_DAC)
	if gone(err) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: p.Path, Err: err}
	}
	defer windows.CloseHandle(h)
	id, err := identify(h)
	if err != nil {
		return &os.PathError{Op: "read", Path: p.Path, Err: err}
	}
	if id.reparse() || id.volume != p.Volume || id.id != p.ID {
		return nil
	}
	err = editDACL(h, func(acl []byte) ([]byte, bool, error) { return withoutGuard(acl, sidBytes(sid)) })
	if err != nil && !errors.Is(err, errNoDACL) {
		return &os.PathError{Op: "take Silkpool's denial away from", Path: p.Path, Err: err}
	}
	return nil
}

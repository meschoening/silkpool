package macos

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// userLocks are the user flags that stop even root deleting an entry:
// uchg and uappnd, which the service account may set on what it owns.
const userLocks = unix.UF_IMMUTABLE | unix.UF_APPEND

// clearUserFlags clears uchg and uappnd from dir and everything in it,
// so that os.RemoveAll can delete them. Everything there is Silkpool's by
// where it is, though a DeleteAccount that stopped once the service
// account's records were gone can have left something the account
// locked. A missing dir is nothing to clear.
//
// It follows no link: it reads and opens each entry through the folder
// above it, which it holds open, so a link is read and opened as the
// link, and a folder swapped for a link as it goes is never entered. A
// locked file with other names, or a locked socket, fails closed, naming
// it (unlockable). It leaves every other flag, such as one only root can
// set, so os.RemoveAll then fails on that entry, naming it.
func clearUserFlags(dir string) error {
	err := clearFlagsAt(unix.AT_FDCWD, dir, dir)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// clearFlagsAt clears the user locks of the entry name in the folder
// parent, at path, and of everything in it. It reads each entry's
// metadata without opening it, and opens only a folder, to go into, or
// a locked entry, to unlock: macOS won't open a socket, which a job may
// leave. It opens a folder with O_DIRECTORY and anything else as itself,
// a link as the link (O_SYMLINK), and goes on only if what it opened is
// the entry it read.
func clearFlagsAt(parent int, name, path string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	if !isDir && st.Flags&userLocks == 0 {
		return nil
	}
	if err := unlockable(&st, path); err != nil {
		return err
	}
	mode := unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC
	if isDir {
		mode |= unix.O_DIRECTORY | unix.O_NOFOLLOW
	} else {
		mode |= unix.O_SYMLINK
	}
	fd, err := unix.Openat(parent, name, mode, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var now unix.Stat_t
	if err := unix.Fstat(fd, &now); err != nil {
		return &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if now.Dev != st.Dev || now.Ino != st.Ino {
		return fmt.Errorf("%s changed as it was read, so it's left", path)
	}
	if now.Flags&userLocks != 0 {
		if err := unlockable(&now, path); err != nil {
			return err
		}
		if err := unix.Fchflags(fd, int(now.Flags&^userLocks)); err != nil {
			return &os.PathError{Op: "chflags", Path: path, Err: err}
		}
	}
	if !isDir {
		return nil
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		return &os.PathError{Op: "readdir", Path: path, Err: err}
	}
	for _, n := range names {
		if err := clearFlagsAt(fd, n, filepath.Join(path, n)); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return nil
}

// unlockable fails for a locked entry, which st describes, at path, that
// clearFlagsAt mustn't or can't unlock: a file with other names, a hard
// link a job made to a file elsewhere, which its lock would be cleared
// for too, and a socket, which macOS won't open.
func unlockable(st *unix.Stat_t, path string) error {
	if st.Flags&userLocks == 0 {
		return nil
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if st.Nlink > 1 {
			return fmt.Errorf("%s is locked and has %d names, so unlocking it would unlock it elsewhere too: it's left", path, st.Nlink)
		}
	case unix.S_IFSOCK:
		return fmt.Errorf("%s is a locked socket, which macOS won't open to unlock: it's left", path)
	}
	return nil
}

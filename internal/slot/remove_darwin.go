package slot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A remover deletes what a slot's reset, the account-wide clean and the
// account's deletion delete: what's in Silkpool's own folders of the
// slots and the account, the service account's /var/folders folders and
// its sfanalytics folder. A job may have left anything there, so it
// never follows a link: it opens each folder through the one above it,
// with O_NOFOLLOW, and deletes each entry as what it is, a link as a
// link.
//
// The service account may set the user flags uchg and uappnd on what it
// owns, which stop even root deleting it (chflags(2)). The remover clears
// them, but only on an entry the service account owns. The account can't
// set a system flag, so an entry carrying one, or a user flag on an entry
// of another account's, fails closed, naming it: but for sunlnk on a
// folder, with keepNoUnlink, which SIP gives the folders in an account's
// /var/folders folder. Such a folder is emptied, and kept.
//
// An entry in the folder it deletes or empties that the OS refuses it,
// a stat of it, or the opening or reading of a folder, fails it closed,
// naming the entry: but with refused set, the remover leaves the entry,
// adding it there, and it leaves its vault, if one is set.
type remover struct {
	// uid is the service account's, whose entries' user flags the remover
	// clears, or -1 for none.
	uid int
	// keepNoUnlink keeps each folder flagged sunlnk, emptied.
	keepNoUnlink bool
	// ownedOnly deletes only what uid owns, as the sweep does in a shared
	// place: an entry another account owns, even in a folder of uid's,
	// fails closed, naming it, and is left. That uid owns the folder, or
	// that the entry changed since uid's account was made, doesn't show
	// a job made it: another account may have written it there, or a job
	// may have hard-linked another's file in (ADR 0017).
	ownedOnly bool
	// refused, set, leaves each entry the OS refuses the remover (a
	// refusal), adding its path here, with the folders that hold it,
	// as nothing to delete, and goes on. DeleteAccount empties the
	// account's /var/folders folders so: SIP refuses even root a stat of
	// the data vaults macOS keeps there, such as
	// 0/com.apple.LaunchServices.dv (#294). The folder the remover was
	// asked to delete or empty, refused, still fails it.
	refused *[]string
	// vault, set, is a data vault whose contents the clean has reset
	// another way, as it resets the Launch Services database with
	// lsregister (resetLaunchServices). The remover leaves it, if the OS
	// refuses it, its stat, open or read, with the folders that hold it,
	// as handled, and goes on, set or not refused. An entry in it the OS
	// refuses is a refusal like any other.
	vault *resetVault
}

// A resetVault is a remover's vault: its path, and whether it was left.
type resetVault struct {
	path string
	left bool
}

// A refusal is the OS refusing the remover an entry: its stat, or the
// opening or reading of a folder, failed with EPERM or EACCES (refused).
type refusal struct{ err error }

func (r refusal) Error() string { return r.err.Error() }
func (r refusal) Unwrap() error { return r.err }

// refusing returns err as a refusal, if it's one.
func refusing(err error) error {
	if refused(err) {
		return refusal{err}
	}
	return err
}

// userBlockers are the user flags that stop an entry's deletion.
const userBlockers = unix.UF_IMMUTABLE | unix.UF_APPEND

// systemFlags are the flags only root can set: all but the user's.
const systemFlags = ^uint32(unix.UF_SETTABLE)

// removePath deletes the entry at path, whole, or, with keep, empties
// it, which must then be a folder, and keeps it. It follows the links in
// the folders above it, which must be Silkpool's or the OS's, and never
// the entry's own. A missing entry is nothing to delete.
func (r remover) removePath(path string, keep bool) error {
	dir, name := filepath.Split(filepath.Clean(path))
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(fd)
	return r.removeAt(fd, name, path, keep)
}

// removeAt deletes the entry name in the folder dir, as removePath does.
// path names the entry in errors.
func (r remover) removeAt(dir int, name, path string, keep bool) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return refusing(&os.PathError{Op: "stat", Path: path, Err: err})
	}
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	noUnlink, err := r.check(st.Flags, int(st.Uid), isDir, path)
	if err != nil {
		return err
	}
	if st.Flags&userBlockers != 0 {
		if err := clearFlags(dir, name, path, &st); err != nil {
			return err
		}
	}
	if !isDir {
		if keep {
			return fmt.Errorf("%s isn't a folder", path)
		}
		return unlinkAt(dir, name, 0, path)
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return refusing(&os.PathError{Op: "open", Path: path, Err: err})
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if err := same(fd, &st, path); err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		return refusing(err)
	}
	left := r.left()
	for _, n := range names {
		p := filepath.Join(path, n)
		err := r.removeAt(fd, n, p, false)
		if no := (refusal{}); errors.As(err, &no) {
			switch {
			case r.vault != nil && p == r.vault.path && refusedAt(err) == p:
				r.vault.left = true
				continue
			case r.refused != nil:
				*r.refused = append(*r.refused, p)
				continue
			}
		}
		if err != nil {
			return err
		}
	}
	// A folder that holds what was left is kept.
	if keep || noUnlink || r.left() > left {
		return nil
	}
	return unlinkAt(dir, name, unix.AT_REMOVEDIR, path)
}

// refusedAt is the path a refusal names: the entry whose stat, open or
// read the OS refused.
func refusedAt(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Path
	}
	return ""
}

// left counts the entries the remover has left, refused, and its vault.
func (r remover) left() int {
	n := 0
	if r.refused != nil {
		n = len(*r.refused)
	}
	if r.vault != nil && r.vault.left {
		n++
	}
	return n
}

// check checks an entry's flags before the remover deletes it: an entry
// of the account owner, at path, a folder or not. It fails on a system
// flag, and on a user flag that blocks deletion on an entry that isn't
// the service account's. It reports whether the entry is a folder to keep,
// since it carries sunlnk and the remover keeps those.
func (r remover) check(flags uint32, owner int, isDir bool, path string) (noUnlink bool, err error) {
	if r.ownedOnly && owner != r.uid {
		return false, fmt.Errorf("%s is in a folder of the service account's, but it isn't the account's, "+
			"so nothing shows a job made it: it's left", path)
	}
	system := flags & systemFlags
	if r.keepNoUnlink && isDir && system&unix.SF_NOUNLINK != 0 {
		noUnlink, system = true, system&^unix.SF_NOUNLINK
	}
	if system != 0 {
		return false, fmt.Errorf("%s carries the system flags %#x, which the service account can't set, so it's left", path, system)
	}
	if flags&userBlockers != 0 && (r.uid < 0 || owner != r.uid) {
		return false, fmt.Errorf("%s is flagged uchg or uappnd, and isn't the service account's, so it's left", path)
	}
	return noUnlink, nil
}

// clearFlags clears uchg and uappnd from the entry name in dir, which st
// describes, through a descriptor of the entry itself: a link's own,
// through O_SYMLINK, and a FIFO's without waiting for a writer.
func clearFlags(dir int, name, path string, st *unix.Stat_t) error {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_SYMLINK|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	if err := same(fd, st, path); err != nil {
		return err
	}
	if err := unix.Fchflags(fd, int(st.Flags&^userBlockers)); err != nil {
		return &os.PathError{Op: "chflags", Path: path, Err: err}
	}
	return nil
}

// same fails unless fd is the entry st describes, at path: an entry
// replaced since it was read is left.
func same(fd int, st *unix.Stat_t, path string) error {
	var now unix.Stat_t
	if err := unix.Fstat(fd, &now); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if now.Dev != st.Dev || now.Ino != st.Ino {
		return fmt.Errorf("%s changed as it was deleted", path)
	}
	return nil
}

func unlinkAt(dir int, name string, flags int, path string) error {
	if err := unix.Unlinkat(dir, name, flags); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}

// accountFolders returns the service account's folders in dir, /var/folders:
// each a folder, not a link, in one of dir's folders, that uid owns.
// Ensure never gives an account an ID such a folder holds, so one that
// uid owns was made for the account since. It fails unless it reads
// them all, but with skipped set: then a folder macOS refuses to open or
// read (refused), dir or one of its folders, is added to skipped as
// nothing to delete, and the scan goes on.
func accountFolders(dir string, uid int, skipped *[]string) ([]string, error) {
	skip := func(path string, err error) bool {
		if skipped == nil || !refused(err) {
			return false
		}
		*skipped = append(*skipped, path)
		return true
	}
	top, err := openFolder(unix.AT_FDCWD, dir, dir)
	if skip(dir, err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer top.Close()
	buckets, err := top.Readdirnames(-1)
	if skip(dir, err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []string
	for _, b := range buckets {
		path := filepath.Join(dir, b)
		var st unix.Stat_t
		err := unix.Fstatat(int(top.Fd()), b, &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) || err == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "stat", Path: path, Err: err}
		}
		bucket, err := openFolder(int(top.Fd()), b, path)
		if skip(path, err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		names, err := bucket.Readdirnames(-1)
		if err != nil {
			bucket.Close()
			if skip(path, err) {
				continue
			}
			return nil, fmt.Errorf("can't read %s: %w", path, err)
		}
		for _, n := range names {
			// A folder gone since the listing holds nothing to empty.
			if err = unix.Fstatat(int(bucket.Fd()), n, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
				err = nil
			} else if err != nil {
				break
			} else if st.Mode&unix.S_IFMT == unix.S_IFDIR && int(st.Uid) == uid {
				found = append(found, filepath.Join(path, n))
			}
		}
		bucket.Close()
		if err != nil {
			return nil, fmt.Errorf("can't read %s: %w", path, err)
		}
	}
	return found, nil
}

// refused reports whether err is the OS refusing to open or read a
// folder: EPERM or EACCES. For root, as DeleteAccount runs, only macOS's
// own protections, SIP's and TCC's, refuse that, as it refuses even root
// /private/var/protected/sfanalytics on macOS 26 (#229).
func refused(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}

// openFolder opens the folder name in dir, never through a link there.
func openFolder(dir int, name, path string) (*os.File, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

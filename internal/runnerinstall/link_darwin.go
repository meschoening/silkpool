package runnerinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func init() {
	linkFile = clone
	readableSymlink = lchmod
}

// clone clones the install's file src as dst with clonefile(2), which
// APFS answers with a file of its own sharing src's blocks, copy-on-write.
// It keeps src's mode, without setuid or setgid, and its extended
// attributes, and, made by root, its owner. It never follows a symlink,
// and never falls back to a copy, which would cost a runner's root the
// install's whole size.
func clone(src, dst string) error {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("%s isn't on a filesystem that can clone files, such as APFS", filepath.Dir(dst))
	}
	if err != nil {
		return &os.LinkError{Op: "clonefile", Old: src, New: dst, Err: err}
	}
	return nil
}

// lchmod makes the symlink at path 0755. macOS gives a new symlink its
// mode from the umask, and a slot can't readlink(2) one it can't read.
func lchmod(path string) error {
	if err := unix.Fchmodat(unix.AT_FDCWD, path, 0o755, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "lchmod", Path: path, Err: err}
	}
	return nil
}

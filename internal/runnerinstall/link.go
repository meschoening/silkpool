//go:build unix

package runnerinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/meschoening/silkpool/internal/runnerapp"
)

// Link builds a runner's root at root, such as
// /var/lib/silkpool/slots/1/runner, from links to the install of
// version, for the machine's service account, which every slot runs as,
// with uid and gid.
//
// The root's files are linked to the install's, so they stay owned by
// root and read-only to the account: a job can't change the install through
// them. On Linux they're hardlinks to the install's files. On macOS
// they're APFS clones, files of their own that share the install's
// blocks and keep its files' modes and, made by root, their owner; even
// a change to one never reaches the install. Its folders and symlinks
// are made afresh, owned by root, and none of them leads into the
// install, which a job reaches only through its links. The root folder
// itself belongs to the service account, 0700, so the runner can write its
// own files in it: .runner, its credentials, _diag and _work.
//
// root must not exist yet, because each runner gets a fresh one, and its
// parent must, on the same filesystem as Dir: an APFS volume on macOS. A
// failed Link leaves no root behind.
func (i Installs) Link(version, root string, uid, gid int) error {
	if !runnerapp.Valid(version) {
		return fmt.Errorf("%q isn't a version of the runner application", version)
	}
	install := i.path(version)
	if _, err := os.Stat(install); err != nil {
		return fmt.Errorf("the runner application %s isn't installed: %w", version, err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	if err := link(install, root); err != nil {
		os.RemoveAll(root)
		return err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		os.RemoveAll(root)
		return err
	}
	// Last, so the account can't touch the root until it's complete.
	if err := os.Chown(root, uid, gid); err != nil {
		os.RemoveAll(root)
		return err
	}
	return nil
}

// link links what the folder install holds into the folder root.
func link(install, root string) error {
	return filepath.WalkDir(install, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(install, path)
		if err != nil || rel == "." {
			return err
		}
		dst := filepath.Join(root, rel)
		switch t := d.Type(); {
		case t.IsDir():
			if err := os.Mkdir(dst, 0o755); err != nil {
				return err
			}
			return os.Chmod(dst, 0o755)
		case t.IsRegular():
			err := linkFile(path, dst)
			if errors.Is(err, syscall.EXDEV) {
				return fmt.Errorf("%s isn't on the same filesystem as the installs in %s", root, filepath.Dir(install))
			}
			return err
		case t&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
			return readableSymlink(dst)
		}
		return fmt.Errorf("the install holds %s, which isn't a file, a folder or a link", rel)
	})
}

// linkFile links the install's file src into a runner's root as dst,
// which isn't there yet. It's a hardlink, but macOS's file makes it an
// APFS clone.
var linkFile = os.Link

// readableSymlink makes the new symlink at path readable by everyone,
// whatever the agent's umask, since a job reads it: node does, resolving
// npm. Linux's symlinks always are; macOS's file sets the mode.
var readableSymlink = func(string) error { return nil }

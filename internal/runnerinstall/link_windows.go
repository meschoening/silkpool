package runnerinstall

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/meschoening/silkpool/internal/runnerapp"
)

// Link builds a runner's root at root, such as
// C:\ProgramData\Silkpool\slots\1\runner, from links to the install of
// version, for the machine's service account, Account.
//
// The root's files are NTFS hardlinks to the install's: other names of
// the same files, so they keep the install's descriptor. They stay the
// agent's, which the account can only read and run, so a job can't
// change the install through them. A file NTFS won't link again, such as
// one that already has the 1024 names it allows, or any link NTFS
// refuses, is copied instead, with the same descriptor. The root's
// folders are made afresh, the agent's, which the account can only list
// and pass through. The root folder itself is the account's, so the
// runner can write its own files in it: .runner, its credentials, _diag
// and _work, which are the account's too.
//
// root must not exist yet, because each runner gets a fresh one, and its
// parent must, on the same volume as Dir, or every file is copied. A
// failed Link leaves no root behind.
func (i Installs) Link(version, root string) error {
	if !runnerapp.Valid(version) {
		return fmt.Errorf("%q isn't a version of the runner application", version)
	}
	s, err := i.sids()
	if err != nil {
		return err
	}
	install := i.path(version)
	if _, err := os.Stat(install); err != nil {
		return fmt.Errorf("the runner application %s isn't installed: %w", version, err)
	}
	// The agent's alone until it's complete: the account runs other
	// slots' runners, which mustn't reach into it before.
	private, err := s.private()
	if err != nil {
		return err
	}
	if err := mkdir(root, private); err != nil {
		return err
	}
	if err := s.link(install, root); err != nil {
		os.RemoveAll(root)
		return err
	}
	whole, err := s.root()
	if err == nil {
		err = apply(root, whole)
	}
	if err != nil {
		os.RemoveAll(root)
		return err
	}
	return nil
}

// link links what the folder install holds into the folder root.
func (s sids) link(install, root string) error {
	readable, err := s.readable()
	if err != nil {
		return err
	}
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
			return mkdir(dst, readable)
		case t.IsRegular():
			if os.Link(path, dst) == nil {
				return nil
			}
			return copyFile(path, dst, readable)
		}
		return fmt.Errorf("the install holds %s, which isn't a file or a folder", rel)
	})
}

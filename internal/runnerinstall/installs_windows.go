package runnerinstall

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/runnerapp"
)

// extractor returns what extracts the node's platform's package: on
// Windows, the zip of win-x64 or win-arm64.
func (i Installs) extractor() (func(pkg, dir string) error, error) {
	switch i.Platform {
	case runnerapp.WinX64, runnerapp.WinARM64:
		return unzip, nil
	}
	return nil, fmt.Errorf("installing the runner application for %s isn't supported on %s", i.Platform, runtime.GOOS)
}

// prepare makes Dir, and makes it the agent's alone, SYSTEM's as the
// agent runs, whoever made it. It refuses at once an Account that isn't
// a SID, which every install's files name.
//
// Only a folder that isn't already the agent's alone gets its
// descriptor, since giving one passes it on to every install under it,
// which a Delete may be deleting.
func (i Installs) prepare() error {
	s, err := i.sids()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(i.Dir, 0o700); err != nil {
		return err
	}
	if s.isPrivate(i.Dir) {
		return nil
	}
	sd, err := s.private()
	if err != nil {
		return err
	}
	return apply(i.Dir, sd)
}

// finish gives each file of the extracted install tmp its descriptor, so
// the service account can only read and run it, and renames tmp to
// install. Its folders keep what they inherit from Dir: the agent's
// alone. Each file reached the disk as it was extracted, and the rename
// does before finish returns, so that even a power cut can't leave an
// install that looks usable but isn't.
func (i Installs) finish(tmp, install string) error {
	s, err := i.sids()
	if err != nil {
		return err
	}
	sd, err := s.readable()
	if err != nil {
		return err
	}
	err = filepath.WalkDir(tmp, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		return apply(path, sd)
	})
	if err != nil {
		return err
	}
	beforeRename(tmp)
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(install)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "rename", Old: tmp, New: install, Err: err}
	}
	return nil
}

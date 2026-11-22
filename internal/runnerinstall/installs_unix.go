//go:build unix

package runnerinstall

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/meschoening/silkpool/internal/runnerapp"
)

// extractor returns what extracts the node's platform's package: on
// Linux and macOS, a tar.gz of linux-x64, linux-arm64 or osx-arm64.
func (i Installs) extractor() (func(pkg, dir string) error, error) {
	switch i.Platform {
	case runnerapp.LinuxX64, runnerapp.LinuxARM64, runnerapp.OSXARM64:
		return extract, nil
	}
	return nil, fmt.Errorf("installing the runner application for %s isn't supported on %s", i.Platform, runtime.GOOS)
}

// prepare makes Dir, and makes it its owner's alone, root's as the agent
// runs.
func (i Installs) prepare() error {
	if err := os.MkdirAll(i.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(i.Dir, 0o700)
}

// finish renames the extracted install tmp to install. Everything in it
// reaches the disk first, so that even a power cut can't leave an install
// that looks usable but isn't.
func (i Installs) finish(tmp, install string) error {
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	err := filepath.WalkDir(tmp, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		return syncPath(path)
	})
	if err != nil {
		return err
	}
	beforeRename(tmp)
	if err := os.Rename(tmp, install); err != nil {
		return err
	}
	return syncPath(filepath.Dir(install))
}

// syncPath flushes the file or folder at path to the disk.
func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

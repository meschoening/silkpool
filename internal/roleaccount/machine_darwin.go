package roleaccount

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Mac is the Mac this runs on: its own directory, its processes as ps
// lists them, and the folders a deleted account leaves behind.
var Mac = Machine{Directory: Local, Processes: processes, Leftovers: leftovers}

// processes lists every running process, with its effective, real and
// saved UIDs and GIDs, as ps prints them. Each column has its own -o, so
// none's empty header swallows the next.
func processes(ctx context.Context) ([]Process, error) {
	cols := []string{"uid", "ruid", "svuid", "gid", "rgid", "svgid", "pid"}
	args := []string{"-A"}
	for _, c := range cols {
		args = append(args, "-o", c+"=")
	}
	out, err := exec.CommandContext(ctx, "/bin/ps", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("ps -A: %w", err)
	}
	return parseProcesses(string(out), len(cols))
}

// leftovers lists the folders in /var/folders' buckets and in /Users,
// each with its owner and group. A folder gone since its listing is left
// out, since nothing of an account is kept there.
func leftovers() ([]Owned, error) {
	buckets, err := children("/var/folders")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, b := range buckets {
		in, err := children(b)
		if err != nil {
			return nil, err
		}
		paths = append(paths, in...)
	}
	users, err := children("/Users")
	if err != nil {
		return nil, err
	}
	var owned []Owned
	for _, p := range append(paths, users...) {
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		st := info.Sys().(*syscall.Stat_t)
		owned = append(owned, Owned{Path: p, UID: int(st.Uid), GID: int(st.Gid)})
	}
	return owned, nil
}

// children returns the paths of what's in dir, failing if it can't be
// read whole.
func children(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = filepath.Join(dir, e.Name())
	}
	return paths, nil
}

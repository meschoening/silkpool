package slot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/godbus/dbus/v5"
)

// WorkerProgram is the runner application's worker in a runner root:
// the program the listener starts to run its job, just after taking it
// (GLOSSARY.md's Worker).
const WorkerProgram = "bin/Runner.Worker"

// cgroupRoot is where systemd mounts the unified cgroup hierarchy.
const cgroupRoot = "/sys/fs/cgroup"

// WorkerUp reports whether runner's worker is up in slot n: whether a
// process of the runner's unit runs slot n's runner root's
// WorkerProgram, from "Decide what Emergency stop does in a job's first
// second".
// It looks at the processes in the unit's cgroup, so it never counts
// another runner's, and no job code runs before the worker, so a job
// can't fake one. A unit that's gone, or has no processes, has none.
// Reading which program a slot account's process runs takes
// CAP_SYS_PTRACE, which root has. It may run beside Wait, Stop and Kill
// on the same slot.
func (s *Slots) WorkerUp(ctx context.Context, n int, runner string) (bool, error) {
	if err := checkRunner(runner); err != nil {
		return false, err
	}
	var path dbus.ObjectPath
	err := s.manager(ctx, "GetUnit", s.unit(n, runner)).Store(&path)
	if gone(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cgroup string
	err = storeProperty(ctx, s.bus.Object(systemd, path), "ControlGroup", &cgroup)
	if gone(err) || err == nil && cgroup == "" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return runsIn(filepath.Join(cgroupRoot, cgroup), filepath.Join(s.RunnerRoot(n), WorkerProgram))
}

// runsIn reports whether a process in the cgroup whose folder is dir, or
// in one below it, runs program: whether /proc names program as the
// program it runs. A process or a cgroup that's gone meanwhile has none,
// and a process whose program can't be read is an error.
func runsIn(dir, program string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case !d.IsDir():
			return nil
		}
		procs, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, pid := range strings.Fields(string(procs)) {
			exe, err := os.Readlink("/proc/" + pid + "/exe")
			switch {
			case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ESRCH):
				// It has ended.
			case err != nil:
				return fmt.Errorf("can't read which program process %s runs: %w", pid, err)
			case exe == program:
				found = true
				return fs.SkipAll
			}
		}
		return nil
	})
	return found, err
}

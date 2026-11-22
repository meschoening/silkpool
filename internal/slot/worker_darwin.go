package slot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// errNoProcess is a process whose program can't be read now, which counts
// as running no worker: it ended before its program was read, or the
// kernel can't copy its arguments, as while it execs or exits. Each later
// look tries again.
var errNoProcess = errors.New("the process is gone, or its program can't be read now")

// procArgs reads kern.procargs2 for a process, or is replaced by a test.
var procArgs = func(pid int) ([]byte, error) { return unix.SysctlRaw("kern.procargs2", pid) }

// WorkerUp reports whether runner's worker is up in slot n: whether a
// process of the slot's launchd job runs slot n's runner root's
// WorkerProgram, from "Decide what Emergency stop does in a job's first
// second" (#123). Every slot runs as the one service account (#214), so
// it looks in the slot's job, not among the account's processes: at the
// processes descended from the job's own, the listener, which launchd
// names. Another slot's processes never descend from it, so it never
// counts another runner's worker. A process's program is the path its
// exec was given, as the kernel keeps it (programOf), and no job code
// runs before the worker, so a job can't fake one. A job that's gone, or
// not running, has none. Reading another account's process's program
// takes root, which the agent has. It may run beside Wait, Stop and Kill
// on the same slot.
func (s *Slots) WorkerUp(ctx context.Context, n int, runner string) (bool, error) {
	if err := checkRunner(runner); err != nil {
		return false, err
	}
	st, err := s.status(ctx, n, runner)
	if errors.Is(err, ErrGone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.Running() {
		return false, nil
	}
	children, err := s.children()
	if err != nil {
		return false, fmt.Errorf("can't list the Mac's processes: %w", err)
	}
	return s.runsUnder(children, st.PID, s.workerPrograms(n))
}

// workerPrograms are the paths of slot n's worker: its runner root's
// WorkerProgram, and the same with the root's links resolved, as the
// runner names its own folder (/private/var/db/… for /var/db/…).
func (s *Slots) workerPrograms(n int) []string {
	root := s.RunnerRoot(n)
	programs := []string{filepath.Join(root, WorkerProgram)}
	if r, err := filepath.EvalSymlinks(root); err == nil && r != root {
		programs = append(programs, filepath.Join(r, WorkerProgram))
	}
	return programs
}

// runsUnder reports whether a process descended from pid, which children
// lists by parent, runs one of programs. A process that ends before its
// program is read runs none, though what it started, as the listing has
// it, is looked at still; a program that can't be read otherwise is an
// error.
func (s *Slots) runsUnder(children map[int][]int, pid int, programs []string) (bool, error) {
	seen := map[int]bool{pid: true}
	next := slices.Clone(children[pid])
	for len(next) > 0 {
		p := next[0]
		next = next[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		program, err := s.program(p)
		switch {
		case errors.Is(err, errNoProcess):
		case err != nil:
			return false, fmt.Errorf("can't read which program process %d runs: %w", p, err)
		case slices.Contains(programs, program):
			return true, nil
		}
		next = append(next, children[p]...)
	}
	return false, nil
}

// listChildren lists every live process's PID by its parent's, as the
// kernel lists them (kern.proc.all).
func listChildren() (map[int][]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	children := map[int][]int{}
	for _, p := range procs {
		children[int(p.Eproc.Ppid)] = append(children[int(p.Eproc.Ppid)], int(p.Proc.P_pid))
	}
	return children, nil
}

// programOf returns the program process pid runs: the path its exec was
// given, which the kernel keeps at the start of its arguments
// (kern.procargs2: the argument count, then that path). It's
// errNoProcess for a process that has ended, or is ending. The kernel
// answers EINVAL both for that and for a process it won't show the
// caller, such as another account's to anyone but root, so a process it
// still lists, not a zombie, is refused, not gone. It answers EIO when
// it can't copy a process's arguments, as during its exec or its exit,
// when the macos lane met it as the listener started a job's worker:
// errNoProcess too, so the next look tries again. Nothing promises a
// live process's arguments can be read again, but one that never could
// would only withhold WorkerStarted: Emergency stop interrupts the
// listener once node.WorkerWait has passed all the same.
func programOf(pid int) (string, error) {
	b, err := procArgs(pid)
	if errors.Is(err, unix.EIO) {
		return "", errNoProcess
	}
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ESRCH) {
		live, lerr := listed(pid)
		switch {
		case lerr != nil:
			return "", fmt.Errorf("%w, and whether it still runs can't be told: %w", err, lerr)
		case live:
			return "", fmt.Errorf("the kernel won't show it: %w", err)
		}
		return "", errNoProcess
	}
	if err != nil {
		return "", err
	}
	return parseProcArgs(b)
}

// zombie is the state of a process that has ended but not been reaped
// (SZOMB in sys/proc.h).
const zombie = 5

// listed reports whether the kernel lists process pid, and it isn't a
// zombie (kern.proc.pid, which gives no record for a process that's
// gone).
func listed(pid int) (bool, error) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	switch {
	case errors.Is(err, unix.EIO):
		return false, nil
	case err != nil:
		return false, err
	}
	return int(p.Proc.P_pid) == pid && p.Proc.P_stat != zombie, nil
}

// parseProcArgs reads the program's path from what kern.procargs2 gives:
// a 4-byte argument count, then the path, ended by a NUL.
func parseProcArgs(b []byte) (string, error) {
	if len(b) < 4 {
		return "", fmt.Errorf("kern.procargs2 gave %d bytes, too few for its argument count", len(b))
	}
	path, _, ok := bytes.Cut(b[4:], []byte{0})
	if !ok || len(path) == 0 {
		return "", errors.New("kern.procargs2 gave no program path")
	}
	return string(path), nil
}

package winslot

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobEndLimit is how long the host waits for the listener's job to
// empty once it has ended everything in it.
const jobEndLimit = 30 * time.Second

// newSlotJob makes the slot's job object, name, open to owner alone,
// that kills every process in it once its last handle closes, as when
// the host's process ends. It allows no breakaway, so a process in it
// can't start one outside it. It refuses a job of that name that's
// there already, which another host, or someone else, holds.
func newSlotJob(name string, owner *windows.SID) (windows.Handle, error) {
	job, existed, err := createJob(name, owner)
	if err != nil {
		return 0, err
	}
	if existed {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("a job object named %s is there already", name)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// startInJob starts cmd inside the slot's job, nested in the node job,
// so that cmd and every process it starts are in both from their first
// instruction: cmd starts suspended, goes into node, then into slot,
// which nests slot in node, and only then runs. If anything fails, cmd
// is ended and waited for before startInJob returns, and so is whatever
// it started in slot; nothing else in node is touched.
func startInJob(cmd *exec.Cmd, node, slot windows.Handle) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := uint32(cmd.Process.Pid)
	fail := func(err error, ran bool) error {
		if ran {
			// It may have run, but only in slot.
			windows.TerminateJobObject(slot, Terminated)
		}
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	if err := assign(node, pid); err != nil {
		return fail(fmt.Errorf("putting it in the node job: %w", err), false)
	}
	if err := assign(slot, pid); err != nil {
		return fail(fmt.Errorf("putting it in its slot's job, inside the node job: %w", err), false)
	}
	if err := resume(pid); err != nil {
		return fail(fmt.Errorf("starting it in its job object: %w", err), true)
	}
	return nil
}

// assign puts the process pid in job.
func assign(job windows.Handle, pid uint32) error {
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(p)
	return windows.AssignProcessToJobObject(job, p)
}

// resume lets the suspended process pid run. A process made suspended
// has one thread, its first, until that runs, so resume fails unless it
// finds exactly one, suspended once.
func resume(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	var threads []uint32
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID == pid {
			threads = append(threads, entry.ThreadID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("listing its threads: %w", err)
	}
	if len(threads) != 1 {
		return fmt.Errorf("it has %d threads, want its first alone", len(threads))
	}
	t, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, threads[0])
	if err != nil {
		return err
	}
	defer windows.CloseHandle(t)
	was, err := windows.ResumeThread(t)
	if err != nil {
		return err
	}
	if was != 1 {
		return fmt.Errorf("its first thread was suspended %d times, want once", was)
	}
	return nil
}

// basicAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type basicAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

// endJob ends every process in job and waits, up to limit, until none
// is left, which confirms they've ended.
func endJob(job windows.Handle, limit time.Duration) error {
	if err := windows.TerminateJobObject(job, Terminated); err != nil {
		return fmt.Errorf("ending the listener's job: %w", err)
	}
	deadline := time.Now().Add(limit)
	for {
		var info basicAccounting
		if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
			return fmt.Errorf("counting the listener's job's processes: %w", err)
		}
		if info.ActiveProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of the listener's job's processes still run %s after ending it", info.ActiveProcesses, limit)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Programs returns the full paths of the programs the processes in the
// slot's job name run, as the agent's worker check reads them, from "Decide
// what Emergency stop does in a job's first second". The job must be as a
// host makes it for the account the caller runs as, SYSTEM's alone on a
// node, so nothing else can stand in for it, and a job nests no other, so
// its processes are its slot's alone. A job that isn't there, as once its
// host has stopped, runs none, and so does a process that ends before its
// program is read.
func Programs(name string) ([]string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	job, ok, err := openJob(name, jobQuery, u.User.Sid)
	if err != nil || !ok {
		return nil, err
	}
	defer windows.CloseHandle(job)
	pids, err := jobPids(job)
	if err != nil {
		return nil, fmt.Errorf("listing the processes in the job object %s: %w", name, err)
	}
	var programs []string
	for _, pid := range pids {
		p, err := programOf(pid)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			// It has ended.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading which program process %d runs: %w", pid, err)
		}
		programs = append(programs, p)
	}
	return programs, nil
}

// jobPids returns the IDs of the processes in job, from
// JOBOBJECT_BASIC_PROCESS_ID_LIST, growing its list until they fit.
func jobPids(job windows.Handle) ([]uint32, error) {
	// The list's two ULONG counts, NumberOfAssignedProcesses and
	// NumberOfProcessIdsInList, take the first 8 bytes, then come the IDs,
	// each a ULONG_PTR.
	head := int(8 / unsafe.Sizeof(uintptr(0)))
	for size := 64; ; size *= 2 {
		buf := make([]uintptr, head+size)
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(buf[0])), nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, err
		}
		counts := (*[2]uint32)(unsafe.Pointer(&buf[0]))
		if assigned, listed := counts[0], counts[1]; err == nil && listed >= assigned {
			pids := make([]uint32, listed)
			for i, id := range buf[head : head+int(listed)] {
				pids[i] = uint32(id)
			}
			return pids, nil
		}
	}
}

// programOf returns the full path of the program the process pid runs.
func programOf(pid uint32) (string, error) {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

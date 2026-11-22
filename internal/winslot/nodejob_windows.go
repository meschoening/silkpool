package winslot

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/winacl"
)

// The job objects' calls and values x/sys doesn't have.
var (
	procCreateJobObjectW = kernel32.NewProc("CreateJobObjectW")
	procOpenJobObjectW   = kernel32.NewProc("OpenJobObjectW")
	procIsProcessInJob   = kernel32.NewProc("IsProcessInJob")

	procAdjustTokenPrivileges = windows.NewLazySystemDLL("advapi32.dll").NewProc("AdjustTokenPrivileges")
)

const (
	jobAssignProcess = 0x0001 // JOB_OBJECT_ASSIGN_PROCESS
	jobQuery         = 0x0004 // JOB_OBJECT_QUERY
	// jobAllAccess is JOB_OBJECT_ALL_ACCESS.
	jobAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x3f

	cpuRateEnable  = 0x1 // JOB_OBJECT_CPU_RATE_CONTROL_ENABLE
	cpuRateHardCap = 0x4 // JOB_OBJECT_CPU_RATE_CONTROL_HARD_CAP

	allProcessorGroups = 0xffff // ALL_PROCESSOR_GROUPS

	// The messages a job sends its completion port that the watch reads.
	msgActiveProcessZero   = 4  // JOB_OBJECT_MSG_ACTIVE_PROCESS_ZERO
	msgNewProcess          = 6  // JOB_OBJECT_MSG_NEW_PROCESS
	msgExitProcess         = 7  // JOB_OBJECT_MSG_EXIT_PROCESS
	msgAbnormalExit        = 8  // JOB_OBJECT_MSG_ABNORMAL_EXIT_PROCESS
	msgJobMemoryLimit      = 10 // JOB_OBJECT_MSG_JOB_MEMORY_LIMIT
	seIncreaseBasePriority = "SeIncreaseBasePriorityPrivilege"
)

// cpuRateControl is JOBOBJECT_CPU_RATE_CONTROL_INFORMATION, with its
// union as CpuRate.
type cpuRateControl struct {
	ControlFlags uint32
	CpuRate      uint32
}

// associateCompletionPort is JOBOBJECT_ASSOCIATE_COMPLETION_PORT.
type associateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

// jobSD returns a job object's descriptor: owned by owner, which alone
// may open it, with every right.
func jobSD(owner *windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;0x%x;;;%s)", owner, jobAllAccess, owner))
}

// checkJob accepts the job object h only if it's as jobSD makes it for
// owner: so no one else made it, or can open it.
func checkJob(h windows.Handle, owner *windows.SID) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return winacl.Check(sd, owner, []winacl.Entry{{SID: owner, Mask: jobAllAccess}})
}

// createJob makes the job object name, or opens it if it's there, with
// jobSD's descriptor for owner, and reports whether it was there.
func createJob(name string, owner *windows.SID) (h windows.Handle, existed bool, err error) {
	sd, err := jobSD(owner)
	if err != nil {
		return 0, false, err
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, false, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	r, _, e := procCreateJobObjectW.Call(uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(p)))
	if r == 0 {
		return 0, false, e
	}
	return windows.Handle(r), errors.Is(e, windows.ERROR_ALREADY_EXISTS), nil
}

// openJob opens the job object name with access, and checks it's
// owner's alone. It reports false if there's no job of that name.
func openJob(name string, access uint32, owner *windows.SID) (windows.Handle, bool, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, false, err
	}
	r, _, e := procOpenJobObjectW.Call(uintptr(access|windows.READ_CONTROL), 0, uintptr(unsafe.Pointer(p)))
	if r == 0 {
		if errors.Is(e, windows.ERROR_FILE_NOT_FOUND) {
			return 0, false, nil
		}
		return 0, false, e
	}
	h := windows.Handle(r)
	if err := checkJob(h, owner); err != nil {
		windows.CloseHandle(h)
		return 0, false, fmt.Errorf("the job object %s isn't Silkpool's, so it won't use it: %w", name, err)
	}
	return h, true, nil
}

// inJob reports whether the process p is in the job object job, or a
// job nested in it.
func inJob(p, job windows.Handle) (bool, error) {
	var in int32
	if r, _, e := procIsProcessInJob.Call(uintptr(p), uintptr(job), uintptr(unsafe.Pointer(&in))); r == 0 {
		return false, e
	}
	return in != 0, nil
}

// A NodeJob is the node job: a named job object, open to SYSTEM alone,
// that the agent keeps and every slot host nests its own job in, so
// that its limits bound every slot together (ADR 0014). It outlives an
// agent restart while any slot host holds it, and keeps its limits.
type NodeJob struct {
	name   string
	handle windows.Handle
	// owner is the only account that may open the job, and the slots'
	// jobs: SYSTEM, or in tests their own account.
	owner *windows.SID
	// priority is whether the job holds its processes below normal
	// priority, which takes SeIncreaseBasePriorityPrivilege. Only tests
	// that run without it go without.
	priority bool
}

// OpenNodeJob makes the node job name, or opens it if it's there, as
// after an agent restart while slots run. It refuses a job of that name
// that isn't SYSTEM's alone. A job it makes holds its processes below
// normal priority from the start; one that's there keeps its limits.
func OpenNodeJob(name string) (*NodeJob, error) {
	return openNodeJob(name, winacl.WellKnown(windows.WinLocalSystemSid), true)
}

func openNodeJob(name string, owner *windows.SID, priority bool) (*NodeJob, error) {
	h, existed, err := createJob(name, owner)
	if err != nil {
		return nil, fmt.Errorf("making the node job %s: %w", name, err)
	}
	if err := checkJob(h, owner); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("the node job %s isn't Silkpool's, so it won't use it: %w", name, err)
	}
	j := &NodeJob{name: name, handle: h, owner: owner, priority: priority}
	if !existed {
		if err := j.SetBudget(availability.Budget{}); err != nil {
			j.Close()
			return nil, err
		}
	}
	return j, nil
}

// Close closes the agent's handle to the node job. The job goes once
// no slot host holds it either.
func (j *NodeJob) Close() error { return windows.CloseHandle(j.handle) }

// cpus is the machine's logical CPU count across every processor group,
// which a job's CPU rate is a share of.
func cpus() int { return int(windows.GetActiveProcessorCount(allProcessorGroups)) }

// SetBudget holds the slots to the budget b: a job memory limit and a
// CPU rate hard cap (LimitsFor), each set, or cleared where b has none,
// and below normal priority whatever b is. A memory limit below what the
// slots have committed fails their next commits rather than ending
// anything, but the node lowers it only once no slot holds a job.
func (j *NodeJob) SetBudget(b availability.Budget) error {
	l, err := LimitsFor(b, cpus())
	if err != nil {
		return fmt.Errorf("can't hold the slots to the budget: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if l.Memory > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(l.Memory)
	}
	if j.priority {
		if err := enablePrivilege(seIncreaseBasePriority); err != nil {
			return fmt.Errorf("can't hold the slots below normal priority: %w", err)
		}
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PRIORITY_CLASS
		info.BasicLimitInformation.PriorityClass = windows.BELOW_NORMAL_PRIORITY_CLASS
	}
	if _, err := windows.SetInformationJobObject(j.handle, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("can't set the node job's memory limit and priority: %w", err)
	}
	var rate cpuRateControl
	if l.CPURate > 0 {
		rate = cpuRateControl{ControlFlags: cpuRateEnable | cpuRateHardCap, CpuRate: l.CPURate}
	} else {
		// Windows clears a cap that's on, but refuses to clear one on a
		// job that never had one ("The parameter is incorrect", on the VM).
		var now cpuRateControl
		if err := windows.QueryInformationJobObject(j.handle, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&now)), uint32(unsafe.Sizeof(now)), nil); err != nil {
			return fmt.Errorf("reading the node job's CPU rate: %w", err)
		}
		if now.ControlFlags&cpuRateEnable == 0 {
			return nil
		}
	}
	if _, err := windows.SetInformationJobObject(j.handle, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate))); err != nil {
		return fmt.Errorf("can't set the node job's CPU rate: %w", err)
	}
	return nil
}

// Limits reads the limits the node job holds the slots to.
func (j *NodeJob) Limits() (Limits, error) {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(j.handle, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return Limits{}, fmt.Errorf("reading the node job's limits: %w", err)
	}
	var rate cpuRateControl
	if err := windows.QueryInformationJobObject(j.handle, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate)), nil); err != nil {
		return Limits{}, fmt.Errorf("reading the node job's CPU rate: %w", err)
	}
	var l Limits
	flags := info.BasicLimitInformation.LimitFlags
	if flags&windows.JOB_OBJECT_LIMIT_JOB_MEMORY != 0 {
		l.Memory = uint64(info.JobMemoryLimit)
	}
	l.BelowNormal = flags&windows.JOB_OBJECT_LIMIT_PRIORITY_CLASS != 0 && info.BasicLimitInformation.PriorityClass == windows.BELOW_NORMAL_PRIORITY_CLASS
	if rate.ControlFlags&cpuRateEnable != 0 && rate.ControlFlags&cpuRateHardCap != 0 {
		l.CPURate = rate.CpuRate
	}
	return l, nil
}

// enablePrivilege enables the privilege name in the process's token,
// failing if the token doesn't hold it.
func enablePrivilege(name string) error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
		return err
	}
	privs := windows.Tokenprivileges{PrivilegeCount: 1, Privileges: [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}}}
	// AdjustTokenPrivileges succeeds without enabling a privilege the
	// token doesn't hold, saying so only in the last error, which Call
	// reads at once.
	r, _, e := procAdjustTokenPrivileges.Call(uintptr(token), 0, uintptr(unsafe.Pointer(&privs)), 0, 0, 0)
	switch {
	case r == 0:
		return e
	case errors.Is(e, windows.ERROR_NOT_ALL_ASSIGNED):
		return fmt.Errorf("the process doesn't hold %s", name)
	}
	return nil
}

// A Watch hears the node job's messages, and tells which slot's job held
// each process that tried to commit memory past the node's memory
// budget, which fails the commit while the process, its job and its
// slot carry on. Windows says which process with the message, and the
// watch looks for it in the slots' jobs by name, while they're there.
//
// A job's messages aren't sure to arrive, so a slot the watch names did
// run out, but one it doesn't name may have too.
type Watch struct {
	job   *NodeJob
	port  windows.Handle
	slots func() []string
	done  chan struct{}

	mu sync.Mutex
	// held are the processes in the node job, by ID, each held open
	// from its first message to its exit's, so its ID can't name another
	// process meanwhile.
	held map[uint32]windows.Handle
	// out are the slot jobs that held a process that ran out.
	out map[string]bool
}

// Watch starts watching the node job's messages, looking in the slot
// jobs slots names for a process that ran out of the memory budget.
// slots is called from the watch's own goroutine, as each message comes. A
// job takes one completion port, so a watch of a restarted agent takes
// the job from its last one's. Windows then names every process already
// in the job, so a slot running across the restart is watched as before.
func (j *NodeJob) Watch(slots func() []string) (*Watch, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("making the node job's completion port: %w", err)
	}
	set := func(port windows.Handle) error {
		a := associateCompletionPort{CompletionPort: port}
		_, err := windows.SetInformationJobObject(j.handle, windows.JobObjectAssociateCompletionPortInformation, uintptr(unsafe.Pointer(&a)), uint32(unsafe.Sizeof(a)))
		return err
	}
	if err := set(port); err != nil {
		// The job keeps the last watch's port, though its agent has gone,
		// until it's taken away.
		if err := set(0); err != nil {
			windows.CloseHandle(port)
			return nil, fmt.Errorf("taking the node job's messages from its last watch: %w", err)
		}
		if err := set(port); err != nil {
			windows.CloseHandle(port)
			return nil, fmt.Errorf("watching the node job's messages: %w", err)
		}
	}
	w := &Watch{job: j, port: port, slots: slots, done: make(chan struct{}), held: map[uint32]windows.Handle{}, out: map[string]bool{}}
	go w.run()
	return w, nil
}

// Close stops the watch. Its port stays the job's until the next watch
// takes the job from it.
func (w *Watch) Close() error {
	err := windows.CloseHandle(w.port)
	<-w.done
	return err
}

// OutOfMemory reports whether a process in the slot job named slot has
// tried to commit memory past the node's budget since it was last
// asked, and forgets it: so the agent asks once before it starts a
// slot's runner, and once its service has stopped.
func (w *Watch) OutOfMemory(slot string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.out[slot]
	delete(w.out, slot)
	return out
}

func (w *Watch) run() {
	defer close(w.done)
	defer func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		for pid, p := range w.held {
			windows.CloseHandle(p)
			delete(w.held, pid)
		}
	}()
	for {
		var msg uint32
		var key uintptr
		var ov *windows.Overlapped
		if err := windows.GetQueuedCompletionStatus(w.port, &msg, &key, &ov, windows.INFINITE); err != nil {
			// The port is closed.
			return
		}
		// A job's message carries a process ID where an overlapped
		// structure would be.
		pid := uint32(uintptr(unsafe.Pointer(ov)))
		switch msg {
		case msgNewProcess:
			w.hold(pid)
		case msgExitProcess, msgAbnormalExit:
			w.let(pid)
		case msgActiveProcessZero:
			// No process is left, so neither should a held one be, as an
			// exit's message isn't sure to arrive.
			w.mu.Lock()
			for pid, p := range w.held {
				windows.CloseHandle(p)
				delete(w.held, pid)
			}
			w.mu.Unlock()
		case msgJobMemoryLimit:
			w.ranOut(pid)
		}
	}
}

// hold holds the process pid open, if it can.
func (w *Watch) hold(pid uint32) {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if old, ok := w.held[pid]; ok {
		windows.CloseHandle(old)
	}
	w.held[pid] = p
}

// let lets the process pid go.
func (w *Watch) let(pid uint32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p, ok := w.held[pid]; ok {
		windows.CloseHandle(p)
		delete(w.held, pid)
	}
}

// ranOut marks the slot whose job holds the process pid, which tried to
// commit past the budget.
func (w *Watch) ranOut(pid uint32) {
	w.mu.Lock()
	p, held := w.held[pid]
	w.mu.Unlock()
	if !held {
		// Its first message may not have arrived: it's held now, if it's
		// still there.
		w.hold(pid)
		w.mu.Lock()
		p, held = w.held[pid]
		w.mu.Unlock()
		if !held {
			return
		}
	}
	// The process is the node job's, so its ID is the one it ran out
	// under.
	if in, err := inJob(p, w.job.handle); err != nil || !in {
		return
	}
	for _, slot := range w.slots() {
		h, ok, err := openJob(slot, jobQuery, w.job.owner)
		if err != nil || !ok {
			continue
		}
		in, err := inJob(p, h)
		windows.CloseHandle(h)
		if err == nil && in {
			w.mu.Lock()
			w.out[slot] = true
			w.mu.Unlock()
			return
		}
	}
}

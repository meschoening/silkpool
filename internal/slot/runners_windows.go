package slot

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winexposure"
	"github.com/meschoening/silkpool/internal/winslot"
)

// WorkerProgram is the runner application's worker program, in a
// runner root, which the worker check looks for (WorkerUp).
var WorkerProgram = filepath.Join("bin", "Runner.Worker.exe")

// CheckAccount makes the machine's service account, or checks it for
// drift, restoring what Silkpool owns, and records its SID in the agent's
// state (winaccount.Keep). It waits for an account-wide clean under way,
// which changes the account too.
func (s *Slots) CheckAccount(context.Context) error {
	s.use.RLock()
	defer s.use.RUnlock()
	s.account.Lock()
	defer s.account.Unlock()
	if s.keep != nil {
		return s.keep()
	}
	return winaccount.Keep(s.machine, s.Account, s.accountRecord())
}

// Ready readies slot n for a runner, as PrepareSlot asks, and returns
// what the node applies: Prepared, or PrepareFailed with SlotCantStart or
// SlotExposed. It checks the service account (CheckAccount), whose
// failure the slot names as winaccount.Detail words it; makes the slot's
// folder and its temporary folder, the account's (slotFolders); runs the
// account-wide clean if it's due (cleanFirst); and then the machine's
// exposure check (Exposure). Any failure but a hit is SlotCantStart: the
// slot can't be vouched for.
func (s *Slots) Ready(ctx context.Context, n int) node.Event {
	if err := s.CheckAccount(ctx); err != nil {
		return node.PrepareFailed{Slot: n, Reason: node.SlotCantStart, Detail: winaccount.Detail(err)}
	}
	err := s.slotFolders(n)
	if err == nil {
		err = s.cleanFirst(ctx)
	}
	if err == nil {
		err = s.Exposure(ctx)
	}
	var hit *winexposure.Exposed
	switch {
	case err == nil:
		return node.Prepared{Slot: n}
	case errors.As(err, &hit):
		return node.PrepareFailed{Slot: n, Reason: node.SlotExposed, Detail: hit.Detail}
	}
	return node.PrepareFailed{Slot: n, Reason: node.SlotCantStart, Detail: detail(err)}
}

// slotFolders makes slot n's folder, which inherits the root's
// descriptor, so the account may pass through it to its runner root and
// its temporary folder, but change nothing in it; and its temporary
// folder, the agent's with the account given full control, from the
// start. A temporary folder already there gets that descriptor again.
// Neither may be a link or a junction. The runner root is runnerinstall
// Link's, from the slot's folder.
func (s *Slots) slotFolders(n int) error {
	sid, err := s.serviceSID()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Folder(n), 0o755); err != nil {
		return fmt.Errorf("can't make slot %d's folder: %w", n, err)
	}
	if err := plainFolderAt(s.Folder(n)); err != nil {
		return fmt.Errorf("slot %d's folder isn't one: %w", n, err)
	}
	sd, err := accountsFolder(sid)
	if err != nil {
		return err
	}
	temp := s.TempDir(n)
	switch err := makeFolder(temp, sd); {
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		if err := plainFolderAt(temp); err != nil {
			return fmt.Errorf("slot %d's temporary folder isn't one: %w", n, err)
		}
		if err := secure(temp, sd); err != nil {
			return fmt.Errorf("can't give slot %d's temporary folder to the service account: %w", n, err)
		}
	case err != nil:
		return fmt.Errorf("can't make slot %d's temporary folder: %w", n, err)
	}
	return nil
}

// Start starts runner's listener in slot n, from the runner root the
// caller built, with its JIT config.
//
//  1. It makes sure the slot's service is Silkpool's, and points at the
//     slot hosts' copy for the agent's version, Program, making the
//     service if it's missing. It refuses a service of the slot's name
//     that runs a program other than a slot host's copy, and one whose
//     host still runs. A service moves to the agent's copy only here,
//     between its runners (winslot.Move), and the copies no service uses
//     are pruned then.
//  2. It records runner in the slot's folder, so that a restarted agent
//     knows whose host the slot's is, and makes the runner's folder in
//     the agent's logs.
//  3. It writes config to a file in the runner root that only the agent,
//     SYSTEM on a node, may open (winslot.ConfigFile): other slots' jobs
//     run as the same account, so it's never the account's.
//  4. It starts the service with the runner's winslot.Spec as its start
//     arguments, which no other account can read. The host reads and
//     deletes the config file, logs on as the account, and starts the
//     listener in the slot's job, nested in the node job, with its output
//     in the runner's ListenerLog.
//
// A slot runs one runner at a time. Start fails if the service doesn't
// start, and then deletes the config file. The account-wide clean waits
// for a Start, and a Start for the clean.
func (s *Slots) Start(ctx context.Context, n int, runner string, config secret.Value) error {
	if err := checkRunner(runner); err != nil {
		return err
	}
	s.use.RLock()
	defer s.use.RUnlock()
	sid, err := s.serviceSID()
	if err != nil {
		return err
	}
	name := s.ServiceName(n)
	if err := s.hostFor(name); err != nil {
		return fmt.Errorf("slot %d's service isn't ready: %w", n, err)
	}
	root := s.RunnerRoot(n)
	if err := plainFolderAt(root); err != nil {
		return fmt.Errorf("slot %d's runner root isn't ready: %w", n, err)
	}
	if err := plainFolderAt(s.TempDir(n)); err != nil {
		return fmt.Errorf("slot %d's temporary folder isn't ready: %w", n, err)
	}
	if err := os.MkdirAll(s.RunnerLogs(runner), 0o700); err != nil {
		return fmt.Errorf("can't make runner %s's folder in the agent's logs: %w", runner, err)
	}
	if err := writeRecord(filepath.Join(s.Folder(n), runnerFile), runner); err != nil {
		return fmt.Errorf("can't record runner %s in slot %d: %w", runner, n, err)
	}
	path := filepath.Join(root, winslot.ConfigFile)
	if err := writeConfig(path, config); err != nil {
		return fmt.Errorf("can't write the JIT config of runner %s: %w", runner, err)
	}
	job := winslot.SlotJob(name)
	// What a slot's last runner ran out of isn't this one's.
	s.outOfMemory(job)
	spec := winslot.Spec{
		Account: s.Account.Name, SID: sid,
		Root: root, Temp: s.TempDir(n), Log: s.ListenerLog(runner),
		NodeJob: s.NodeJob, Job: job,
	}
	if err := s.services.Start(name, spec); err != nil {
		os.Remove(path)
		return fmt.Errorf("the service manager didn't start runner %s's slot host %s: %w", runner, name, err)
	}
	return nil
}

// hostFor makes the slot service name, or checks it's Silkpool's and
// moves it to Program while it's stopped.
func (s *Slots) hostFor(name string) error {
	if s.Program == "" {
		return errors.New("there's no slot host's copy of Silkpool for its service to run")
	}
	st, err := s.services.State(name)
	if err != nil {
		return fmt.Errorf("can't read the slot service %s: %w", name, err)
	}
	switch st {
	case winslot.Missing:
		return s.services.Create(name, s.Program)
	case winslot.Active:
		return fmt.Errorf("its slot host %s is still running", name)
	}
	now, err := s.services.Program(name)
	if err != nil {
		return fmt.Errorf("can't read the slot service %s: %w", name, err)
	}
	if !s.isCopy(now) {
		return fmt.Errorf("the service %s runs %s, which isn't a copy of Silkpool for the slot hosts, so Silkpool won't use it", name, now)
	}
	if err := winslot.Move(s.services, name, s.Program); err != nil {
		return err
	}
	if !samePath(now, s.Program) {
		s.pruneCopies()
	}
	return nil
}

// isCopy reports whether program is a slot hosts' copy of silkpool.exe,
// in its version's folder in Copies.
func (s *Slots) isCopy(program string) bool {
	dir := s.Copies().Dir
	return samePath(filepath.Dir(filepath.Dir(program)), dir) && strings.EqualFold(filepath.Base(program), winslot.Program)
}

// PruneCopies deletes the slot hosts' copies of silkpool.exe that no
// slot's service runs, but Program's, as the agent does once it has made
// its own copy, and Start once it has moved a service to it. A copy a host
// still runs from is left: it's in use, and Windows refuses to delete it.
// A failure is left for a later prune, which finds it again.
func (s *Slots) PruneCopies() error {
	s.pruning.Lock()
	defer s.pruning.Unlock()
	numbers, err := s.slotNumbers()
	if err != nil {
		return err
	}
	var names []string
	for _, n := range numbers {
		names = append(names, s.ServiceName(n))
	}
	inUse, err := winslot.InUse(s.services, names)
	if err != nil {
		return err
	}
	_, err = s.Copies().Prune(filepath.Base(filepath.Dir(s.Program)), inUse)
	return err
}

func (s *Slots) pruneCopies() { s.PruneCopies() }

// SlotJobs returns the names of the job objects of the slots whose
// folders are there, which the node job's watch looks in
// (winslot.SlotJob).
func (s *Slots) SlotJobs() []string {
	numbers, _ := s.slotNumbers()
	var jobs []string
	for _, n := range numbers {
		jobs = append(jobs, winslot.SlotJob(s.ServiceName(n)))
	}
	return jobs
}

// slotNumbers returns the numbers of the slots whose folders are there.
func (s *Slots) slotNumbers() ([]int, error) {
	folders, err := names(filepath.Join(s.Root, "slots"))
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, f := range folders {
		if n, err := strconv.Atoi(f); err == nil && n > 0 {
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	return numbers, nil
}

// Stop stops runner's listener in slot n gracefully: its host sends it
// Ctrl+C (winslot.ControlCtrlC), and ends its job winslot.Grace later
// unless it has exited. It returns once the host has taken the control;
// Wait reports the end. A host still starting refuses controls, so Stop
// tells it again until it takes one, for up to hostWait. A slot whose
// host isn't running, or whose last runner is another, has nothing to
// stop.
func (s *Slots) Stop(ctx context.Context, n int, runner string) error {
	return s.control(ctx, n, runner, s.services.Stop)
}

// Kill ends runner's listener's job in slot n at once
// (winslot.ControlKill), with everything in it, as Stop tells its host.
func (s *Slots) Kill(ctx context.Context, n int, runner string) error {
	return s.control(ctx, n, runner, s.services.Kill)
}

func (s *Slots) control(ctx context.Context, n int, runner string, send func(name string) error) error {
	if err := checkRunner(runner); err != nil {
		return err
	}
	if s.recordedRunner(n) != runner {
		return nil
	}
	name := s.ServiceName(n)
	deadline := time.Now().Add(s.hostWait)
	for {
		st, err := s.services.State(name)
		if err != nil || st != winslot.Active {
			return err
		}
		err = send(name)
		if err == nil || errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return nil
		}
		if !errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-time.After(s.poll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Wait waits for runner's listener in slot n to end, and returns how it
// ended: the listener's exit code, as its host stopped with it, and
// whether a process in the slot's job ran out of the node's memory budget
// meanwhile. The service manager keeps a stopped service's last exit
// until it next starts, so a runner that ended while the agent was down
// is read the same way. A host that failed, and so ran no listener or
// lost it, is an exit of 1: the host noted why in the listener's log.
// It's ErrGone if the slot's last runner is another, or its service is
// gone.
func (s *Slots) Wait(ctx context.Context, n int, runner string) (Exit, error) {
	if err := checkRunner(runner); err != nil {
		return Exit{}, err
	}
	name := s.ServiceName(n)
	tick := time.NewTicker(s.poll)
	defer tick.Stop()
	for {
		if s.recordedRunner(n) != runner {
			return Exit{}, ErrGone
		}
		st, err := s.services.State(name)
		if err != nil {
			return Exit{}, err
		}
		switch st {
		case winslot.Missing:
			return Exit{}, ErrGone
		case winslot.Stopped:
			code, err := s.services.Exit(name)
			exit := Exit{Code: int(code), OutOfMemory: s.outOfMemory(winslot.SlotJob(name))}
			if err != nil {
				exit.Code = 1
			}
			return exit, nil
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return Exit{}, ctx.Err()
		}
	}
}

// outOfMemory asks the node job whether the slot job named job ran out
// of the memory budget, and forgets it.
func (s *Slots) outOfMemory(job string) bool {
	return s.Jobs != nil && s.Jobs.OutOfMemory(job)
}

// WorkerUp reports whether runner's worker is up in slot n: whether a
// process in the slot's job object runs slot n's runner root's
// WorkerProgram, from "Decide what Emergency stop does in a job's first
// second" (#123). Every slot runs as the one service account (#214), so
// it looks in the slot host's job, which holds the slot's processes
// alone and allows no breakaway, never among the account's processes:
// it never counts another runner's worker. The job is SYSTEM's alone,
// and no job code runs before the worker, so a job can't fake one. A
// slot whose host has stopped, or whose last runner is another, has
// none. It may run beside Wait, Stop and Kill on the same slot.
func (s *Slots) WorkerUp(_ context.Context, n int, runner string) (bool, error) {
	if err := checkRunner(runner); err != nil {
		return false, err
	}
	if s.recordedRunner(n) != runner {
		return false, nil
	}
	read := winslot.Programs
	if s.programs != nil {
		read = s.programs
	}
	programs, err := read(winslot.SlotJob(s.ServiceName(n)))
	if err != nil {
		return false, err
	}
	worker := filepath.Join(s.RunnerRoot(n), WorkerProgram)
	return slices.ContainsFunc(programs, func(p string) bool { return samePath(p, worker) }), nil
}

// Alive returns the runners whose slots' hosts are still running, sorted:
// what a restarted agent tells the node as Started's Alive, and follows
// again (winslot.Adopt).
func (s *Slots) Alive(context.Context) ([]string, error) {
	recorded, err := s.Recorded()
	if err != nil {
		return nil, err
	}
	bySlot := map[string]string{}
	for n, runner := range recorded {
		bySlot[s.ServiceName(n)] = runner
	}
	active, err := winslot.Adopt(s.services, slices.Sorted(maps.Keys(bySlot)))
	if err != nil {
		return nil, err
	}
	var alive []string
	for _, name := range active {
		alive = append(alive, bySlot[name])
	}
	slices.Sort(alive)
	return alive, nil
}

// DeleteSlot deletes what slot n keeps on the machine, as DeleteSlot
// asks once the slot is idle: it ends its host, if it still runs, and
// waits until nothing is left in its job (endHost), deletes its service,
// and deletes its folder. It leaves a
// service of the slot's name that isn't Silkpool's, and fails.
func (s *Slots) DeleteSlot(ctx context.Context, n int) node.Event {
	err := s.deleteSlot(ctx, n)
	if err != nil {
		return node.DeleteFailed{Slot: n, Detail: detail(fmt.Errorf("couldn't delete slot %d: %w", n, err))}
	}
	return node.SlotDeleted{Slot: n}
}

func (s *Slots) deleteSlot(ctx context.Context, n int) error {
	name := s.ServiceName(n)
	st, err := s.services.State(name)
	if err != nil {
		return err
	}
	if st != winslot.Missing {
		program, err := s.services.Program(name)
		if err != nil {
			return err
		}
		if !s.isCopy(program) {
			return fmt.Errorf("the service %s runs %s, which isn't a copy of Silkpool for the slot hosts, so Silkpool won't delete it", name, program)
		}
		if err := s.endHost(ctx, n); err != nil {
			return err
		}
		if err := s.services.Delete(name); err != nil {
			return fmt.Errorf("can't delete its service %s: %w", name, err)
		}
	}
	return os.RemoveAll(s.Folder(n))
}

// SetBudget holds the slots to the budget b, through the node job.
func (s *Slots) SetBudget(_ context.Context, b availability.Budget) error {
	if s.Jobs == nil {
		return errors.New("there's no node job to hold the slots to it")
	}
	return s.Jobs.SetBudget(b)
}

// samePath reports whether a and b name the same path, as Windows
// compares them: ignoring case, and either separator.
func samePath(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

// agentSID is the SID of the account the agent runs as: LocalSystem on a
// node, and a test's own account in the windows lane.
func agentSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("finding the agent's account: %w", err)
	}
	return u.User.Sid.String(), nil
}

// accountsFolder is the descriptor of a slot's temporary folder: the
// agent's, with the service account sid given full control, inherited by
// what's made in it, and protected from what's above it.
func accountsFolder(sid string) (*windows.SECURITY_DESCRIPTOR, error) {
	agent, err := agentSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf("O:%[1]sD:P(A;OICI;FA;;;%[1]s)(A;OICI;FA;;;%[2]s)", agent, sid))
}

// agentsFile is the descriptor of a runner's JIT config file: the
// agent's alone, protected from what's above it.
func agentsFile() (*windows.SECURITY_DESCRIPTOR, error) {
	agent, err := agentSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf("O:%[1]sD:P(A;;FA;;;%[1]s)", agent))
}

// attributesOf returns sd as the attributes a new file or folder is made
// with, so it never exists without it.
func attributesOf(sd *windows.SECURITY_DESCRIPTOR) *windows.SecurityAttributes {
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa
}

// makeFolder makes the folder path with sd from the start. It's
// windows.ERROR_ALREADY_EXISTS if something is there.
func makeFolder(path string, sd *windows.SECURITY_DESCRIPTOR) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.CreateDirectory(p, attributesOf(sd))
}

// secure gives what's at path, which isn't a link, the owner and the
// protected DACL of sd.
func secure(path string, sd *windows.SECURITY_DESCRIPTOR) error {
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
}

// plainFolderAt checks that path is a folder, not a link, a junction or
// any other reparse point, opening it as it is.
func plainFolderAt(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	ok, err := plainFolder(h)
	if err == nil && !ok {
		err = fmt.Errorf("%s is a link or isn't a folder", path)
	}
	return err
}

// writeConfig writes config to a new file at path that only the agent
// may open, from the start (agentsFile). It never opens what's there: a
// file an earlier attempt left is deleted first, and anything else in
// its place fails it.
func writeConfig(path string, config secret.Value) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sd, err := agentsFile()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, attributesOf(sd), windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "create", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	_, err = f.WriteString(config.Reveal())
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(path)
	}
	return err
}

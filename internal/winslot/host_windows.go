package winslot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/meschoening/silkpool/internal/winacl"
)

// Main runs silkpool.exe as a slot host, as the service manager starts
// it in a slot's service, and returns the code to exit with. It runs
// only as LocalSystem, under the service manager, since S4U needs
// SeTcbPrivilege. Its start arguments are the runner's Spec.
func Main(args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "silkpool %s takes its runner from its service's start arguments, not its command line\n", Mode)
		return 2
	}
	if err := runsAsSystem(); err != nil {
		fmt.Fprintf(os.Stderr, "silkpool %s: %v\n", Mode, err)
		return 1
	}
	inService, err := svc.IsWindowsService()
	if err != nil || !inService {
		fmt.Fprintf(os.Stderr, "silkpool %s: a slot host runs only as a slot's service, which the service manager starts\n", Mode)
		return 1
	}
	// A service of its own process gets its name from the SCM, so the
	// name here isn't read: every slot's service runs a host.
	if err := svc.Run(Mode, service{}); err != nil {
		return 1
	}
	return 0
}

// runsAsSystem fails unless the process runs as LocalSystem.
func runsAsSystem() error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !u.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return errors.New("a slot host runs only as LocalSystem")
	}
	return nil
}

// service is a slot's service, which hosts one runner's listener.
type service struct{}

// Execute hosts the runner its start arguments name until its listener
// exits. ControlCtrlC, or a stop or shutdown, sends Ctrl+C, and
// ControlKill ends the listener's job (controls). The service stays
// Running until the listener has ended, since the service manager
// refuses a service's own controls to one that's stopping: so a kill
// still reaches a listener that ignores Ctrl+C before its grace is up,
// once the agent's ControlCtrlC sent it. The service stops with the
// listener's exit code as its service-specific exit code, or, if the
// host failed, a Windows error.
func (service) Execute(args []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	spec, err := ParseSpec(args[1:])
	if err != nil {
		return false, uint32(windows.ERROR_INVALID_PARAMETER)
	}
	c := newControls()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case r := <-requests:
				switch r.Cmd {
				case svc.Interrogate:
					status <- r.CurrentStatus
				case svc.Stop, svc.Shutdown:
					status <- r.CurrentStatus
					fallthrough
				default:
					c.take(r.Cmd)
				}
			}
		}
	}()
	h := host{spec: spec, logon: s4u, grace: Grace, owner: winacl.WellKnown(windows.WinLocalSystemSid)}
	code, err := h.run(c.stop, c.kill, func(int) {
		status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	})
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && errno != 0 {
			return false, uint32(errno)
		}
		return false, uint32(windows.ERROR_GEN_FAILURE)
	}
	if code != 0 {
		return true, code
	}
	return false, 0
}

// controls are the host's stop and kill, as the service controls it
// takes close them.
type controls struct {
	stop, kill         chan struct{}
	stopOnce, killOnce sync.Once
}

func newControls() *controls {
	return &controls{stop: make(chan struct{}), kill: make(chan struct{})}
}

// take acts on the service control cmd: ControlCtrlC, a stop and a
// shutdown close stop, ControlKill closes kill, and any other does
// nothing.
func (c *controls) take(cmd svc.Cmd) {
	switch cmd {
	case svc.Cmd(ControlCtrlC), svc.Stop, svc.Shutdown:
		c.stopOnce.Do(func() { close(c.stop) })
	case svc.Cmd(ControlKill):
		c.killOnce.Do(func() { close(c.kill) })
	}
}

// A session is the account the listener runs as: its token, and end,
// which unloads its profile and closes the token. Tests run the host
// with no token, as themselves, or with their own token, which the host
// takes bypassing traverse checking from as it does from the account's.
type session struct {
	token windows.Token
	end   func() error
}

// host hosts one runner's listener.
type host struct {
	spec Spec
	// logon logs on as the spec's account and loads its profile.
	logon func(Spec) (session, error)
	// grace is how long after Ctrl+C the job is terminated.
	grace time.Duration
	// owner is the account the node job and the slot's job are open to
	// alone: SYSTEM, or in tests their own account.
	owner *windows.SID
}

// Terminated is the exit code of a listener whose job the host
// terminated, actions/runner's TerminatedError.
const Terminated = 1

// run hosts the listener until it exits, and returns its exit code.
// started is called once the listener runs, with its process ID. A
// close of stop sends it Ctrl+C, and its job is terminated h.grace later
// unless it has exited; a close of kill terminates the job at once.
// Whatever is left in the job once the listener exits ends with it, and
// run returns only once nothing runs in the job.
//
// The host's own failures, and its terminations, are noted in the
// listener's log, which the agent tails.
func (h host) run(stop, kill <-chan struct{}, started func(pid int)) (uint32, error) {
	log, err := os.OpenFile(h.spec.Log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("opening the listener's log: %w", err)
	}
	defer log.Close()
	code, err := h.host(log, stop, kill, started)
	if err != nil {
		note(log, "%v", err)
	}
	return code, err
}

func (h host) host(log *os.File, stop, kill <-chan struct{}, started func(pid int)) (uint32, error) {
	config, err := takeConfig(h.spec.Root)
	if err != nil {
		return 0, err
	}
	s, err := h.logon(h.spec)
	if err != nil {
		return 0, fmt.Errorf("logging on as %s: %w", h.spec.Account, err)
	}
	defer s.end()
	env := os.Environ()
	// The listener, and so every job, runs as the account's token without
	// bypassing traverse checking (WithoutTraverseBypass, ADR 0020).
	var runAs windows.Token
	if s.token != 0 {
		if env, err = s.token.Environ(false); err != nil {
			return 0, fmt.Errorf("reading %s's environment: %w", h.spec.Account, err)
		}
		if runAs, err = WithoutTraverseBypass(s.token); err != nil {
			return 0, fmt.Errorf("%s's token: %w", h.spec.Account, err)
		}
		defer runAs.Close()
	}
	// The host holds the node job as long as its listener runs, so the
	// job and its limits outlive the agent's restarts. With no node job,
	// it starts nothing, rather than a listener the budget doesn't hold.
	node, ok, err := openJob(h.spec.NodeJob, jobAssignProcess, h.owner)
	switch {
	case err != nil:
		return 0, fmt.Errorf("opening the node job: %w", err)
	case !ok:
		return 0, fmt.Errorf("there's no node job %s for the listener's job to go in", h.spec.NodeJob)
	}
	defer windows.CloseHandle(node)
	job, err := newSlotJob(h.spec.Job, h.owner)
	if err != nil {
		return 0, fmt.Errorf("making the listener's job object: %w", err)
	}
	defer windows.CloseHandle(job) // ends whatever is left in it
	cmd := exec.Command(filepath.Join(h.spec.Root, Listener), "run")
	cmd.Dir = h.spec.Root
	cmd.Env = listenerEnv(env, h.spec, config)
	cmd.Stdout, cmd.Stderr = log, log
	// The listener gets a console of its own, with no window, for the
	// host to attach to and send Ctrl+C.
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(runAs), CreationFlags: windows.CREATE_NO_WINDOW}
	if err := takeCtrlC(); err != nil {
		return 0, fmt.Errorf("letting the listener take Ctrl+C: %w", err)
	}
	if err := startInJob(cmd, node, job); err != nil {
		return 0, fmt.Errorf("starting the listener: %w", err)
	}
	pid := cmd.Process.Pid
	started(pid)
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	terminate := func(why string) {
		note(log, "%s, so it ended the listener's job", why)
		windows.TerminateJobObject(job, Terminated)
	}
	select {
	case <-exited:
	case <-kill:
		terminate("it was told to")
	case <-stop:
		if err := interrupt(uint32(pid)); err != nil {
			terminate(fmt.Sprintf("it couldn't send the listener Ctrl+C (%v)", err))
			break
		}
		select {
		case <-exited:
		case <-kill:
			terminate("it was told to")
		case <-time.After(h.grace):
			terminate(fmt.Sprintf("the listener ran on %s after Ctrl+C", h.grace))
		}
	}
	<-exited
	if err := endJob(job, jobEndLimit); err != nil {
		return 0, err
	}
	return uint32(cmd.ProcessState.ExitCode()), nil
}

// takeConfig reads the runner's JIT config from its file in root, and
// deletes the file, so the runner can't start from it again.
func takeConfig(root string) (string, error) {
	path := filepath.Join(root, ConfigFile)
	config, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("can't read the runner's JIT config: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("can't delete the runner's JIT config file: %w", err)
	}
	return string(config), nil
}

// note adds the host's line to the listener's log.
func note(log io.Writer, format string, args ...any) {
	fmt.Fprintf(log, "Silkpool's slot host: "+format+"\n", args...)
}

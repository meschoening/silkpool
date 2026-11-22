package winslot

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// SCM is Windows' service manager, as the slot hosts' rules and the
// agent use it. Reading a service's state and program needs no
// administrator; making, changing, starting, controlling and deleting
// one does.
type SCM struct{}

var _ Services = SCM{}

// Marker is the description every slot service carries.
const Marker = "Silkpool slot host"

// Create makes the slot service name: demand-start, running program in
// slot-host mode as LocalSystem, and carrying Marker. No slot service
// starts at boot (ADR 0006).
func (SCM) Create(name, program string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.CreateService(name, program, mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartManual,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  "Silkpool slot " + name,
		Description:  Marker,
	}, Mode)
	if err != nil {
		return fmt.Errorf("making the slot service %s: %w", name, err)
	}
	return s.Close()
}

// open opens the service name with access, and reports false if it
// doesn't exist.
func open(name string, managerAccess, access uint32) (*mgr.Service, bool, error) {
	m, err := windows.OpenSCManager(nil, nil, managerAccess)
	if err != nil {
		return nil, false, fmt.Errorf("connecting to the service manager: %w", err)
	}
	defer windows.CloseServiceHandle(m)
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, err
	}
	h, err := windows.OpenService(m, p, access)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &mgr.Service{Name: name, Handle: h}, true, nil
}

// use opens the service name with access and calls f with it, failing
// if it doesn't exist.
func use(name string, access uint32, f func(*mgr.Service) error) error {
	s, ok, err := open(name, windows.SC_MANAGER_CONNECT, access)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the slot service %s doesn't exist", name)
	}
	defer s.Close()
	return f(s)
}

// State returns the service name's state.
func (SCM) State(name string) (State, error) {
	st, err := status(name)
	if errors.Is(err, errMissing) {
		return Missing, nil
	}
	if err != nil {
		return 0, err
	}
	if st.State == svc.Stopped {
		return Stopped, nil
	}
	return Active, nil
}

var errMissing = errors.New("the service doesn't exist")

// status queries the service name's status.
func status(name string) (svc.Status, error) {
	s, ok, err := open(name, windows.SC_MANAGER_CONNECT, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return svc.Status{}, err
	}
	if !ok {
		return svc.Status{}, errMissing
	}
	defer s.Close()
	return s.Query()
}

// Program returns the program the service name runs: the first word of
// its command line, unquoted.
func (SCM) Program(name string) (string, error) {
	var c mgr.Config
	err := use(name, windows.SERVICE_QUERY_CONFIG, func(s *mgr.Service) (err error) {
		c, err = s.Config()
		return err
	})
	if err != nil {
		return "", err
	}
	return program(c.BinaryPathName), nil
}

// program returns the program a service's command line runs.
func program(line string) string {
	if rest, ok := strings.CutPrefix(line, `"`); ok {
		p, _, _ := strings.Cut(rest, `"`)
		return p
	}
	p, _, _ := strings.Cut(line, " ")
	return p
}

// SetProgram makes the service name run program in slot-host mode,
// changing nothing else of it.
func (SCM) SetProgram(name, program string) error {
	line, err := windows.UTF16PtrFromString(syscall.EscapeArg(program) + " " + Mode)
	if err != nil {
		return err
	}
	return use(name, windows.SERVICE_CHANGE_CONFIG, func(s *mgr.Service) error {
		const same = windows.SERVICE_NO_CHANGE
		return windows.ChangeServiceConfig(s.Handle, same, same, same, line, nil, nil, nil, nil, nil, nil)
	})
}

// Start starts the slot service name's host with the runner spec.
func (SCM) Start(name string, spec Spec) error {
	return use(name, windows.SERVICE_START, func(s *mgr.Service) error { return s.Start(spec.Args()...) })
}

// Stop asks the slot service name's host to stop its listener: Ctrl+C,
// then its job's end Grace later. It returns at once; the service is
// Stopped once the listener and its job have ended. It sends
// ControlCtrlC, never the service's stop control, after which the
// service manager would refuse Kill.
func (SCM) Stop(name string) error {
	return use(name, windows.SERVICE_USER_DEFINED_CONTROL, func(s *mgr.Service) error {
		_, err := s.Control(svc.Cmd(ControlCtrlC))
		return err
	})
}

// Kill tells the slot service name's host to end its listener's job at
// once.
func (SCM) Kill(name string) error {
	return use(name, windows.SERVICE_USER_DEFINED_CONTROL, func(s *mgr.Service) error {
		_, err := s.Control(svc.Cmd(ControlKill))
		return err
	})
}

// Exit returns how the slot service name's last host ended, once it's
// stopped: the listener's exit code, or the host's own failure.
func (SCM) Exit(name string) (code uint32, err error) {
	st, err := status(name)
	if err != nil {
		return 0, err
	}
	if st.State != svc.Stopped {
		return 0, fmt.Errorf("the slot service %s hasn't stopped", name)
	}
	switch st.Win32ExitCode {
	case 0:
		return 0, nil
	case uint32(windows.ERROR_SERVICE_SPECIFIC_ERROR):
		return st.ServiceSpecificExitCode, nil
	}
	return 0, fmt.Errorf("the slot host failed: %w", syscall.Errno(st.Win32ExitCode))
}

// Delete deletes the slot service name, which goes once its host has
// stopped and nothing holds it open. One that doesn't exist is gone
// already.
func (SCM) Delete(name string) error {
	s, ok, err := open(name, windows.SC_MANAGER_CONNECT, windows.DELETE)
	if err != nil || !ok {
		return err
	}
	defer s.Close()
	return s.Delete()
}

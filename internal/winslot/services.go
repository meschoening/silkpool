package winslot

import (
	"errors"
	"fmt"
)

// A State is how the service manager shows a slot's service.
type State int

const (
	// Missing is a service that doesn't exist.
	Missing State = iota
	// Stopped is a service whose host isn't running: the slot is free.
	Stopped
	// Active is a service in any other state, starting, running or
	// stopping: its host runs, and may hold a listener.
	Active
)

func (s State) String() string {
	switch s {
	case Missing:
		return "missing"
	case Stopped:
		return "stopped"
	}
	return "active"
}

// Services is what the slot hosts' rules read and change of the
// service manager: SCM on Windows, and fakes in the tests, so the rules
// are tested on every OS.
type Services interface {
	// State returns the service name's state.
	State(name string) (State, error)
	// Program returns the program the service name runs.
	Program(name string) (string, error)
	// SetProgram makes the service name run program, in slot-host mode.
	SetProgram(name, program string) error
}

// ErrActive is Move's refusal of a service whose host runs.
var ErrActive = errors.New("its slot host is running")

// Move points the slot service name at program, the copy for the
// agent's version, while the slot is free between runners. It refuses
// while the service's host runs, so a runner always finishes on the
// copy it started from. A service already there is left as it is.
func Move(s Services, name, program string) error {
	st, err := s.State(name)
	if err != nil {
		return fmt.Errorf("reading the slot service %s: %w", name, err)
	}
	switch st {
	case Missing:
		return fmt.Errorf("the slot service %s doesn't exist", name)
	case Active:
		return fmt.Errorf("can't move the slot service %s to %s: %w", name, program, ErrActive)
	}
	now, err := s.Program(name)
	if err != nil {
		return fmt.Errorf("reading the slot service %s: %w", name, err)
	}
	if samePath(now, program) {
		return nil
	}
	if err := s.SetProgram(name, program); err != nil {
		return fmt.Errorf("moving the slot service %s to %s: %w", name, program, err)
	}
	return nil
}

// InUse returns the programs the slot services names run, for
// Copies.Prune. A service that doesn't exist uses none.
func InUse(s Services, names []string) ([]string, error) {
	var programs []string
	for _, n := range names {
		st, err := s.State(n)
		if err != nil {
			return nil, fmt.Errorf("reading the slot service %s: %w", n, err)
		}
		if st == Missing {
			continue
		}
		p, err := s.Program(n)
		if err != nil {
			return nil, fmt.Errorf("reading the slot service %s: %w", n, err)
		}
		programs = append(programs, p)
	}
	return programs, nil
}

// Adopt returns which of the slot services names still have a host
// running, as a restarted agent asks: each one's runner outlived the
// agent, and the agent follows it again.
func Adopt(s Services, names []string) ([]string, error) {
	var active []string
	for _, n := range names {
		st, err := s.State(n)
		if err != nil {
			return nil, fmt.Errorf("reading the slot service %s: %w", n, err)
		}
		if st == Active {
			active = append(active, n)
		}
	}
	return active, nil
}

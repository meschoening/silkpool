// Package winsac reads Windows' Smart App Control, which blocks GitHub's
// unsigned runner application whether or not Silkpool is signed
// (actions/runner#4770). So a node runs jobs only while it's off, as
// "Define how each binary is packaged, installed, upgraded and signed per
// OS" decided (ADR 0013): before each runner, the agent reads its state,
// and while it's On the slot can't start, which the node shows as a
// Degraded reason, not as a runner that never came online; `enroll` says
// so while it's On or in Evaluation.
//
// Windows keeps the state in VerifiedAndReputablePolicyState, a DWORD
// under HKLM\SYSTEM\CurrentControlSet\Control\CI\Policy that any account
// may read: 0 is Off, 1 On and 2 Evaluation, as Microsoft's "Test App
// Signatures with Smart App Control" documents.
package winsac

import "fmt"

// A State is Smart App Control's state.
type State int

const (
	// Off blocks nothing. A Windows without Smart App Control, which
	// has no value, is Off too.
	Off State = iota
	// On blocks programs without a trusted signature or a good
	// reputation, GitHub's runner application among them.
	On
	// Evaluation blocks nothing, while Windows works out whether to turn
	// it On or Off by itself.
	Evaluation
)

func (s State) String() string {
	switch s {
	case Off:
		return "Off"
	case On:
		return "On"
	case Evaluation:
		return "Evaluation"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// Blocks reports whether s blocks GitHub's runner application.
func (s State) Blocks() bool { return s == On }

// Warns reports whether enroll should say Smart App Control blocks the
// runner application: while it's On, and in Evaluation, which may turn
// it On.
func (s State) Warns() bool { return s == On || s == Evaluation }

// FromValue returns the State VerifiedAndReputablePolicyState's value v
// names, or an error for a value Microsoft doesn't document.
func FromValue(v uint64) (State, error) {
	switch v {
	case 0:
		return Off, nil
	case 1:
		return On, nil
	case 2:
		return Evaluation, nil
	}
	return Off, fmt.Errorf("the Smart App Control state reads %d, which isn't Off (0), On (1) or Evaluation (2)", v)
}

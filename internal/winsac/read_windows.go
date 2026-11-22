package winsac

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// Where Windows keeps the state.
const (
	policyKey   = `SYSTEM\CurrentControlSet\Control\CI\Policy`
	policyValue = "VerifiedAndReputablePolicyState"
)

// Read reads Smart App Control's state from the machine's registry,
// which needs no privilege. A machine with no value, or no key, is Off:
// Windows 10 and Windows Server have no Smart App Control.
func Read() (State, error) { return read(registry.LOCAL_MACHINE, policyKey) }

// read reads the state from the value under root\path.
func read(root registry.Key, path string) (State, error) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return Off, nil
	}
	if err != nil {
		return Off, fmt.Errorf("reading Smart App Control's state: %w", err)
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(policyValue)
	if errors.Is(err, registry.ErrNotExist) {
		return Off, nil
	}
	if err != nil {
		return Off, fmt.Errorf("reading Smart App Control's state: %w", err)
	}
	return FromValue(v)
}

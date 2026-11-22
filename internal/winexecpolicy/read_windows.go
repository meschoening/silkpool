package winexecpolicy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Where Windows and each PowerShell keep what Read reads, under
// HKEY_LOCAL_MACHINE.
const (
	// windowsPolicyKey is Windows PowerShell's "Turn on Script
	// Execution", and corePolicyKey PowerShell 7's.
	windowsPolicyKey = `SOFTWARE\Policies\Microsoft\Windows\PowerShell`
	corePolicyKey    = `SOFTWARE\Policies\Microsoft\PowerShellCore`
	// shellIDsKey holds Windows PowerShell's LocalMachine policy.
	shellIDsKey = `SOFTWARE\Microsoft\PowerShell\1\ShellIds\Microsoft.PowerShell`
	// environmentKey holds the machine's environment, whose Path is the
	// PATH of the service account a job runs as.
	environmentKey = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
)

// The values PowerShell reads there.
const (
	enableScriptsValue   = "EnableScripts"
	executionPolicyValue = "ExecutionPolicy"
	useWindowsValue      = "UseWindowsPowerShellPolicySetting"
	pathValue            = "Path"
)

// verNTWorkstation is OSVERSIONINFOEXW's ProductType of a Windows
// client.
const verNTWorkstation = 1

// configName is PowerShell 7's configuration file, in $PSHOME.
const configName = "powershell.config.json"

// where is where read looks: the registry's keys under root, and whether
// the machine is a Windows Server.
type where struct {
	root                                             registry.Key
	windowsPolicy, corePolicy, shellIDs, environment string
	server                                           bool
}

// Read reads the policy each PowerShell's scripts meet on this machine,
// Windows PowerShell's first and then PowerShell 7's, as a job's step
// meets it. It needs no privilege.
func Read() []Reading {
	return read(where{
		root:          registry.LOCAL_MACHINE,
		windowsPolicy: windowsPolicyKey, corePolicy: corePolicyKey,
		shellIDs: shellIDsKey, environment: environmentKey,
		// Any product type but a workstation's is a server's: a domain
		// controller's too.
		server: windows.RtlGetVersion().ProductType != verNTWorkstation,
	})
}

// read reads both shells' policies from w.
func read(w where) []Reading {
	return []Reading{readWindows(w), readCore(w)}
}

// readWindows reads Windows PowerShell's policy. Each scope is read only
// when no higher one sets a policy, so a lower scope that can't be read
// doesn't hide one that can.
func readWindows(w where) Reading {
	r := Reading{Shell: WindowsPowerShell, Installed: true}
	settings := Settings{Default: Restricted}
	if w.server {
		settings.Default = RemoteSigned
	}
	if settings.MachinePolicy, _, r.Err = scriptExecution(w.root, w.windowsPolicy); r.Err != nil {
		return r
	}
	if !settings.MachinePolicy.SetsPolicy() {
		local, err := stringValue(w.root, w.shellIDs, executionPolicyValue)
		if err != nil {
			r.Err = fmt.Errorf("reading Windows PowerShell's LocalMachine policy: %w", err)
			return r
		}
		settings.LocalMachine = local
	}
	r.Policy, r.Scope = settings.Effective()
	return r
}

// readCore reads PowerShell 7's policy, from the $PSHOME of the pwsh.exe
// a job's step would find on the machine's PATH. As for Windows
// PowerShell, each scope is read only when no higher one sets a policy.
func readCore(w where) Reading {
	r := Reading{Shell: PowerShell7}
	home, err := pshome(w)
	if err != nil || home == "" {
		r.Err = err
		return r
	}
	r.Installed = true
	settings := Settings{Default: Restricted}
	core, useWindows, err := scriptExecution(w.root, w.corePolicy)
	if err != nil {
		r.Err = err
		return r
	}
	var windowsGP ScriptExecution
	if useWindows {
		if windowsGP, _, err = scriptExecution(w.root, w.windowsPolicy); err != nil {
			r.Err = err
			return r
		}
	}
	settings.MachinePolicy = CoreMachinePolicy(core, useWindows, windowsGP, ScriptExecution{})
	if !settings.MachinePolicy.SetsPolicy() {
		config, err := readConfig(home)
		if err != nil {
			r.Err = err
			return r
		}
		// The configuration's PowerShellPolicies count only when Group
		// Policy sets neither value.
		if !settings.MachinePolicy.Set() {
			if config.PoliciesErr != nil {
				r.Err = config.PoliciesErr
				return r
			}
			settings.MachinePolicy = config.Policies
		}
		if !settings.MachinePolicy.SetsPolicy() {
			if config.LocalMachineErr != nil {
				r.Err = config.LocalMachineErr
				return r
			}
			settings.LocalMachine = config.ExecutionPolicy
		}
	}
	r.Policy, r.Scope = settings.Effective()
	return r
}

// readConfig reads $PSHOME's powershell.config.json, which a home
// without one holds nothing of.
func readConfig(home string) (Config, error) {
	path := filepath.Join(home, configName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading PowerShell 7's configuration: %w", err)
	}
	c, err := ParseConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// pshome returns the folder of the first pwsh.exe on the machine's Path,
// or "" when there's none: PowerShell 7 isn't installed for the service
// account's jobs. A job's environment is the account's, which the
// machine's holds, since its own is deleted with its profile.
func pshome(w where) (string, error) {
	path, err := stringValue(w.root, w.environment, pathValue)
	if err != nil {
		return "", fmt.Errorf("reading the machine's PATH: %w", err)
	}
	if path, err = registry.ExpandString(path); err != nil {
		return "", fmt.Errorf("expanding the machine's PATH: %w", err)
	}
	for dir := range strings.SplitSeq(path, ";") {
		dir = strings.Trim(strings.TrimSpace(dir), `"`)
		if !filepath.IsAbs(dir) {
			continue
		}
		exe := filepath.Join(dir, "pwsh.exe")
		if fi, err := os.Stat(exe); err != nil || fi.IsDir() {
			continue
		}
		// $PSHOME is the folder of the program itself, past any link.
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		return filepath.Dir(exe), nil
	}
	return "", nil
}

// scriptExecution reads "Turn on Script Execution" from root\path, as
// PowerShell does: EnableScripts counts only as a DWORD of 0 or 1, and
// ExecutionPolicy only as a string. It also returns whether
// UseWindowsPowerShellPolicySetting is a DWORD other than 0. A missing
// key sets nothing.
func scriptExecution(root registry.Key, path string) (ScriptExecution, bool, error) {
	var s ScriptExecution
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("reading Group Policy's execution policy: %w", err)
	}
	defer k.Close()
	enable, set, err := dword(k, enableScriptsValue)
	if err != nil {
		return s, false, err
	}
	if set && (enable == 0 || enable == 1) {
		b := enable == 1
		s.EnableScripts = &b
	}
	policy, _, err := k.GetStringValue(executionPolicyValue)
	switch {
	case err == nil:
		s.ExecutionPolicy = policy
	case !errors.Is(err, registry.ErrNotExist) && !errors.Is(err, registry.ErrUnexpectedType):
		return s, false, fmt.Errorf("reading Group Policy's execution policy: %w", err)
	}
	useWindows, set, err := dword(k, useWindowsValue)
	if err != nil {
		return s, false, err
	}
	return s, set && useWindows != 0, nil
}

// dword reads k's DWORD name, and whether it's set as one.
func dword(k registry.Key, name string) (uint64, bool, error) {
	v, typ, err := k.GetIntegerValue(name)
	switch {
	case errors.Is(err, registry.ErrNotExist), errors.Is(err, registry.ErrUnexpectedType):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("reading Group Policy's execution policy: %w", err)
	}
	return v, typ == registry.DWORD, nil
}

// stringValue reads the string name under root\path, or "" when the key
// or the value is missing.
func stringValue(root registry.Key, path, name string) (string, error) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return v, err
}

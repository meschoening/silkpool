// Package winexecpolicy reads the PowerShell execution policy that a
// job's PowerShell step meets on a Windows node. GitHub's runner writes a
// `pwsh` or `powershell` step to a .ps1 file and runs it as a script, as
// Silkpool's service account, so a policy that refuses unsigned local
// scripts, Restricted or AllSigned, fails every such step at once. The
// node stays healthy, and runs every other job, so it isn't a Degraded
// reason: `enroll` names it, and the diagnostics report shows it.
//
// Windows PowerShell and PowerShell 7 keep separate settings, and
// PowerShell takes the first scope that sets one, in this order
// (about_Execution_Policies; PowerShell 7's SecuritySupport.cs):
//
//   - MachinePolicy, Group Policy's "Turn on Script Execution" for the
//     computer. Windows PowerShell's is under
//     HKLM\Software\Policies\Microsoft\Windows\PowerShell (the Policy
//     CSP's ADMX_PowerShellExecutionPolicy), PowerShell 7's under
//     HKLM\Software\Policies\Microsoft\PowerShellCore, unless its
//     UseWindowsPowerShellPolicySetting says to take Windows
//     PowerShell's (PowerShellCoreExecutionPolicy.admx; Utils.cs). Both
//     hold EnableScripts, a DWORD, 0 for Restricted and 1 for the
//     ExecutionPolicy string beside it. PowerShell 7 falls back to the
//     PowerShellPolicies of $PSHOME\powershell.config.json when Group
//     Policy sets neither value (about_PowerShell_Config).
//   - UserPolicy, the same for the user, and Process, CurrentUser: none
//     of these is read. The account a job runs as is Silkpool's service
//     account, whose profile, which holds CurrentUser and the user's
//     Group Policy, the account-wide clean deletes. A job's process sets
//     no Process policy unless its own step does.
//   - LocalMachine. Windows PowerShell's is the ExecutionPolicy value
//     under HKLM\Software\Microsoft\PowerShell\1\ShellIds\Microsoft.PowerShell
//     (about_Execution_Policies), PowerShell 7's the
//     "Microsoft.PowerShell:ExecutionPolicy" key of
//     $PSHOME\powershell.config.json (about_PowerShell_Config), which
//     PowerShell 7's own install writes as RemoteSigned.
//
// With no scope set, Windows PowerShell's policy is Restricted on a
// Windows client and RemoteSigned on Windows Server
// (about_Execution_Policies), and PowerShell 7's is Restricted on either
// (SecuritySupport.cs). Every one of these is readable without
// privilege.
//
// This file is the reading's logic, which needs no Windows; the reader
// is read_windows.go.
package winexecpolicy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
)

// A Policy is an execution policy, as PowerShell names it.
type Policy string

// The policies a scope can set.
const (
	// Restricted runs no scripts.
	Restricted Policy = "Restricted"
	// AllSigned runs only scripts a trusted publisher signed, which the
	// runner's step scripts aren't.
	AllSigned Policy = "AllSigned"
	// RemoteSigned runs unsigned scripts written on the machine, as the
	// runner's step scripts are.
	RemoteSigned Policy = "RemoteSigned"
	// Unrestricted runs unsigned scripts, warning only of those from
	// outside the local intranet zone.
	Unrestricted Policy = "Unrestricted"
	// Bypass blocks nothing.
	Bypass Policy = "Bypass"
)

// ParsePolicy returns the policy a scope's name s sets, in any case, as
// PowerShell reads it. Any other name, Default and Undefined among them,
// is Restricted: PowerShell 7 parses it as its Default policy, which its
// SecurityManager.cs treats as Restricted.
func ParsePolicy(s string) Policy {
	for _, p := range []Policy{Restricted, AllSigned, RemoteSigned, Unrestricted, Bypass} {
		if strings.EqualFold(s, string(p)) {
			return p
		}
	}
	return Restricted
}

// Blocks reports whether p refuses the runner's step scripts, which are
// unsigned and written on the machine: Restricted and AllSigned do.
func (p Policy) Blocks() bool { return p == Restricted || p == AllSigned }

// A Scope is where the policy in effect came from.
type Scope string

// The scopes a reading names.
const (
	// MachinePolicy is Group Policy for the computer, or, for PowerShell
	// 7, the PowerShellPolicies of $PSHOME's configuration file.
	MachinePolicy Scope = "MachinePolicy"
	// LocalMachine is Set-ExecutionPolicy's machine-wide scope.
	LocalMachine Scope = "LocalMachine"
	// NoScope is that no scope Silkpool reads sets one, so the policy is
	// PowerShell's default.
	NoScope Scope = "none"
)

// A Shell is a PowerShell a job's step can run its script in, by the
// name a workflow's `shell:` gives it.
type Shell string

// The two PowerShells.
const (
	// WindowsPowerShell is powershell.exe, which every Windows has.
	WindowsPowerShell Shell = "powershell"
	// PowerShell7 is pwsh.exe, the default shell of a job's step on
	// Windows when it's installed.
	PowerShell7 Shell = "pwsh"
)

// Name is the shell's name, as Microsoft writes it: "Windows PowerShell"
// or "PowerShell 7".
func (s Shell) Name() string {
	switch s {
	case WindowsPowerShell:
		return "Windows PowerShell"
	case PowerShell7:
		return "PowerShell 7"
	}
	return string(s)
}

// A Reading is the policy a shell's scripts meet, and its scope, or why
// it couldn't be read.
type Reading struct {
	Shell Shell
	// Installed reports that the shell is there. Windows PowerShell
	// always is; PowerShell 7 is when pwsh.exe is on the machine's PATH,
	// which is the PATH a job's steps get.
	Installed bool
	Policy    Policy
	Scope     Scope
	// Err says why the policy couldn't be read, when it couldn't.
	Err error
}

// Blocks reports whether the shell is installed, its policy was read,
// and it refuses the runner's step scripts.
func (r Reading) Blocks() bool { return r.Installed && r.Err == nil && r.Policy.Blocks() }

// ScriptExecution is Group Policy's "Turn on Script Execution" as
// PowerShell reads it, from the registry or from a configuration file's
// PowerShellPolicies.
type ScriptExecution struct {
	// EnableScripts is false for Restricted, true for ExecutionPolicy,
	// or nil when it isn't set.
	EnableScripts *bool
	// ExecutionPolicy is the policy's name, which counts only while
	// EnableScripts is true.
	ExecutionPolicy string
}

// Set reports whether either value is set. A ScriptExecution that's set
// is what PowerShell 7 takes for MachinePolicy, even when it sets no
// policy, as when ExecutionPolicy is set without EnableScripts.
func (s ScriptExecution) Set() bool { return s.EnableScripts != nil || s.ExecutionPolicy != "" }

// policy returns the name of the policy s sets, or "" when it sets none,
// as PowerShell's GetGroupPolicyValue does.
func (s ScriptExecution) policy() string {
	switch {
	case s.EnableScripts == nil:
		return ""
	case !*s.EnableScripts:
		return string(Restricted)
	}
	return s.ExecutionPolicy
}

// CoreMachinePolicy returns PowerShell 7's MachinePolicy, as its
// GetPolicySetting finds it: its own Group Policy key's, core, or
// Windows PowerShell's, windows, when core's
// UseWindowsPowerShellPolicySetting, useWindows, says so; and when the
// one it takes sets neither value, config, from the PowerShellPolicies
// of $PSHOME's powershell.config.json.
func CoreMachinePolicy(core ScriptExecution, useWindows bool, windows, config ScriptExecution) ScriptExecution {
	gp := core
	if useWindows {
		gp = windows
	}
	if gp.Set() {
		return gp
	}
	return config
}

// Settings are what a shell reads for its policy, scope by scope.
type Settings struct {
	MachinePolicy ScriptExecution
	// LocalMachine is the policy's name, or "" when it isn't set.
	LocalMachine string
	// Default is the policy when no scope sets one.
	Default Policy
}

// Effective returns the policy in effect, and the scope it came from:
// the first of MachinePolicy and LocalMachine that sets one, or
// Default.
func (s Settings) Effective() (Policy, Scope) {
	if name := s.MachinePolicy.policy(); name != "" {
		return ParsePolicy(name), MachinePolicy
	}
	if s.LocalMachine != "" {
		return ParsePolicy(s.LocalMachine), LocalMachine
	}
	return s.Default, NoScope
}

// SetsPolicy reports whether s sets a policy, so that no lower scope
// counts.
func (s ScriptExecution) SetsPolicy() bool { return s.policy() != "" }

// Config is what PowerShell 7's powershell.config.json holds of the
// policy. A value of the wrong type is an error only for its own scope,
// which a higher scope's policy makes moot.
type Config struct {
	// ExecutionPolicy is its LocalMachine policy, when it's $PSHOME's,
	// or LocalMachineErr says why it can't be read.
	ExecutionPolicy string
	LocalMachineErr error
	// Policies is its PowerShellPolicies' ScriptExecution, or
	// PoliciesErr says why it can't be read.
	Policies    ScriptExecution
	PoliciesErr error
}

// configFile is the file's JSON. encoding/json matches its keys in any
// case, as PowerShell does.
type configFile struct {
	ExecutionPolicy json.RawMessage `json:"Microsoft.PowerShell:ExecutionPolicy"`
	Policies        json.RawMessage `json:"PowerShellPolicies"`
}

// ParseConfig reads a powershell.config.json, which may begin with a
// byte order mark, UTF-8's or UTF-16's, as PowerShell 7 reads it. A
// file that isn't JSON is an error, as it stops PowerShell 7 from
// starting.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	var f configFile
	if err := json.Unmarshal(decode(data), &f); err != nil {
		return c, fmt.Errorf("reading powershell.config.json: %w", err)
	}
	if len(f.ExecutionPolicy) > 0 && string(f.ExecutionPolicy) != "null" {
		if err := json.Unmarshal(f.ExecutionPolicy, &c.ExecutionPolicy); err != nil {
			c.LocalMachineErr = fmt.Errorf("reading powershell.config.json's execution policy: %w", err)
		}
	}
	if len(f.Policies) > 0 {
		var s struct {
			ScriptExecution struct {
				ExecutionPolicy *string
				EnableScripts   *bool
			}
		}
		if err := json.Unmarshal(f.Policies, &s); err != nil {
			c.PoliciesErr = fmt.Errorf("reading powershell.config.json's PowerShellPolicies: %w", err)
		} else {
			c.Policies.EnableScripts = s.ScriptExecution.EnableScripts
			if p := s.ScriptExecution.ExecutionPolicy; p != nil {
				c.Policies.ExecutionPolicy = *p
			}
		}
	}
	return c, nil
}

// decode returns data as UTF-8, past a byte order mark: UTF-8's, or
// UTF-16's, which Windows PowerShell's Out-File writes.
func decode(data []byte) []byte {
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(data, []byte("\xEF\xBB\xBF")):
		return data[3:]
	case bytes.HasPrefix(data, []byte("\xFF\xFE")):
		order = binary.LittleEndian
	case bytes.HasPrefix(data, []byte("\xFE\xFF")):
		order = binary.BigEndian
	default:
		return data
	}
	units := make([]uint16, (len(data)-2)/2)
	for i := range units {
		units[i] = order.Uint16(data[2+2*i:])
	}
	return []byte(string(utf16.Decode(units)))
}

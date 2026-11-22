package wording

import (
	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/winexecpolicy"
)

// The words for a Windows node's PowerShell execution policy, which
// fails a job's PowerShell steps while it refuses unsigned local
// scripts. The runner runs each such step as a .ps1 file. It blocks no
// other job, so it isn't a Degraded reason: enroll names it, and the
// diagnostics report shows it.

// ExecutionPolicyPage is the operator page on the execution policy, in
// Silkpool's public docs.
const ExecutionPolicyPage = "https://github.com/meschoening/silkpool/blob/main/docs/windows-execution-policy.md"

// ExecutionPolicy is the line that follows an enroll on a Windows
// machine where an installed PowerShell's policy blocks the runner's step
// scripts, or "" where none does: the policy, where it came from, which
// steps fail, and the operator page. A policy that couldn't be read says
// nothing.
func ExecutionPolicy(readings []winexecpolicy.Reading) string {
	var windows, core winexecpolicy.Reading
	for _, r := range readings {
		switch r.Shell {
		case winexecpolicy.WindowsPowerShell:
			windows = r
		case winexecpolicy.PowerShell7:
			core = r
		}
	}
	var subject, steps string
	switch {
	case windows.Blocks() && core.Blocks() && windows.Policy == core.Policy && windows.Scope == core.Scope:
		subject = "Windows PowerShell's and PowerShell 7's execution policy here is " + policyFrom(windows)
	case windows.Blocks() && core.Blocks():
		subject = "Windows PowerShell's execution policy here is " + policyFrom(windows) + " and PowerShell 7's is " + policyFrom(core)
	case windows.Blocks():
		subject = "Windows PowerShell's execution policy here is " + policyFrom(windows)
	case core.Blocks():
		subject = "PowerShell 7's execution policy here is " + policyFrom(core)
	default:
		return ""
	}
	// The runner's default shell is PowerShell 7, or Windows PowerShell
	// without it. A step whose own shell sets the policy, as the operator
	// page's does, doesn't fail, unless MachinePolicy or UserPolicy sets
	// it.
	switch {
	case windows.Blocks() && core.Blocks():
		steps = "a job's steps with `shell: powershell` or `shell: pwsh` will fail, and so will steps that use the runner's default shell"
	case windows.Blocks() && !core.Installed && core.Err == nil:
		steps = "a job's steps with `shell: powershell` will fail, and so will steps that use the runner's default shell, which is Windows PowerShell without PowerShell 7"
	case windows.Blocks():
		steps = "a job's steps with `shell: powershell` will fail"
	default:
		steps = "a job's steps with `shell: pwsh` will fail, and so will steps that use the runner's default shell, which is PowerShell 7"
	}
	return subject + ", so " + steps + ". " + ExecutionPolicyPage + " says how to let them run."
}

// policyFrom is a reading's policy and the scope it came from, as
// Get-ExecutionPolicy -List names it: "Restricted (MachinePolicy)", or
// "Restricted (PowerShell's default)".
func policyFrom(r winexecpolicy.Reading) string {
	if r.Scope == winexecpolicy.NoScope {
		return string(r.Policy) + " (PowerShell's default)"
	}
	return string(r.Policy) + " (" + string(r.Scope) + ")"
}

// powerShell is the diagnostics report's line for each PowerShell: its
// policy, where it came from, and whether it blocks a job's steps.
func powerShell(shells []diagnostics.PowerShell) []string {
	var lines []string
	for _, p := range shells {
		line := winexecpolicy.Shell(p.Shell).Name() + ": "
		switch {
		case p.Error != "":
			line += "couldn't be read: " + p.Error
		case !p.Installed:
			line += "not installed"
		default:
			line += p.Policy
			if winexecpolicy.Scope(p.Scope) == winexecpolicy.NoScope {
				line += ", PowerShell's default, since no scope sets one"
			} else {
				line += ", from " + p.Scope
			}
			if p.Blocks {
				line += " · blocks the scripts of a job's steps in it"
			}
		}
		lines = append(lines, line)
	}
	return lines
}

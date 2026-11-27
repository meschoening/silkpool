package wording

// The node CLI's words for enrolling: the commands that need elevation,
// as each OS runs them, and an enroll whose answer was lost, from "Retry
// a lost enrollment answer once, then tell the operator".

// An Elevation is how a node's OS runs the CLI's commands that need
// elevation, enroll, remove and set-hub-url: with sudo on Linux and
// macOS, and in PowerShell, run as administrator, on Windows, which has
// no sudo out of the box, as the Hub's Add node page gives them (#365).
type Elevation struct {
	// Windows reports that the CLI runs on Windows.
	Windows bool
}

// Command is the CLI's command that needs elevation, as an
// administrator types it: "sudo silkpool enroll <code>", or on Windows
// "silkpool enroll <code>".
func (e Elevation) Command(args string) string {
	if e.Windows {
		return "silkpool " + args
	}
	return "sudo silkpool " + args
}

// Needs follows a command that needs elevation in the CLI's usage.
func (e Elevation) Needs() string {
	if e.Windows {
		return "Needs a PowerShell run as administrator."
	}
	return "Needs sudo."
}

// RunIt ends a refusal of a command from a caller that isn't elevated:
// "enroll changes this machine's accounts, so run it <RunIt>."
func (e Elevation) RunIt() string {
	if e.Windows {
		return "from an administrator PowerShell"
	}
	return "with sudo"
}

// Where says where to run an unenrolled machine's enroll command, as the
// tray's panel says it (TrayNotEnrolled), or is empty with sudo, which
// runs in any shell.
func (e Elevation) Where() string {
	if e.Windows {
		return where["windows"]
	}
	return ""
}

// EnrollAnswerLost follows an enroll that failed because the Hub's
// answer was lost after the enroll went out, and again on the agent's
// one retry. The Hub may have enrolled the machine, but a rerun with the
// same code reuses the pending node key, so the Hub answers it as the
// node it enrolled, as "Make enrollment safe to retry, check node names
// and prune old codes" decided.
func (e Elevation) EnrollAnswerLost() string {
	again := "again with the same code"
	if e.Windows {
		again += ", from a PowerShell run as administrator"
	}
	return "Run `" + e.Command("enroll <code>") + "` " + again + ". That's safe: " +
		"the machine joins as one node, whether or not the Hub enrolled it already."
}

// SmartAppControl follows an enroll on a Windows machine whose Smart App
// Control is on, or in evaluation, which may turn it on. It blocks
// GitHub's unsigned runner application, so the node runs no jobs while
// it's on, and shows it as its Degraded reason, node.SmartAppControlOn,
// whose title begins this line, as "Define how each binary is packaged,
// installed, upgraded and signed per OS" decided (ADR 0013).
const SmartAppControl = "Smart App Control is on, and it blocks GitHub's runner application. " + turnOffSmartAppControl

// turnOffSmartAppControl is where Smart App Control is turned off.
const turnOffSmartAppControl = "Turn it off in Windows Security ▸ App & browser control."

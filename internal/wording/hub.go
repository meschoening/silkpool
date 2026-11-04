package wording

import (
	"fmt"
	"strings"
	"time"
)

// The Hub's words, which the Hub's CLI prints and the dashboard reuses:
// codes and the commands that enroll with them, removing a node, the
// listener and setup, and changing the Hub's URL. They're from "Name the
// CLI and define its commands"
// and "Prototype the Hub dashboard".

// A CodeState is an enrollment code's state, as the Hub names it.
type CodeState string

// The states of a code.
const (
	CodeOpen    CodeState = "open"
	CodeUsed    CodeState = "used"
	CodeRevoked CodeState = "revoked"
	CodeExpired CodeState = "expired"
)

// Code is an enrollment code's state in words: "Open · expires tomorrow
// 4:54 PM", "Used by build-pc", "Revoked" or "Expired".
func (c Clock) Code(state CodeState, expires time.Time, node string) string {
	switch state {
	case CodeOpen:
		return "Open · expires " + c.When(expires)
	case CodeUsed:
		return "Used by " + node
	case CodeRevoked:
		return "Revoked"
	}
	return "Expired"
}

// An EnrollCommand is the command that enrolls a machine with a code, on
// some OSes, and where to run it.
type EnrollCommand struct {
	OSes, Where, Command string
}

// EnrollCommands are the commands that enroll a machine with code, as
// Add node shows them: with sudo on Linux and macOS, and in an
// administrator PowerShell on Windows.
func EnrollCommands(code string) []EnrollCommand {
	return []EnrollCommand{
		{OSes: "Linux and macOS", Where: "in a shell", Command: "sudo silkpool enroll " + code},
		{OSes: "Windows", Where: "in PowerShell, run as administrator", Command: "silkpool enroll " + code},
	}
}

// EnrollNote follows the enroll commands.
const EnrollNote = "It joins under its hostname. Add --name to choose another. It joins paused, so nothing runs until you resume it."

// HubRemoveConfirm is the Hub's confirmation of removing the node name:
// a connected node finishes its jobs first, as the node's own removal
// says (View.RemoveConfirm), and one that isn't connected is removed at
// once, which can't stop a job its last report showed running.
func HubRemoveConfirm(name string, connected bool, jobs []string) string {
	if connected {
		first := fmt.Sprintf("%s has no job running.", name)
		if len(jobs) > 0 {
			first = fmt.Sprintf("%s finishes %s first; removing never cancels a job.", name, list(jobs))
		}
		return first + " Then its slot accounts are deleted and it leaves the Hub for good. Enrolling it again needs a new code."
	}
	s := fmt.Sprintf("%s isn't connected, so it's removed from the Hub now and its key is revoked.", name)
	if len(jobs) > 0 {
		s += fmt.Sprintf(" Its last report showed %s running. A job already running there can't be stopped from here.", list(jobs))
	}
	return s + " The machine cleans itself up the next time it connects."
}

// RemoveNodeQuestion asks for the confirmation of removing a node.
func RemoveNodeQuestion(name string) string { return fmt.Sprintf("Remove %s from the Hub?", name) }

// Removing starts following a removal.
func Removing(name string) string {
	return fmt.Sprintf("Removing %s. Ctrl+C stops watching; the removal carries on.", name)
}

// RemovalStep is how far a connected node's removal has gone, from its
// state and the jobs it holds: finishing them, then cleaning up.
func RemovalStep(state string, jobs []string) string {
	if len(jobs) > 0 {
		return fmt.Sprintf("Finishing %s, then it leaves the Hub.", plural(len(jobs), "job"))
	}
	if state == "Drained" {
		return "Cleaning up its slots, then it leaves the Hub."
	}
	return "Retiring its runners, then it leaves the Hub."
}

// Removed ends a removal: the node is a tombstone.
func Removed(name string) string {
	return fmt.Sprintf("%s has left the Hub. Its history is kept, and enrolling it again needs a new code.", name)
}

// StoppedWatching is what Ctrl+C prints while a removal is followed.
const StoppedWatching = "Stopped watching. The removal carries on."

// Listener is the listener's mode in words: "self-signed certificate",
// "certificate files" or "behind a proxy".
func Listener(mode string) string {
	switch mode {
	case "self-signed":
		return "self-signed certificate"
	case "certificate-files":
		return "certificate files"
	case "behind-proxy":
		return "behind a proxy, over plain HTTP"
	}
	return mode
}

// SetupSteps are the titles of setup's steps, by the step's name.
var SetupSteps = map[string]string{
	"checks":     "Check the Hub",
	"account":    "Choose the GitHub account",
	"create-app": "Create the GitHub App",
	"sign-in":    "Sign in with GitHub",
	"install":    "Install the App",
	"enable":     "Add a target",
	"done":       "Done",
}

// Setup is how far first-run setup has gone, from the latest step done,
// the count done and the count of steps, and the Hub's URL, where it
// carries on.
func Setup(step string, done, of int, url string) string {
	switch {
	case step == "done":
		return "Done."
	case step == "":
		return "Not started. Open the setup link silkpool-hub init printed, or make a new one with silkpool-hub setup-code."
	}
	return fmt.Sprintf("%d of %d steps done, the last %q. Carry on at %s.", done, of, SetupSteps[step], url)
}

// SetupValid says how long a setup link works.
func (c Clock) SetupValid(expires time.Time, reassign bool) string {
	if reassign {
		return fmt.Sprintf("The next GitHub sign-in through this link becomes the Hub's operator. It works until then, or for 24 h, until %s.", c.When(expires))
	}
	return fmt.Sprintf("It works until you sign in, or for 24 h, until %s.", c.When(expires))
}

// SetHubURLCommand is the command that points a node at the Hub's new
// URL, and where to run it, on an OS as runner labels name it.
func SetHubURLCommand(os, url string) EnrollCommand {
	if os == "windows" {
		return EnrollCommand{OSes: "Windows", Where: "in PowerShell, run as administrator", Command: "silkpool set-hub-url " + url}
	}
	return EnrollCommand{OSes: "Linux and macOS", Where: "in a shell", Command: "sudo silkpool set-hub-url " + url}
}

// SetHubURLUnknown says to point the nodes named, whose platform is
// unknown, at the Hub's new URL with the command for the OS each runs.
func SetHubURLUnknown(names []string) string {
	if len(names) == 1 {
		return names[0] + "'s platform is unknown: run the command for its OS there."
	}
	return "The platforms of " + list(names) + " are unknown: run the command for each one's OS there."
}

// SignInFails says sign-in fails until the sign-in App's settings
// follow the Hub's new URL.
const SignInFails = "Sign-in to the dashboard fails until the sign-in App's settings are changed."

// SelfHosted warns of the self-hosted custom label.
func SelfHosted(name string) string {
	return fmt.Sprintf("With self-hosted, generic self-hosted jobs can reach %s too.", name)
}

// Platform is an OS and architecture, as runner labels name them, in
// words: "Windows · x64".
func Platform(os, arch string) string {
	name, ok := map[string]string{"linux": "Linux", "windows": "Windows", "macos": "macOS"}[os]
	if !ok {
		return ""
	}
	return name + " · " + arch
}

// UnknownPlatform is the platform of a node whose agent stated one
// Silkpool doesn't support, in place of whatever it stated.
const UnknownPlatform = "Unknown platform"

// Nodes names nodes in a sentence: "build-pc", "build-pc and build-mac".
func Nodes(names []string) string { return list(names) }

// Quote writes a target as a shell takes it: quoted when it has a space.
func Quote(target string) string {
	if strings.ContainsAny(target, " \t") {
		return `"` + target + `"`
	}
	return target
}

// CandidateRefused is why a target GitHub offers can't be one, by the
// refusal's code, as target available and Add a target show it.
func CandidateRefused(code string) string {
	switch code {
	case "public":
		return "Public: Silkpool never serves public repositories."
	case "public-group":
		return "Allows public repositories, so it can't be a target."
	}
	return ""
}

// Serves is what an organization target's runner group serves: all of
// its repositories, its private ones, or the ones it's given.
func Serves(org, visibility string, repos []string) string {
	switch visibility {
	case "all":
		return fmt.Sprintf("all of %s's repositories", org)
	case "private":
		return fmt.Sprintf("%s's private repositories", org)
	}
	if len(repos) == 0 {
		return "no repository yet"
	}
	return strings.Join(repos, ", ")
}

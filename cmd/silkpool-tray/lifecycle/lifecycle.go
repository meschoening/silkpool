// Package lifecycle is when the tray app quits or relaunches itself,
// from what it reads of the agent, as "Define how each binary is
// packaged, installed, upgraded and signed per OS" has it:
//
//   - started at sign-in in an account that isn't an administrator, it
//     exits at once and shows nothing; opened by hand there, it shows
//     the "needs an administrator account" state;
//   - when the agent's version changes, it relaunches itself from the new
//     binary.
//
// The sign-in entries, the Run entry on Windows and the LaunchAgent on
// macOS, start it with --sign-in, SignInFlag. It exits 0 when it quits, so the
// LaunchAgent, which starts it again only after a failure, leaves it be.
//
// It doesn't import Wails, so its tests run in the default lane.
package lifecycle

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/meschoening/silkpool/internal/cli"
	"github.com/meschoening/silkpool/internal/control"
)

// SignInFlag is the flag the sign-in entries start the app with, by its
// name.
const SignInFlag = "sign-in"

// An Action is what the app does on a reading, beyond showing it.
type Action int

const (
	// Show shows the reading.
	Show Action = iota
	// Quit exits at once, showing nothing.
	Quit
	// Relaunch starts the app again from its binary, which an upgrade
	// has replaced, and exits.
	Relaunch
)

// A Lifecycle decides, from each reading in turn, whether the app shows
// it, quits or relaunches.
type Lifecycle struct {
	// SignIn reports that the app was started at sign-in, not opened by
	// hand.
	SignIn bool
	// version is the agent's version as last read.
	version string
}

// Next returns what the app does on r. It quits if it was started at
// sign-in and the agent refused the account as no administrator's. It
// relaunches once the agent's version is another than it read before,
// as after an upgrade restarts the agent: the installer has replaced the
// app's binary too, since every binary in a release carries the same
// version. A version it reads at first, even another than the app's, is
// no change: the binary on disk can't be newer than the agent it came
// with, so relaunching from it would change nothing, over and over.
func (l *Lifecycle) Next(r cli.Reading) Action {
	if l.SignIn && errors.As(r.Err, new(control.NotAdmin)) {
		return Quit
	}
	if r.Version == "" {
		return Show
	}
	before := l.version
	l.version = r.Version
	if before != "" && before != r.Version {
		return Relaunch
	}
	return Show
}

// Command is the command that starts the app again from exe, the binary
// it was started from, with args, on goos. An upgrade replaces the
// binary at that path: on Windows the installer renames the running exe
// aside and puts the new one there. On macOS a binary inside an app
// bundle is started through open, as its own app, so it isn't part of
// the old app's launchd job, whose processes launchd ends with it.
func Command(goos, exe string, args []string) *exec.Cmd {
	if goos == "darwin" {
		if bundle, ok := appBundle(exe); ok {
			open := []string{"-n", bundle}
			if len(args) > 0 {
				open = append(append(open, "--args"), args...)
			}
			return exec.Command("/usr/bin/open", open...)
		}
	}
	return exec.Command(exe, args...)
}

// appBundle returns the app bundle exe is the binary of, as in
// /Applications/Silkpool.app/Contents/MacOS/Silkpool, if it's in one.
func appBundle(exe string) (string, bool) {
	macOS := filepath.Dir(exe)
	contents := filepath.Dir(macOS)
	bundle := filepath.Dir(contents)
	if filepath.Base(macOS) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(bundle, ".app") {
		return "", false
	}
	return bundle, true
}

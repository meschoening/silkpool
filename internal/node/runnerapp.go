package node

import (
	"fmt"
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/runnerapp"
)

// The runner application's rules on the node are in "Define how the
// agent installs, verifies and updates the runner application", recorded as
// ADR 0011. The Hub picks each target's version, and the node fetches,
// verifies and installs it itself, then rolls its idle runners over.

// FirstInstallRetry and MaxInstallRetry bound the wait between attempts
// to install the runner application, which doubles from FirstInstallRetry
// up to MaxInstallRetry. Each attempt can download up to 230 MB.
const (
	FirstInstallRetry = time.Minute
	MaxInstallRetry   = time.Hour
)

// ExitVersionRefused is the listener's exit code when GitHub refuses its
// runner application's version. The agent starts every listener with
// ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1, which every
// version GitHub still accepts understands, so the refusal isn't a bare
// exit 1.
const ExitVersionRefused = 7

// A RunnerApp is a release of the runner application, such as 2.338.2,
// with the SHA-256 of its package for the node's platform, in lower-case
// hex. The zero RunnerApp is none.
type RunnerApp struct{ Version, SHA256 string }

// An Update is the node's install of the runner application its Hub
// wants. The zero Update is none under way.
type Update struct {
	// Version is the version being installed.
	Version string
	// Out reports that an InstallRunnerApp for Version is out.
	Out bool
	// Attempts counts the failed attempts at Version, and RetryAt is when
	// to try again.
	Attempts int
	RetryAt  time.Time
	// Failed is why the latest attempt failed, or empty.
	Failed string
}

// installBackoff is the wait after the attempts-th failed install.
func installBackoff(attempts int) time.Duration {
	d := FirstInstallRetry
	for i := 1; i < attempts && d < MaxInstallRetry; i++ {
		d *= 2
	}
	return min(d, MaxInstallRetry)
}

// outdated reports whether r started from an install other than the one
// new runners start from.
func (n Node) outdated(r Runner) bool { return n.Installed != "" && r.Version != n.Installed }

// installs returns the versions of the installs n uses: the one new
// runners start from, and each hosted runner's.
func (n Node) installs() []string {
	var versions []string
	if n.Installed != "" {
		versions = append(versions, n.Installed)
	}
	for _, s := range n.Slots {
		if v := s.Runner.Version; s.Phase == Hosting && v != "" && !slices.Contains(versions, v) {
			versions = append(versions, v)
		}
	}
	return versions
}

// wanted records the runner application the Hub wants. A new version
// starts its own attempts, at once.
func (a *applying) wanted(app RunnerApp) {
	n := a.n
	n.RunnerApp = app
	if app.Version != n.Update.Version && !n.Update.Out {
		n.Update = Update{}
	}
}

// installReady records that version's verified install is ready. Unless
// the Hub has since moved on to another version, new runners start from
// it, and settling retires the idle runners on any other.
func (a *applying) installReady(version string) {
	n := a.n
	if n.Update.Version == version {
		n.Update.Out = false
	}
	if version != n.RunnerApp.Version {
		if n.Update.Version == version {
			n.Update = Update{}
		}
		n.Discarded = n.Installed == ""
		return
	}
	n.Installed, n.Discarded = version, false
	if !n.Update.Out {
		n.Update = Update{}
	}
}

// foundInstalls makes the newest of the installs the agent found when it
// started the one new runners start from. An install request that was
// out died with the agent, and so did every preparation.
func (a *applying) foundInstalls(installs []string) {
	n := a.n
	n.Installed, n.Discarded, n.Update.Out = "", false, false
	for _, v := range installs {
		if n.Installed == "" || runnerapp.Compare(v, n.Installed) > 0 {
			n.Installed = v
		}
	}
}

// installFailed records a failed attempt to install version.
func (a *applying) installFailed(version, reason string) {
	u := &a.n.Update
	if u.Version != version || !u.Out {
		return
	}
	u.Out = false
	u.Attempts++
	u.Failed, u.RetryAt = reason, a.now.Add(installBackoff(u.Attempts))
	if version != a.n.RunnerApp.Version {
		// The Hub has moved on: its version starts afresh.
		*u = Update{}
	}
}

// install deletes the installs that the node used before this Apply, or
// that e reported, and doesn't use now, then asks for the install of the
// wanted version if it's due. Of the ones it used, it deletes none while
// it doesn't know the install new runners start from, as when the agent
// applies a dead runner's exit before Started. Wipe deletes every
// install. One install is asked for at a time, and none while the node
// is being removed.
func (a *applying) install(before []string, e Event) {
	n := a.n
	var reported []string
	switch e := e.(type) {
	case InstallReady:
		reported = []string{e.Version}
	case Started:
		reported = e.Installs
	}
	now := n.installs()
	for _, v := range before {
		if n.Installed != "" && !slices.Contains(now, v) {
			a.emit(DeleteInstall{Version: v})
		}
	}
	for _, v := range reported {
		if !slices.Contains(before, v) && !slices.Contains(now, v) {
			a.emit(DeleteInstall{Version: v})
		}
	}
	want, u := n.RunnerApp, &n.Update
	if want.Version == "" || want.Version == n.Installed || u.Out || n.Removal.Started || a.now.Before(u.RetryAt) {
		return
	}
	if u.Version != want.Version {
		*u = Update{}
	}
	u.Version, u.Out = want.Version, true
	a.emit(InstallRunnerApp(want))
}

// refused records that GitHub refused the version of s's runner, which
// took no job: the Hub hears of it through the outbox, and the slot
// fails with RunnerAppUnavailable, as a listener that never came online
// would have failed. A late listener already counted its attempt.
func (a *applying) refused(s *Slot) {
	r := s.Runner
	a.happened(RunnerVersionRefused{Slot: s.Number, Runner: r.Name, Version: r.Version})
	detail := fmt.Sprintf("GitHub no longer accepts runner %s", r.Version)
	switch {
	case r.Retirement != NotRetiring:
	case r.Late:
		s.Failure.Reason, s.Failure.Detail = RunnerAppUnavailable, detail
	default:
		a.fail(s, RunnerAppUnavailable, detail)
	}
}

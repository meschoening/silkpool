package runnerapp

import (
	"maps"
	"slices"
	"time"
)

// CheckEvery is how often the Hub checks every target's served version.
const CheckEvery = time.Hour

// Targets are the runner versions of the Hub's targets, and the checks
// under way. The Hub stores each target's Version, which must survive
// restarts because it's never lowered; the rest lasts until the Hub
// restarts.
type Targets struct {
	// Targets holds each target, as the Hub keys it.
	Targets map[string]Target
	// CheckAt is when the next hourly check is due, or zero before the
	// Hub has started.
	CheckAt time.Time
	// Fetching reports that a fetch of the latest release is out.
	Fetching bool
}

// A Target is one target's runner version.
type Target struct {
	// Version is the version the target's nodes run, or zero until the
	// Hub has found one. It's never lowered.
	Version Version
	// Checking reports that a check of the target's downloads endpoint
	// is out.
	Checking bool
	// Latest reports that the target waits on the latest release:
	// GitHub served it no version, or refused its version.
	Latest bool
}

// An Event is something the Hub learns about runner versions: that it
// has started, that the clock moved on, a change to its targets, a
// node's refusal entry, or GitHub's answer to a check. Events about a
// target the Hub doesn't hold are ignored.
type Event interface{ event() }

// Started tells the Hub it has just started. Whatever was under way is
// forgotten, and every target is checked.
type Started struct{}

// Tick tells the Hub the clock reads now. It applies one at every
// WakeAt.
type Tick struct{}

// Added tells the Hub the operator added a target, which is checked at
// once.
type Added struct{ Target string }

// Dropped tells the Hub a target is gone, and its version with it.
type Dropped struct{ Target string }

// Refused relays a node's refusal entry: GitHub refused Version on
// Target, so a listener on it exited with code 7. A refusal of the
// target's version asks for an immediate check and for the latest
// release, which the target moves to if it's newer: that covers a
// critical security cut-off that lands before the downloads endpoint
// catches up. A refusal of any other version only asks for the check.
type Refused struct{ Target, Version string }

// Served brings the downloads endpoint's answer for Target. An answer
// that doesn't serve one version for every platform Silkpool supports,
// such as the empty list GitHub gives a repository that has never run a
// workflow, makes the target wait on the latest release instead.
type Served struct {
	Target    string
	Downloads []Download
}

// CheckFailed reports that the downloads endpoint couldn't be read for
// Target. Its version stays, until the next hourly check.
type CheckFailed struct{ Target string }

// Latest brings actions/runner's latest release, which each target
// waiting on it moves to if it's newer.
type Latest struct{ Release Release }

// LatestFailed reports that the latest release couldn't be fetched. The
// targets waiting on it stop waiting, until a check or a refusal asks
// again.
type LatestFailed struct{}

func (Started) event()      {}
func (Tick) event()         {}
func (Added) event()        {}
func (Dropped) event()      {}
func (Refused) event()      {}
func (Served) event()       {}
func (CheckFailed) event()  {}
func (Latest) event()       {}
func (LatestFailed) event() {}

// An Effect is something Apply asks of the Hub, beyond saving the
// Targets it returns, which comes first.
type Effect interface{ effect() }

// CheckDownloads asks the Hub to read Target's downloads endpoint, and
// to apply Served or CheckFailed.
type CheckDownloads struct{ Target string }

// FetchLatest asks the Hub to fetch actions/runner's latest release,
// and to apply Latest or LatestFailed.
type FetchLatest struct{}

// Changed reports that Target's version is now Version. The Hub sends
// it to the target's connected nodes in their Hub settings, each with
// its own platform's SHA-256.
type Changed struct {
	Target  string
	Version Version
}

// WakeAt asks the Hub to apply a Tick at At, replacing any earlier
// WakeAt.
type WakeAt struct{ At time.Time }

func (CheckDownloads) effect() {}
func (FetchLatest) effect()    {}
func (Changed) effect()        {}
func (WakeAt) effect()         {}

// Apply applies e to v at now. It returns the Targets that result,
// which the Hub saves first, and the effects it must then carry out, in
// order, ending with a WakeAt once the Hub has started. Targets go in
// the order of their keys.
func (v Targets) Apply(e Event, now time.Time) (Targets, []Effect) {
	v.Targets = maps.Clone(v.Targets)
	if v.Targets == nil {
		v.Targets = map[string]Target{}
	}
	a := &applying{v: &v}
	switch e := e.(type) {
	case Started:
		v.Fetching = false
		for _, key := range a.keys() {
			v.Targets[key] = Target{Version: v.Targets[key].Version}
		}
		a.checkAll()
		v.CheckAt = now.Add(CheckEvery)
	case Tick:
		if !v.CheckAt.IsZero() && !now.Before(v.CheckAt) {
			a.checkAll()
			v.CheckAt = now.Add(CheckEvery)
		}
	case Added:
		if _, ok := v.Targets[e.Target]; !ok {
			v.Targets[e.Target] = Target{}
			a.check(e.Target)
		}
	case Dropped:
		delete(v.Targets, e.Target)
	case Refused:
		if t, ok := v.Targets[e.Target]; ok {
			a.check(e.Target)
			if e.Version == t.Version.Version {
				a.waitOnLatest(e.Target)
			}
		}
	case Served:
		if t, ok := v.Targets[e.Target]; ok {
			t.Checking = false
			v.Targets[e.Target] = t
			if version, ok := served(e.Downloads); ok {
				a.raise(e.Target, version)
			} else {
				a.waitOnLatest(e.Target)
			}
		}
	case CheckFailed:
		if t, ok := v.Targets[e.Target]; ok {
			t.Checking = false
			v.Targets[e.Target] = t
		}
	case Latest:
		v.Fetching = false
		version, ok := latest(e.Release)
		for _, key := range a.keys() {
			if t := v.Targets[key]; t.Latest {
				t.Latest = false
				v.Targets[key] = t
				if ok {
					a.raise(key, version)
				}
			}
		}
	case LatestFailed:
		v.Fetching = false
		for _, key := range a.keys() {
			t := v.Targets[key]
			t.Latest = false
			v.Targets[key] = t
		}
	}
	if !v.CheckAt.IsZero() {
		a.emit(WakeAt{At: v.CheckAt})
	}
	return v, a.effects
}

// applying is one Apply under way.
type applying struct {
	v       *Targets
	effects []Effect
}

func (a *applying) emit(e Effect) { a.effects = append(a.effects, e) }

// keys returns the targets' keys, in order.
func (a *applying) keys() []string { return slices.Sorted(maps.Keys(a.v.Targets)) }

// checkAll checks every target.
func (a *applying) checkAll() {
	for _, key := range a.keys() {
		a.check(key)
	}
}

// check reads target's downloads endpoint, unless a read is already out.
func (a *applying) check(target string) {
	t := a.v.Targets[target]
	if t.Checking {
		return
	}
	t.Checking = true
	a.v.Targets[target] = t
	a.emit(CheckDownloads{Target: target})
}

// waitOnLatest makes target wait on the latest release, and fetches it
// unless a fetch is already out.
func (a *applying) waitOnLatest(target string) {
	t := a.v.Targets[target]
	t.Latest = true
	a.v.Targets[target] = t
	if !a.v.Fetching {
		a.v.Fetching = true
		a.emit(FetchLatest{})
	}
}

// raise moves target to version if it's higher than the target's own,
// or the target has none. A target's version is never lowered.
func (a *applying) raise(target string, version Version) {
	t := a.v.Targets[target]
	if t.Version.Version != "" && Compare(version.Version, t.Version.Version) <= 0 {
		return
	}
	t.Version = version
	a.v.Targets[target] = t
	a.emit(Changed{Target: target, Version: version})
}

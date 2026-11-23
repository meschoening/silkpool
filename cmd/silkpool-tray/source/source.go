// Package source is where the tray panel's state comes from: what the
// tray reads of the agent, its status as the stream `silkpool status
// --watch` reads gives it (ADR 0009), or why there's none. The app reads
// the agent through the CLI's control-channel client, cli.Link. A Fake
// gives it from fixture states instead, which the panel's golden tests
// render, and which `silkpool-tray --fixture <name>` shows to check the
// panel by hand.
//
// It doesn't import Wails, so its tests run in the default lane.
package source

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/cli"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/version"
	"github.com/meschoening/silkpool/internal/wire"
)

// A Source gives what the tray reads of the agent, as cli.Link does.
type Source interface {
	// Watch calls f with what it reads now, then again after every
	// change, until ctx is done.
	Watch(ctx context.Context, f func(cli.Reading))
}

// The link is a Source.
var _ Source = (*cli.Link)(nil)

// ErrFixture refuses a command to a Fake, whose fixtures take none.
var ErrFixture = errors.New("the fixtures take no commands")

// A Fake is a Source that gives one of the Fixtures at a time, as its
// agent would report it, until Show picks another. A fixture's times are
// set from when it was picked, so its jobs' elapsed times run on from
// there. It takes no commands: each fails with ErrFixture.
type Fake struct {
	machine wire.Machine
	hub     string
	now     func() time.Time

	mu      sync.Mutex
	fixture Fixture
	at      time.Time
	// changed is closed, and replaced, when Show picks another fixture.
	changed chan struct{}
}

// NewFake returns a Fake on machine, enrolled in the Hub at hubURL,
// showing the fixture named first.
func NewFake(machine wire.Machine, hubURL, first string, now func() time.Time) (*Fake, error) {
	x, ok := Find(first)
	if !ok {
		return nil, fmt.Errorf("no fixture is named %q", first)
	}
	return &Fake{machine: machine, hub: hubURL, now: now, fixture: x, at: now(), changed: make(chan struct{})}, nil
}

// Show picks the fixture named name, from now.
func (f *Fake) Show(name string) error {
	x, ok := Find(name)
	if !ok {
		return fmt.Errorf("no fixture is named %q", name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fixture, f.at = x, f.now()
	close(f.changed)
	f.changed = make(chan struct{})
	return nil
}

// Showing is the name of the fixture the fake shows.
func (f *Fake) Showing() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fixture.Name
}

// Watch gives the fixture's status, as from an agent of the app's
// version, and again each time Show picks one, until ctx is done.
func (f *Fake) Watch(ctx context.Context, fn func(cli.Reading)) {
	for {
		f.mu.Lock()
		x, at, changed := f.fixture, f.at, f.changed
		f.mu.Unlock()
		st, err := x.Status(f.machine, f.hub, at, f.now())
		fn(cli.Reading{Status: st, Version: version.Version, Err: err})
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// Command fails with ErrFixture.
func (f *Fake) Command(context.Context, availability.Event) (control.Reply, error) {
	return control.Reply{}, ErrFixture
}

// Dismiss fails with ErrFixture.
func (f *Fake) Dismiss(context.Context, []int64) (control.Reply, error) {
	return control.Reply{}, ErrFixture
}

// Status is fixture x's status as the agent on machine, enrolled in the
// Hub at hubURL, would report it at now, with the fixture's times set
// from at. A node that isn't enrolled has no Hub, as the agent reports
// none before enrolling.
func (x Fixture) Status(machine wire.Machine, hubURL string, at, now time.Time) (control.Status, error) {
	zone, err := time.LoadLocation(machine.Zone)
	if err != nil {
		return control.Status{}, err
	}
	n, err := x.node(at.In(zone), zone)
	if err != nil {
		return control.Status{}, err
	}
	var hub control.Hub
	if n.Enrolled {
		since := at.Add(-time.Hour)
		if !n.Connected {
			since = at.Add(-12 * time.Minute)
		}
		hub = control.Hub{URL: hubURL, Node: "node-" + machine.Hostname, Since: store.Stamp(since)}
	}
	return control.StatusOf(version.Version, machine, store.Stamp(now), n, hub)
}

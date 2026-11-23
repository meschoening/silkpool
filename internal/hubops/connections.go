package hubops

import (
	"context"
	"slices"
	"strings"

	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
)

// Another GitHub connection, once setup is done, as "Define how an
// operator sets up a Hub and connects GitHub" has it: the signed-in
// operator runs setup's account, create-App, install and target steps
// from the dashboard's Settings, with no setup code. Sign-in stays on
// the first connection's App. The new connection's App is made by the
// same manifest as setup's, and is saved as soon as GitHub hands over
// its credentials, so its install can wait: Settings shows it as not
// installed until GitHub sends the browser back to InstalledPath, which
// Install takes as it does in setup.

// ConnectionCreatedPath is where GitHub sends the browser back after it
// creates a new connection's App, with the code AddConnection converts.
// Like AppCreatedPath, it's only the manifest's redirect URL.
const ConnectionCreatedPath = "/settings/app"

// ConnectionManifest returns the manifest of the App of a new GitHub
// connection, once setup is done: for the personal account, or for the
// organization org, with state. It returns the account too, which
// AddConnection checks GitHub's App against. An account the Hub already
// has a connection to is refused: a Hub holds one personal connection,
// and one per organization.
func (o *Ops) ConnectionManifest(ctx context.Context, kind hubstore.Kind, org, state string) (Manifest, hubstore.Account, error) {
	h, err := o.state(ctx)
	if err != nil {
		return Manifest{}, hubstore.Account{}, err
	}
	if h.Settings.Setup != hubstore.StepDone {
		return Manifest{}, hubstore.Account{}, errStep(h)
	}
	a, err := checkAccount(kind, org)
	if err != nil {
		return Manifest{}, hubstore.Account{}, err
	}
	if c, ok := connectionTo(h, a); ok {
		return Manifest{}, hubstore.Account{}, errConnected(c)
	}
	m, err := o.manifest(h.Settings.BaseURL, a, ConnectionCreatedPath, state)
	return m, a, err
}

// AddConnection converts the code GitHub sent the browser back with,
// after it created a new connection's App, into the App's credentials,
// which become files in the Hub's state folder, and saves the
// connection, not yet installed, from origin. History writes it. An App
// owned by another account than want, the one chosen, is refused, as is
// a personal App of a GitHub user other than the operator, and one owned
// by an account the Hub already has a connection to, which it would
// replace.
func (o *Ops) AddConnection(ctx context.Context, from node.Origin, want hubstore.Account, code string) (hubstore.Connection, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return hubstore.Connection{}, err
	}
	if h.Settings.Setup != hubstore.StepDone {
		return hubstore.Connection{}, errStep(h)
	}
	if want.Kind != hubstore.Personal && want.Kind != hubstore.Organization {
		return hubstore.Connection{}, refuse("invalid-account", "choose your personal account or an organization")
	}
	c, owner, err := o.createApp(ctx, code)
	if err != nil {
		return hubstore.Connection{}, err
	}
	if err := checkOwner(c, owner, want); err != nil {
		return hubstore.Connection{}, err
	}
	if want.Kind == hubstore.Personal && owner.ID != h.Settings.Operator {
		return hubstore.Connection{}, refuse("not-operator", "GitHub created the App %s for %s, who isn't this Hub's operator: delete it in its settings on GitHub, then sign in to GitHub as the operator and add it again", c.AppSlug, owner.Login)
	}
	if i := slices.IndexFunc(h.Connections, func(have hubstore.Connection) bool { return strings.EqualFold(have.Account, c.Account) }); i >= 0 {
		return hubstore.Connection{}, refuse("connected", "GitHub created the App %s for %s, which the Hub already has a connection to, through %s: delete %s in its settings on GitHub", c.AppSlug, c.Account, h.Connections[i].AppSlug, c.AppSlug)
	}
	return c, o.apply(ctx, hubstore.PutConnection{Connection: c, Origin: from})
}

// connectionTo returns the Hub's connection to the account a: its
// personal connection, whichever login it has, or its connection to the
// organization a names.
func connectionTo(h hubstore.Hub, a hubstore.Account) (hubstore.Connection, bool) {
	i := slices.IndexFunc(h.Connections, func(c hubstore.Connection) bool {
		return c.Kind == a.Kind && (a.Kind == hubstore.Personal || strings.EqualFold(c.Account, a.Login))
	})
	if i < 0 {
		return hubstore.Connection{}, false
	}
	return h.Connections[i], true
}

// errConnected refuses another connection to c's account.
func errConnected(c hubstore.Connection) *Refusal {
	if c.Kind == hubstore.Personal {
		return refuse("connected", "the Hub already has a personal connection, to %s: a Hub holds one, so choose an organization", c.Account)
	}
	return refuse("connected", "the Hub already has a connection to %s, through %s", c.Account, c.AppSlug)
}

// A KeepHistory is the history setting, "Keep history for N days":
// Days, or Forever. Changed reports a change that changed it.
type KeepHistory struct {
	Days    int  `json:"days,omitempty"`
	Forever bool `json:"forever,omitempty"`
	Changed bool `json:"changed,omitempty"`
}

// keepOf returns p's history setting.
func keepOf(p pool.Pool) KeepHistory {
	switch p.KeepHistory {
	case 0:
		return KeepHistory{Days: pool.DefaultKeepHistory}
	case pool.KeepForever:
		return KeepHistory{Forever: true}
	}
	return KeepHistory{Days: p.KeepHistory}
}

// KeepHistory returns the history setting.
func (o *Ops) KeepHistory(ctx context.Context) (KeepHistory, error) {
	h, err := o.state(ctx)
	if err != nil {
		return KeepHistory{}, err
	}
	return keepOf(h.Pool), nil
}

// SetKeepHistory keeps history for days, from origin: pool.MinKeepHistory
// or more, or pool.KeepForever. Fewer days are refused. A change writes
// its own history entry, and the next prune drops what's older.
func (o *Ops) SetKeepHistory(ctx context.Context, from node.Origin, days int) (KeepHistory, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	if err := pool.CheckKeepHistory(days); err != nil {
		return KeepHistory{}, refusalOf(err)
	}
	h, err := o.state(ctx)
	if err != nil {
		return KeepHistory{}, err
	}
	before := keepOf(h.Pool)
	if err := o.apply(ctx, pool.SetKeepHistory{Days: days, Origin: from}); err != nil {
		return KeepHistory{}, err
	}
	after := keepOf(pool.Pool{KeepHistory: days})
	after.Changed = after != before
	return after, nil
}

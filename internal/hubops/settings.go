package hubops

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
)

// The dashboard's paths that the Hub's GitHub Apps point at: the setup
// link, the sign-in callback, and where GitHub returns after the App is
// installed. Each App's settings name them, so set-url prints them.
const (
	SetupPath          = "/setup"
	SignInCallbackPath = "/sign-in/callback"
	InstalledPath      = "/setup/installed"
)

// SetupLifetime is how long a setup code lasts, unless the operator
// signs in first.
const SetupLifetime = 24 * time.Hour

// A PoolLabel is pool-label's reply.
type PoolLabel struct {
	PoolLabel string `json:"pool_label"`
	Changed   bool   `json:"changed,omitempty"`
}

// PoolLabel returns the pool label.
func (o *Ops) PoolLabel(ctx context.Context) (PoolLabel, error) {
	h, err := o.state(ctx)
	if err != nil {
		return PoolLabel{}, err
	}
	return PoolLabel{PoolLabel: h.Settings.PoolLabel}, nil
}

// SetPoolLabel renames the pool label, lowercase, under CheckPoolLabel's
// rules. Every runner carries it, so each node's runners are retired and
// minted again with the new one.
func (o *Ops) SetPoolLabel(ctx context.Context, from node.Origin, label string) (PoolLabel, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	label = normal(label)
	if err := CheckPoolLabel(label); err != nil {
		return PoolLabel{}, err
	}
	changed, err := o.changeSettings(ctx, from, func(s *hubstore.Settings) error {
		s.PoolLabel = label
		return nil
	})
	return PoolLabel{PoolLabel: label, Changed: changed}, err
}

// A SetupLink is a setup code just made: the link it's in, which the
// operator opens, when it expires, and whether its sign-in reassigns the
// operator. In JSON, the link is revealed.
type SetupLink struct {
	Link     secret.Value `json:"-"`
	Expires  time.Time    `json:"expires"`
	Reassign bool         `json:"reassign,omitempty"`
}

// MarshalJSON gives the link's fields and the link.
func (l SetupLink) MarshalJSON() ([]byte, error) {
	type fields SetupLink
	return json.Marshal(struct {
		Link string `json:"link"`
		fields
	}{l.Link.Reveal(), fields(l)})
}

// SetupCode makes a fresh setup code, which replaces the last one, for
// SetupLifetime or until the operator signs in. Once the operator is
// recorded, only a reassigning code is made: its next GitHub sign-in
// becomes the operator, and the operator stays until then.
func (o *Ops) SetupCode(ctx context.Context, from node.Origin, reassign bool) (SetupLink, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	code := o.c.NewSecret()
	expires := o.c.Now().Add(SetupLifetime).UTC()
	var base string
	_, err := o.changeSettings(ctx, from, func(s *hubstore.Settings) error {
		if s.Operator != 0 && !reassign {
			return refuse("setup-done", "setup is done, and the operator is recorded: to make another GitHub user the operator, run setup-code --reassign")
		}
		base = s.BaseURL
		s.SetupCode = hubstore.SetupCode{Hash: SetupCodeHash(code), Expires: expires, Reassign: reassign}
		return nil
	})
	if err != nil {
		return SetupLink{}, err
	}
	link := secret.New(base + SetupPath + "?" + url.Values{"code": {code.Reveal()}}.Encode())
	return SetupLink{Link: link, Expires: expires, Reassign: reassign}, nil
}

// A URLChange is set-url's reply: the Hub's new URL, the settings each
// GitHub App needs changed to match it, and the nodes to point at it.
type URLChange struct {
	URL     string   `json:"url"`
	From    string   `json:"from"`
	Changed bool     `json:"changed"`
	Apps    []App    `json:"apps"`
	Nodes   []NodeOS `json:"nodes"`
}

// An App is a connection's App, its settings page on GitHub, and the
// URLs its settings must hold. SignIn marks the App the operator signs
// in through: sign-in fails until its settings are changed.
type App struct {
	Connection string `json:"connection"`
	SignIn     bool   `json:"sign_in,omitempty"`
	Page       string `json:"page"`
	Homepage   string `json:"homepage_url"`
	Callback   string `json:"callback_url"`
	Setup      string `json:"setup_url"`
}

// A NodeOS is a node to run set-hub-url on, and its OS, or empty if it
// has never connected or its platform is unknown.
type NodeOS struct {
	Name            string `json:"name"`
	OS              string `json:"os,omitempty"`
	UnknownPlatform bool   `json:"unknown_platform,omitempty"`
}

// SetURL changes the Hub's URL, and only that. Each node keeps its
// identity and is pointed at the new URL with set-hub-url, which the Hub
// never pushes.
func (o *Ops) SetURL(ctx context.Context, from node.Origin, to string) (URLChange, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	u, err := CheckURL(to)
	if err != nil {
		return URLChange{}, err
	}
	h, err := o.state(ctx)
	if err != nil {
		return URLChange{}, err
	}
	out := URLChange{URL: u, From: h.Settings.BaseURL, Apps: []App{}, Nodes: []NodeOS{}}
	out.Changed, err = o.changeSettings(ctx, from, func(s *hubstore.Settings) error {
		s.BaseURL = u
		return nil
	})
	if err != nil {
		return URLChange{}, err
	}
	for i, c := range h.Connections {
		page := "https://github.com/settings/apps/" + c.AppSlug
		if c.Kind == hubstore.Organization {
			page = "https://github.com/organizations/" + c.Account + "/settings/apps/" + c.AppSlug
		}
		out.Apps = append(out.Apps, App{Connection: c.Account, SignIn: i == 0, Page: page,
			Homepage: u, Callback: u + SignInCallbackPath, Setup: u + InstalledPath})
	}
	for _, n := range h.Pool.Nodes {
		if n.Removed.IsZero() {
			osName, _ := osArch(n.Platform)
			out.Nodes = append(out.Nodes, NodeOS{Name: n.Name, OS: osName, UnknownPlatform: unknownPlatform(n.Platform)})
		}
	}
	slices.SortFunc(out.Nodes, func(a, b NodeOS) int { return strings.Compare(a.OS+" "+a.Name, b.OS+" "+b.Name) })
	return out, nil
}

// CheckURL returns the Hub's URL as the Hub keeps it, or refuses one it
// can't have: an https:// URL with a host, and no path, query or user.
func CheckURL(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	bad := func(why string) (string, error) {
		return "", refuse("invalid-url", "the URL %s won't do: %s", s, why)
	}
	switch {
	case err != nil || u.Host == "" || u.Opaque != "":
		return bad("write it as https://<host>, with a port if it isn't 443")
	case u.Scheme != "https":
		return bad("nodes only connect to a Hub over https://, so put a proxy in front of a plain HTTP one")
	case u.Path != "" && u.Path != "/", u.RawQuery != "", u.Fragment != "", u.User != nil:
		return bad("the Hub serves its own paths, so give only https://<host>")
	}
	return "https://" + u.Host, nil
}

// Status is status's reply.
type Status struct {
	URL      string   `json:"url"`
	Listener Listener `json:"listener"`
	Version  string   `json:"version"`
	// Protocol is the Hub-agent protocol the Hub speaks, and Accepts the
	// agents' protocol versions it accepts: its own and the one before.
	Protocol int   `json:"protocol"`
	Accepts  []int `json:"accepts"`
	// Operator is the operator's GitHub user ID, or 0 before the first
	// sign-in.
	Operator    int64        `json:"operator,omitempty"`
	Setup       Setup        `json:"setup"`
	Connections []Connection `json:"connections"`
	// Nodes counts the nodes that aren't tombstones, Connected those
	// connected, and Removed the tombstones.
	Nodes         int    `json:"nodes"`
	Connected     int    `json:"connected"`
	Removed       int    `json:"removed"`
	Targets       int    `json:"targets"`
	DefaultTarget string `json:"default_target,omitempty"`
	PoolLabel     string `json:"pool_label"`
}

// A Listener is the Hub's listener: its mode, and the address it
// listens on.
type Listener struct {
	Mode    string `json:"mode"`
	Address string `json:"address"`
}

// Setup is how far first-run setup has gone: the latest step finished,
// or empty before the first, and how many of how many are done.
type Setup struct {
	Step string `json:"step,omitempty"`
	Done int    `json:"done"`
	Of   int    `json:"of"`
}

// A Connection is a GitHub connection as status shows it.
type Connection struct {
	Account      string `json:"account"`
	Kind         string `json:"kind"`
	App          string `json:"app"`
	Installation int64  `json:"installation"`
}

// Steps are setup's steps, in order.
var Steps = []hubstore.Step{
	hubstore.StepChecks, hubstore.StepAccount, hubstore.StepCreateApp, hubstore.StepSignIn,
	hubstore.StepInstall, hubstore.StepEnable, hubstore.StepDone,
}

// Status returns the Hub's status.
func (o *Ops) Status(ctx context.Context) (Status, error) {
	h, err := o.state(ctx)
	if err != nil {
		return Status{}, err
	}
	s := h.Settings
	out := Status{
		URL: s.BaseURL, Listener: Listener{Mode: string(s.Listener.Mode), Address: s.Listener.Address},
		Version: o.c.Version, Protocol: o.c.Protocol, Accepts: []int{o.c.Protocol},
		Operator: s.Operator, Setup: Setup{Step: string(s.Setup), Done: slices.Index(Steps, s.Setup) + 1, Of: len(Steps)},
		Connections: []Connection{}, Targets: len(h.Targets), DefaultTarget: s.DefaultTarget, PoolLabel: s.PoolLabel,
	}
	if o.c.Protocol > 1 {
		out.Accepts = append(out.Accepts, o.c.Protocol-1)
	}
	for _, c := range h.Connections {
		out.Connections = append(out.Connections, Connection{Account: c.Account, Kind: string(c.Kind), App: c.AppSlug, Installation: c.Installation})
	}
	for _, n := range h.Pool.Nodes {
		switch {
		case !n.Removed.IsZero():
			out.Removed++
		case n.Connected:
			out.Connected++
			out.Nodes++
		default:
			out.Nodes++
		}
	}
	return out, nil
}

// A Version is version's reply: the running Hub's version.
type Version struct {
	Hub string `json:"hub"`
}

// Version returns the running Hub's version.
func (o *Ops) Version(context.Context) (Version, error) { return Version{Hub: o.c.Version}, nil }

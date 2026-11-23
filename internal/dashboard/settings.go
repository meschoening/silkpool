package dashboard

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/pool"
)

// Settings, as variant A of "Prototype the Hub dashboard" has it: the
// GitHub connections, each with its account, kind, App and
// installation, and "Add a GitHub connection…", which runs setup's
// account, create-App and install steps inline; then "Keep history for
// N days"; then the Hub itself. Adding a connection is the flow of
// package hubops' AddConnection: the account is chosen, the App's
// manifest goes to GitHub, which sends the browser back with the App,
// and the connection is saved, then installed from its row, after which
// "Add a target…" opens. Until GitHub sends the browser back, the
// account chosen is the session's. The page isn't live.

// A settingsPage is Settings.
type settingsPage struct {
	page
	Connections []connectionRow
	// Connect is "Add a GitHub connection…" open: "account" while the
	// account is chosen, "app" when its App is made on GitHub, or empty.
	Connect string
	// Kind and Org are the account chosen so far, Account its name, and
	// HasPersonal reports a personal connection already, which can't be
	// chosen again.
	Kind, Org, Account string
	HasPersonal        bool
	// Manifest is the App's, which the form posts to GitHub.
	Manifest hubops.Manifest
	// Days is the history setting's days, or what was written, and
	// Forever reports it keeping history forever.
	Days    string
	Forever bool
	Hub     hubRows
	// Refusal is why the last form was refused, shown where RefusedIn
	// says: "connect" or "history".
	Refusal   string
	RefusedIn string
}

// A connectionRow is one GitHub connection.
type connectionRow struct {
	Account string
	// Meta is its kind, its App, and its installation.
	Meta string
	// SignIn marks the connection the operator signs in through.
	SignIn bool
	// AppPage is its App's settings on GitHub, and InstallLink the App's
	// install page, until it's installed.
	AppPage, InstallLink string
	Organization         bool
}

// hubRows are the Hub's own rows: its address, its listener, its
// version and the agents it accepts, and who's signed in.
type hubRows struct {
	Address, Listener, Version, Operator string
}

// settings serves Settings, with "Add a GitHub connection…" open at
// the account step for ?connect, and at the App for the account the
// account step chose, ?connect=personal or
// ?connect=organization&org=<name>, whose state the trip to GitHub
// starts.
func (d *Dashboard) settings(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	p, status := settingsPage{}, http.StatusOK
	switch kind := q.Get("connect"); {
	case kind != "":
		p.Kind, p.Org = kind, q.Get("org")
		m, acct, err := d.ops.ConnectionManifest(r.Context(), hubstore.Kind(kind), p.Org, d.newState(w).Reveal())
		if err != nil {
			p.Connect, p.RefusedIn = "account", "connect"
			p.Refusal, status = refusalText(err)
			break
		}
		d.sessions.connect(cookie(r, sessionCookie), acct)
		p.Connect, p.Manifest, p.Account = "app", m, cmpOr(acct.Login, "your personal account")
	case q.Has("connect"):
		p.Connect = "account"
	}
	d.showSettings(w, r, a, status, p)
}

// showSettings serves Settings, as p opens its forms.
func (d *Dashboard) showSettings(w http.ResponseWriter, r *http.Request, a access, status int, p settingsPage) {
	ctx := r.Context()
	st, err := d.ops.Status(ctx)
	if err != nil {
		d.failed(w, r, err)
		return
	}
	keep, err := d.ops.KeepHistory(ctx)
	if err != nil {
		d.failed(w, r, err)
		return
	}
	fillSettings(&p, st, keep, d.ops.GitHubWeb())
	p.Login = a.who.Login
	p.Hub.Operator = a.who.Login + ", signed in with GitHub"
	d.render(w, r, status, "settings", p)
}

// fillSettings fills p in from the Hub's status and its history
// setting, with links to GitHub's pages at web.
func fillSettings(p *settingsPage, st hubops.Status, keep hubops.KeepHistory, web string) {
	p.page = page{Title: "Settings", Tab: "settings"}
	for i, c := range st.Connections {
		p.Connections = append(p.Connections, connectionRowOf(c, i == 0, web))
		p.HasPersonal = p.HasPersonal || c.Kind == string(hubstore.Personal)
	}
	if p.RefusedIn != "history" {
		p.Forever = keep.Forever
		p.Days = strconv.Itoa(cmpOrInt(keep.Days, pool.DefaultKeepHistory))
	}
	p.Hub.Address = st.URL
	p.Hub.Listener = listenerWords[st.Listener.Mode] + ", on " + st.Listener.Address
	var accepts []string
	for _, v := range st.Accepts {
		accepts = append(accepts, strconv.Itoa(v))
	}
	p.Hub.Version = st.Version + " · accepts agents on protocol " + strings.Join(accepts, " or ")
}

func cmpOrInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// listenerWords say how the Hub's listener serves TLS, by its mode.
var listenerWords = map[string]string{
	string(hubstore.SelfSigned):       "Its own self-signed certificate",
	string(hubstore.CertificateFiles): "Certificate files",
	string(hubstore.BehindProxy):      "Behind a TLS proxy",
}

// connectionRowOf returns how Settings shows c, which signIn marks as
// the one the operator signs in through, with links to GitHub's pages
// at web.
func connectionRowOf(c hubops.Connection, signIn bool, web string) connectionRow {
	row := connectionRow{Account: c.Account, SignIn: signIn, Organization: c.Kind == string(hubstore.Organization),
		AppPage: web + "/settings/apps/" + url.PathEscape(c.App)}
	if row.Organization {
		row.AppPage = web + "/organizations/" + url.PathEscape(c.Account) + "/settings/apps/" + url.PathEscape(c.App)
	}
	kind := "Personal account"
	if row.Organization {
		kind = "Organization"
	}
	installed := "installation " + strconv.FormatInt(c.Installation, 10)
	if c.Installation == 0 {
		installed = "not installed yet"
		row.InstallLink = web + "/apps/" + url.PathEscape(c.App) + "/installations/new"
	}
	row.Meta = kind + " · " + c.App + " · private, webhook off · " + installed
	return row
}

// connectionCreated converts the code GitHub sent the browser back
// with, after it made a new connection's App, into the connection to
// the account the session chose.
func (d *Dashboard) connectionCreated(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	if !backFromGitHub(w, r) {
		d.settingsAnswer(w, r, a, refusal("state", "GitHub sent back an App this page didn't ask for: add the connection again from here"), settingsPage{RefusedIn: "connect"})
		return
	}
	_, err := d.ops.AddConnection(r.Context(), a.origin(), a.connecting, r.URL.Query().Get("code"))
	if err == nil {
		d.sessions.connect(cookie(r, sessionCookie), hubstore.Account{})
	}
	d.settingsAnswer(w, r, a, err, settingsPage{RefusedIn: "connect"})
}

// keepHistory sets how long history is kept: keep=days, with days, or
// keep=forever.
func (d *Dashboard) keepHistory(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	keep, days := r.PostFormValue("keep"), strings.TrimSpace(r.PostFormValue("days"))
	p := settingsPage{RefusedIn: "history", Days: days, Forever: keep == "forever"}
	var err error
	switch keep {
	case "forever":
		_, err = d.ops.SetKeepHistory(r.Context(), a.origin(), pool.KeepForever)
	case "days":
		n, perr := strconv.Atoi(days)
		switch {
		case perr != nil:
			err = refusal("invalid-keep-history", "write the days as a whole number, "+strconv.Itoa(pool.MinKeepHistory)+" or more")
		case n < 0:
			// Not pool.KeepForever, which only "forever" asks for.
			err = refusal("invalid-keep-history", pool.ErrKeepHistory.Error())
		default:
			_, err = d.ops.SetKeepHistory(r.Context(), a.origin(), n)
		}
	default:
		err = refusal("invalid-keep-history", "choose how long to keep history")
	}
	d.settingsAnswer(w, r, a, err, p)
}

// settingsAnswer answers a form of Settings: on to Settings after a
// change, or the page again, open as p has it, with the refusal where p
// says.
func (d *Dashboard) settingsAnswer(w http.ResponseWriter, r *http.Request, a access, err error, p settingsPage) {
	if err == nil {
		see(w, r, "/settings")
		return
	}
	var status int
	p.Refusal, status = refusalText(err)
	d.showSettings(w, r, a, status, p)
}

// installedAfterSetup answers GitHub sending the browser back with
// installation id after setup, when a connection's App was installed or
// its repositories changed: on to "Add a target…" if the connection
// serves no target yet, or Settings, or Settings with the refusal.
func (d *Dashboard) installedAfterSetup(w http.ResponseWriter, r *http.Request, a access, id int64, err error) {
	var c hubstore.Connection
	if err == nil {
		c, err = d.ops.Install(r.Context(), a.origin(), id)
	}
	if err != nil {
		d.settingsAnswer(w, r, a, err, settingsPage{RefusedIn: "connect"})
		return
	}
	ts, err := d.ops.Targets(r.Context())
	if err != nil {
		d.failed(w, r, err)
		return
	}
	if !slices.ContainsFunc(ts.Targets, func(t hubops.Target) bool { return t.Connection == c.Account }) {
		see(w, r, "/targets?add")
		return
	}
	see(w, r, "/settings")
}

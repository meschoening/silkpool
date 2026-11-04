package hubops

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/github"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/secret"
)

// The first-run setup in the browser, as "Define how an operator sets up
// a Hub and connects GitHub"
// and ADRs 0001 and 0002 decided: a checklist of Steps, each saved as it
// finishes, so the dashboard resumes at the first unfinished one. The
// browser's part of each flow, and the setup code's guard, are the
// dashboard's. These are the Hub's.

// AppCreatedPath is where GitHub sends the browser back after it creates
// the App from the Hub's manifest, with the code CreateApp converts. It's
// the manifest's redirect URL, which the App doesn't keep, so set-url
// doesn't print it.
const AppCreatedPath = "/setup/app"

// ClockSlack is how far the Hub's clock may be from GitHub's. GitHub
// refuses an App JWT issued in its future or expiring more than 10
// minutes ahead, and the client backdates each one by a minute and has
// it expire 9 minutes on.
const ClockSlack = time.Minute

// SetupProgress is how far setup has gone, for the dashboard's
// checklist.
type SetupProgress struct {
	// URL is the Hub's.
	URL string
	// Step is the latest step finished, or empty before the first.
	Step hubstore.Step
	// Account is the GitHub account setup connects, once chosen, and
	// Connection its connection, once its App is created.
	Account    hubstore.Account
	Connection hubstore.Connection
	// Operator is the operator's GitHub user ID, or 0 until the first
	// sign-in.
	Operator int64
	// Expires is when the setup code expires, or zero once it's spent.
	Expires time.Time
}

// Next returns the first unfinished step, or empty once setup is done.
func (p SetupProgress) Next() hubstore.Step {
	i := slices.Index(Steps, p.Step) + 1
	if i >= len(Steps) {
		return ""
	}
	return Steps[i]
}

// Finished reports that setup is done.
func (p SetupProgress) Finished() bool { return p.Step == hubstore.StepDone }

// Setup returns how far setup has gone.
func (o *Ops) Setup(ctx context.Context) (SetupProgress, error) {
	h, err := o.state(ctx)
	if err != nil {
		return SetupProgress{}, err
	}
	return progressOf(h), nil
}

func progressOf(h hubstore.Hub) SetupProgress {
	s := h.Settings
	p := SetupProgress{URL: s.BaseURL, Step: s.Setup, Account: s.Connecting, Operator: s.Operator,
		Expires: s.SetupCode.Expires}
	if c, ok := setupConnection(h); ok {
		p.Connection = c
	}
	return p
}

// setupConnection returns the connection to the account setup connects,
// once its App is created.
func setupConnection(h hubstore.Hub) (hubstore.Connection, bool) {
	login := h.Settings.Connecting.Login
	i := slices.IndexFunc(h.Connections, func(c hubstore.Connection) bool { return login != "" && strings.EqualFold(c.Account, login) })
	if i < 0 {
		return hubstore.Connection{}, false
	}
	return h.Connections[i], true
}

// done reports whether setup has finished step.
func done(h hubstore.Hub, step hubstore.Step) bool {
	return slices.Index(Steps, h.Settings.Setup) >= slices.Index(Steps, step)
}

// errStep refuses a setup step taken out of turn.
func errStep(h hubstore.Hub) *Refusal {
	next := progressOf(h).Next()
	if next == "" {
		return refuse("setup-step", "setup is done")
	}
	return refuse("setup-step", "setup is at its %s step", next)
}

// advance moves setup on to step, if it's at the step before.
func advance(step hubstore.Step) func(*hubstore.Settings) error {
	return func(s *hubstore.Settings) error {
		if slices.Index(Steps, s.Setup)+1 == slices.Index(Steps, step) {
			s.Setup = step
		}
		return nil
	}
}

// Checks is what the checks step found: whether the Hub reached
// GitHub's API, or why not, and how far the Hub's clock is ahead of
// GitHub's.
type Checks struct {
	Reached bool          `json:"reached"`
	Problem string        `json:"problem,omitempty"`
	Skew    time.Duration `json:"skew"`
}

// ClockOK reports that the Hub's clock is close enough to GitHub's for
// App JWTs.
func (c Checks) ClockOK() bool { return c.Reached && c.Skew.Abs() < ClockSlack }

// Passed reports that both checks passed.
func (c Checks) Passed() bool { return c.Reached && c.ClockOK() }

// Check runs setup's checks: the Hub reaches api.github.com, and its
// clock is within ClockSlack of GitHub's. Once both pass, the step is
// saved.
func (o *Ops) Check(ctx context.Context, from node.Origin) (Checks, error) {
	before := o.c.Now()
	at, err := o.c.GitHub.Clock(ctx)
	if err != nil {
		return Checks{Problem: err.Error()}, nil
	}
	// GitHub's Date is to the second: take the middle of its second, and
	// of the call.
	mid := before.Add(o.c.Now().Sub(before) / 2)
	c := Checks{Reached: true, Skew: mid.Sub(at.Add(500 * time.Millisecond)).Round(time.Second)}
	if c.Passed() {
		o.changing.Lock()
		defer o.changing.Unlock()
		_, err = o.changeSettings(ctx, from, advance(hubstore.StepChecks))
	}
	return c, err
}

// githubLogin is the form of a GitHub login: letters, digits and single
// hyphens, neither first nor last, at most 39 characters.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// ChooseAccount saves the account setup connects: the personal account,
// whose login comes with its App, or the organization org. It picks the
// App's permissions. It can be changed until the App is created.
func (o *Ops) ChooseAccount(ctx context.Context, from node.Origin, kind hubstore.Kind, org string) error {
	o.changing.Lock()
	defer o.changing.Unlock()
	org = strings.TrimSpace(org)
	_, err := o.changeSettings(ctx, from, func(s *hubstore.Settings) error {
		if s.Setup != hubstore.StepChecks && s.Setup != hubstore.StepAccount {
			return errStep(hubstore.Hub{Settings: *s})
		}
		switch {
		case kind == hubstore.Personal:
			org = ""
		case kind != hubstore.Organization:
			return refuse("invalid-account", "choose your personal account or an organization")
		case !githubLogin.MatchString(org):
			return refuse("invalid-account", "%q isn't an organization's name on GitHub: write it as its URL has it, github.com/<name>", org)
		}
		s.Connecting = hubstore.Account{Kind: kind, Login: org}
		s.Setup = hubstore.StepAccount
		return nil
	})
	return err
}

// A Manifest is the App manifest the browser posts to GitHub, as the
// form field manifest, and the page of GitHub's it posts it to, with the
// state GitHub hands back.
type Manifest struct {
	Form string
	JSON string
	// Name is the name it gives the App.
	Name string
}

// appName is the longest name GitHub gives an App.
const appName = 34

// Manifest returns the manifest of the account's App, which GitHub
// creates when the browser posts it to its Form: private, with its
// webhook inactive and no events, the account kind's permissions, and
// the Hub as homepage, sign-in callback and setup URL, which GitHub also
// returns to when the operator changes the repositories it's installed
// on. GitHub then sends the browser to AppCreatedPath with a code and
// state.
//
// It's named "Silkpool <organization>" for an organization, and
// "Silkpool <Hub's host>" for a personal account, whose login the Hub
// can't know yet. The operator can rename it on GitHub's form.
func (o *Ops) Manifest(ctx context.Context, state string) (Manifest, error) {
	h, err := o.state(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if h.Settings.Setup != hubstore.StepAccount {
		return Manifest{}, errStep(h)
	}
	base, a := h.Settings.BaseURL, h.Settings.Connecting
	org := a.Kind == hubstore.Organization
	form := o.web() + "/settings/apps/new"
	name := "Silkpool " + hostLabel(base)
	if org {
		form = o.web() + "/organizations/" + url.PathEscape(a.Login) + "/settings/apps/new"
		name = "Silkpool " + a.Login
	}
	name = strings.TrimSpace(name[:min(len(name), appName)])
	type hook struct {
		URL    string `json:"url"`
		Active bool   `json:"active"`
	}
	m, err := json.Marshal(struct {
		Name          string            `json:"name"`
		URL           string            `json:"url"`
		RedirectURL   string            `json:"redirect_url"`
		CallbackURLs  []string          `json:"callback_urls"`
		SetupURL      string            `json:"setup_url"`
		SetupOnUpdate bool              `json:"setup_on_update"`
		Public        bool              `json:"public"`
		Hook          hook              `json:"hook_attributes"`
		Permissions   map[string]string `json:"default_permissions"`
	}{
		Name: name, URL: base, RedirectURL: base + AppCreatedPath,
		CallbackURLs: []string{base + SignInCallbackPath}, SetupURL: base + InstalledPath, SetupOnUpdate: true,
		Hook: hook{URL: base}, Permissions: github.Permissions(org),
	})
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{Form: form + "?" + url.Values{"state": {state}}.Encode(), JSON: string(m), Name: name}, nil
}

// hostLabel returns the first label of the host of the URL u, or the
// whole of an IP address.
func hostLabel(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "Hub"
	}
	if net.ParseIP(p.Hostname()) != nil {
		return p.Hostname()
	}
	label, _, _ := strings.Cut(p.Hostname(), ".")
	return label
}

// web is github.com, or the fake's URL.
func (o *Ops) web() string { return strings.TrimRight(cmp.Or(o.c.Web, github.Web), "/") }

// GitHubWeb returns github.com's URL, where setup sends the browser, as
// the dashboard's pages must allow their forms to.
func (o *Ops) GitHubWeb() string { return o.web() }

// CreateApp converts the code GitHub sent the browser back with into the
// new App's credentials, which become files in the Hub's state folder,
// and saves the connection to its owner. An App owned by another account
// than the one chosen is refused, and setup stays at the account step.
func (o *Ops) CreateApp(ctx context.Context, from node.Origin, code string) (hubstore.Connection, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return hubstore.Connection{}, err
	}
	if h.Settings.Setup != hubstore.StepAccount {
		return hubstore.Connection{}, errStep(h)
	}
	c, owner, err := o.c.GitHub.CreateApp(ctx, code)
	if err != nil {
		if github.Status(err) == http.StatusNotFound {
			return hubstore.Connection{}, refuse("app-code", "GitHub doesn't know the code it sent back, or it was used: create the App again")
		}
		return hubstore.Connection{}, err
	}
	want := h.Settings.Connecting
	if owner.Organization != (want.Kind == hubstore.Organization) || want.Kind == hubstore.Organization && !strings.EqualFold(owner.Login, want.Login) {
		return hubstore.Connection{}, refuse("wrong-account", "GitHub created the App %s for %s, not for the account you chose: delete it in its settings on GitHub, then create it again", c.AppSlug, owner.Login)
	}
	if err := o.apply(ctx, hubstore.PutConnection{Connection: c, Origin: from}); err != nil {
		return hubstore.Connection{}, err
	}
	_, err = o.changeSettings(ctx, from, func(s *hubstore.Settings) error {
		s.Connecting.Login = c.Account
		return advance(hubstore.StepCreateApp)(s)
	})
	return c, err
}

// SignInLink returns where the browser goes to sign in through the
// sign-in App, the first connection's, which sends it back to
// SignInCallbackPath with a code and state.
func (o *Ops) SignInLink(ctx context.Context, state string) (string, error) {
	h, err := o.state(ctx)
	if err != nil {
		return "", err
	}
	if len(h.Connections) == 0 {
		return "", refuse("no-app", "there's no GitHub App to sign in through yet: setup creates it")
	}
	q := url.Values{"client_id": {h.Connections[0].ClientID}, "redirect_uri": {h.Settings.BaseURL + SignInCallbackPath}, "state": {state}}
	return o.web() + "/login/oauth/authorize?" + q.Encode(), nil
}

// SignedIn is who signed in.
type SignedIn struct {
	ID    int64
	Login string
}

// SignIn exchanges the code of a sign-in through the sign-in App, and
// admits the GitHub user it names if they're the operator. Until the
// operator is recorded, the first sign-in that comes with the setup code
// records them, by user ID, and spends the code; a reassigning code's
// sign-in records a new one. Anyone else is refused, which history keeps
// with their login and address, at most once an hour per login.
func (o *Ops) SignIn(ctx context.Context, code string, setupCode secret.Value, address string) (SignedIn, error) {
	h, err := o.state(ctx)
	if err != nil {
		return SignedIn{}, err
	}
	if len(h.Connections) == 0 {
		return SignedIn{}, refuse("no-app", "there's no GitHub App to sign in through yet: setup creates it")
	}
	user, err := o.c.GitHub.SignIn(ctx, h.Connections[0], code, h.Settings.BaseURL+SignInCallbackPath)
	if err != nil {
		if e, ok := errors.AsType[*github.Error](err); ok && e.Status == http.StatusOK {
			return SignedIn{}, refuse("sign-in", "GitHub didn't sign you in: %s. Try again", e.Message)
		}
		return SignedIn{}, err
	}
	o.changing.Lock()
	defer o.changing.Unlock()
	if h, err = o.state(ctx); err != nil {
		return SignedIn{}, err
	}
	s := h.Settings
	as := node.Origin{Surface: node.Dashboard, Login: user.Login}
	switch {
	case s.Operator == 0 || s.SetupCode.Reassign && o.opens(s, setupCode):
		if !o.opens(s, setupCode) {
			return SignedIn{}, refuse("setup-code", "the operator isn't recorded yet: open the setup link from silkpool-hub init first")
		}
		if _, err := o.changeSettings(ctx, as, func(s *hubstore.Settings) error {
			s.Operator, s.SetupCode = user.ID, hubstore.SetupCode{}
			return advance(hubstore.StepSignIn)(s)
		}); err != nil {
			return SignedIn{}, err
		}
	case user.ID != s.Operator:
		if err := o.apply(ctx, pool.RefuseSignIn{Login: user.Login, Address: address}); err != nil {
			return SignedIn{}, err
		}
		return SignedIn{}, refuse("not-operator", "you're signed in to GitHub as %s, who isn't this Hub's operator", user.Login)
	}
	return SignedIn{ID: user.ID, Login: user.Login}, o.apply(ctx, pool.SignIn{Login: user.Login, Address: address})
}

// opens reports whether code is the setup code s holds, unexpired.
func (o *Ops) opens(s hubstore.Settings, code secret.Value) bool {
	return code.Reveal() != "" && s.SetupCode.Hash == SetupCodeHash(code) && o.c.Now().Before(s.SetupCode.Expires)
}

// SetupCodeOpens reports whether code opens setup, as the dashboard asks
// before anyone signs in, and whether its sign-in reassigns the
// operator. A refusal says why it doesn't: it's spent, replaced or
// expired.
func (o *Ops) SetupCodeOpens(ctx context.Context, code secret.Value) (reassign bool, err error) {
	h, err := o.state(ctx)
	if err != nil {
		return false, err
	}
	s := h.Settings
	switch {
	case o.opens(s, code):
		return s.SetupCode.Reassign, nil
	case s.SetupCode.Hash == ([32]byte{}) && s.Operator != 0:
		return false, refuse("setup-code-used", "setup's sign-in spent this setup link: sign in with GitHub instead")
	case s.SetupCode.Hash != SetupCodeHash(code):
		return false, refuse("setup-code-unknown", "this setup link isn't the Hub's latest: a newer one replaced it. Make another on the Hub's host with sudo silkpool-hub setup-code")
	}
	return false, refuse("setup-code-expired", "this setup link expired after 24 h. Make another on the Hub's host with sudo silkpool-hub setup-code")
}

// InstallLink returns the App's install page on GitHub, which sends the
// browser back to InstalledPath with the installation's ID.
func (o *Ops) InstallLink(ctx context.Context) (string, error) {
	h, err := o.state(ctx)
	if err != nil {
		return "", err
	}
	c, ok := setupConnection(h)
	if !ok {
		return "", errStep(h)
	}
	return o.web() + "/apps/" + url.PathEscape(c.AppSlug) + "/installations/new", nil
}

// Install saves the installation GitHub sent the browser back with,
// after looking it up with the JWT of each connection's App in turn:
// the browser hands it over, so only an App's own installation, on the
// connection's account, counts. GitHub sends the browser back again when
// the operator changes its repositories, or installs the App again.
func (o *Ops) Install(ctx context.Context, from node.Origin, installation int64) (hubstore.Connection, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return hubstore.Connection{}, err
	}
	if !done(h, hubstore.StepSignIn) {
		return hubstore.Connection{}, errStep(h)
	}
	for _, c := range h.Connections {
		in, err := o.c.GitHub.Installation(ctx, c, installation)
		if github.Status(err) == http.StatusNotFound {
			continue
		}
		if err != nil {
			return hubstore.Connection{}, err
		}
		kind := hubstore.Personal
		if in.Account.Organization {
			kind = hubstore.Organization
		}
		if !strings.EqualFold(in.Account.Login, c.Account) || kind != c.Kind {
			return hubstore.Connection{}, refuse("wrong-account", "the App %s is installed on %s, not on %s", c.AppSlug, in.Account.Login, c.Account)
		}
		if in.Suspended {
			return hubstore.Connection{}, refuse("suspended", "the App %s's installation on %s is suspended: unsuspend it in the account's settings on GitHub", c.AppSlug, c.Account)
		}
		c.Installation = in.ID
		if err := o.apply(ctx, hubstore.PutConnection{Connection: c, Origin: from}); err != nil {
			return hubstore.Connection{}, err
		}
		_, err = o.changeSettings(ctx, from, advance(hubstore.StepInstall))
		return c, err
	}
	return hubstore.Connection{}, refuse("not-installed", "none of the Hub's GitHub Apps has an installation %s", strconv.FormatInt(installation, 10))
}

// FinishSetup finishes setup, once a target is added.
func (o *Ops) FinishSetup(ctx context.Context, from node.Origin) error {
	o.changing.Lock()
	defer o.changing.Unlock()
	_, err := o.changeSettings(ctx, from, func(s *hubstore.Settings) error {
		if s.Setup != hubstore.StepEnable && s.Setup != hubstore.StepDone {
			return errStep(hubstore.Hub{Settings: *s})
		}
		s.Setup = hubstore.StepDone
		return nil
	})
	return err
}

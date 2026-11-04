package dashboard

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wording"
)

// A setupPage is the first-run checklist: until setup finishes, the
// whole page. Done steps collapse to one line, the step to do now is
// open with one primary button, and later steps are grey.
type setupPage struct {
	page
	// Host is the Hub's, and Number the step to do now, of Of.
	Host       string
	Number, Of int
	// Resumed reports a setup picked up where it was left, and WithCode
	// one opened with the setup code, before sign-in.
	Resumed, WithCode bool
	Steps             []setupStep
	// Now is what the step to do now shows.
	Now stepBody
}

// A setupStep is one line of the checklist.
type setupStep struct {
	Name   string
	Number int
	Title  string
	// State is done, now or later, and Done the line a done step
	// collapses to.
	State string
	Done  string
}

// stepBody is what the step to do now shows: its refusal, if the last
// try was refused, and what the step needs.
type stepBody struct {
	Refusal string
	// Checks is a run of the checks that didn't pass, and ClockOff says
	// how far off the clock is, if that failed.
	Checks   *hubops.Checks
	ClockOff string
	// Kind and Org are the account step's choice so far.
	Kind, Org string
	// Manifest is the App's, which the form posts to GitHub.
	Manifest hubops.Manifest
	// Account is the account setup connects, and Organization reports
	// an organization's.
	Account      string
	Organization bool
	// InstallLink is the App's install page.
	InstallLink string
	// Candidates are what the account offers as targets, Failed why
	// GitHub didn't list them, and Pick the one picked.
	Candidates []candidate
	Failed     []string
	Pick       string
}

// A candidate is a target GitHub offers, as Add a target lists it.
type candidate struct {
	Target, Title, Meta string
	Disabled            bool
}

// setup serves the checklist, or, with the setup link's code, swaps the
// code for a cookie and redirects to a clean URL.
func (d *Dashboard) setup(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("code") {
		d.setupLink(w, r)
		return
	}
	d.serveSetup(w, r, http.StatusOK, stepBody{})
}

// setupLink takes the setup code the link carries.
func (d *Dashboard) setupLink(w http.ResponseWriter, r *http.Request) {
	code := secret.New(r.URL.Query().Get("code"))
	reassign, err := d.ops.SetupCodeOpens(r.Context(), code)
	if err != nil {
		text, status := refusalText(err)
		if status == http.StatusUnprocessableEntity {
			status = http.StatusForbidden
		}
		d.render(w, r, status, "link", linkPage{page: page{Title: "Set up your Hub"}, Refusal: text})
		return
	}
	p, err := d.ops.Setup(r.Context())
	if err != nil {
		d.failed(w, r, err)
		return
	}
	setCookie(w, setupCookie, code, p.Expires)
	switch {
	case reassign:
		see(w, r, "/sign-in")
	case p.Step != "":
		see(w, r, hubops.SetupPath+"?resumed")
	default:
		see(w, r, hubops.SetupPath)
	}
}

// A linkPage says the dashboard can't be opened yet: the setup link
// didn't open setup, or none was opened.
type linkPage struct {
	page
	Refusal string
}

// serveSetup serves the checklist with now as the open step's body, or
// sends a request that can't take setup's steps where it can go.
func (d *Dashboard) serveSetup(w http.ResponseWriter, r *http.Request, status int, now stepBody) {
	a, ok := d.setupAccess(w, r)
	if !ok {
		return
	}
	if a.progress.Finished() {
		see(w, r, "/")
		return
	}
	p, err := d.setupPage(r.Context(), w, a, now)
	if err != nil {
		d.failed(w, r, err)
		return
	}
	_, p.Resumed = r.URL.Query()["resumed"]
	d.render(w, r, status, "setup", p)
}

// setupAccess returns who r comes from if they may take setup's steps.
// Otherwise it answers: before the operator is recorded, with how to
// open setup, and after, with sign-in.
func (d *Dashboard) setupAccess(w http.ResponseWriter, r *http.Request) (access, bool) {
	a, err := d.access(r.Context(), r)
	switch {
	case err != nil:
		d.failed(w, r, err)
	case a.opens():
		return a, true
	case a.progress.Operator != 0 || a.reassign:
		see(w, r, "/sign-in")
	default:
		d.render(w, r, http.StatusForbidden, "link", linkPage{page: page{Title: "Set up your Hub"}})
	}
	return access{}, false
}

// setupPage builds the checklist for a, with now as the open step's
// body.
func (d *Dashboard) setupPage(ctx context.Context, w http.ResponseWriter, a access, now stepBody) (setupPage, error) {
	pr := a.progress
	next := pr.Next()
	u, _ := url.Parse(pr.URL)
	p := setupPage{page: page{Title: "Set up your Hub", Login: a.who.Login}, Host: u.Host, Of: len(hubops.Steps), WithCode: !a.signedIn()}
	c := pr.Connection
	for i, s := range hubops.Steps {
		st := setupStep{Name: string(s), Number: i + 1, Title: wording.SetupSteps[string(s)], State: "later"}
		switch {
		case s == next:
			st.State, p.Number = "now", i+1
		case slices.Index(hubops.Steps, pr.Step) >= i:
			st.State = "done"
			done, err := d.doneLine(ctx, s, a)
			if err != nil {
				return setupPage{}, err
			}
			st.Done = done
		}
		p.Steps = append(p.Steps, st)
	}
	now.Account, now.Organization = cmpOr(c.Account, pr.Account.Login), pr.Account.Kind == hubstore.Organization
	switch next {
	case hubstore.StepAccount:
		if now.Kind == "" {
			now.Kind, now.Org = string(pr.Account.Kind), pr.Account.Login
		}
	case hubstore.StepCreateApp:
		m, err := d.ops.Manifest(ctx, d.newState(w).Reveal())
		if err != nil {
			return setupPage{}, err
		}
		now.Manifest = m
	case hubstore.StepInstall:
		link, err := d.ops.InstallLink(ctx)
		if err != nil {
			return setupPage{}, err
		}
		now.InstallLink = link
	case hubstore.StepEnable:
		av, err := d.ops.Available(ctx)
		if err != nil {
			return setupPage{}, err
		}
		for _, f := range av.Failed {
			now.Failed = append(now.Failed, wording.Sentence(f.Message))
		}
		for _, cand := range av.Candidates {
			now.Candidates = append(now.Candidates, candidateOf(cand))
		}
	}
	p.Now = now
	return p, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// candidateOf returns how Add a target lists cand.
func candidateOf(cand hubops.Candidate) candidate {
	out := candidate{Target: cand.Target, Disabled: cand.Refused != "" || cand.Added}
	if g := cand.Group; g != nil {
		out.Title = g.Name + " group"
		out.Meta = "Serves " + wording.Serves(cand.Connection, g.Visibility, g.Repositories) + "."
	} else {
		_, name, _ := strings.Cut(cand.Target, "/")
		out.Title, out.Meta = name, "Private"
	}
	switch {
	case cand.Refused != "":
		out.Meta = wording.CandidateRefused(cand.Refused)
	case cand.Added:
		out.Meta = "Already a target"
	}
	return out
}

// doneLine is the line a done step collapses to.
func (d *Dashboard) doneLine(ctx context.Context, s hubstore.Step, a access) (string, error) {
	pr, c := a.progress, a.progress.Connection
	switch s {
	case hubstore.StepChecks:
		return "Reaches api.github.com. The clock is within a minute of GitHub's.", nil
	case hubstore.StepAccount:
		if pr.Account.Kind == hubstore.Organization {
			return "Organization: " + pr.Account.Login, nil
		}
		if pr.Account.Login != "" {
			return "Personal account: " + pr.Account.Login, nil
		}
		return "Personal account", nil
	case hubstore.StepCreateApp:
		return "Created " + c.AppSlug + ": private, webhook off", nil
	case hubstore.StepSignIn:
		if a.who.Login == "" {
			return "Signed in", nil
		}
		return "Signed in as " + a.who.Login, nil
	case hubstore.StepInstall:
		return "Installed on " + c.Account, nil
	case hubstore.StepEnable:
		ts, err := d.ops.Targets(ctx)
		if err != nil {
			return "", err
		}
		if len(ts.Targets) == 0 {
			return "Added a target", nil
		}
		return "Added " + ts.Targets[0].Target, nil
	}
	return "", nil
}

// stepDone answers a step's form: on to the checklist once it's done, or
// the checklist again with the refusal in the open step.
func (d *Dashboard) stepDone(w http.ResponseWriter, r *http.Request, err error, now stepBody) {
	if err == nil {
		see(w, r, hubops.SetupPath)
		return
	}
	text, status := refusalText(err)
	if status == http.StatusBadGateway {
		d.log.Warn("a setup step failed", "path", logPath(r), "err", err)
	}
	now.Refusal = text
	d.serveSetup(w, r, status, now)
}

// checks runs the checks step.
func (d *Dashboard) checks(w http.ResponseWriter, r *http.Request) {
	a, ok := d.setupAccess(w, r)
	if !ok {
		return
	}
	c, err := d.ops.Check(r.Context(), a.origin())
	if err != nil || c.Passed() {
		d.stepDone(w, r, err, stepBody{})
		return
	}
	now := stepBody{Checks: &c}
	if c.Reached {
		now.ClockOff = wording.ClockOff(c.Skew)
	}
	d.serveSetup(w, r, http.StatusUnprocessableEntity, now)
}

// account saves the account step's choice.
func (d *Dashboard) account(w http.ResponseWriter, r *http.Request) {
	a, ok := d.setupAccess(w, r)
	if !ok {
		return
	}
	kind, org := r.PostFormValue("kind"), r.PostFormValue("org")
	err := d.ops.ChooseAccount(r.Context(), a.origin(), hubstore.Kind(kind), org)
	d.stepDone(w, r, err, stepBody{Kind: kind, Org: org})
}

// appCreated converts the code GitHub sent the browser back with, after
// it created the App.
func (d *Dashboard) appCreated(w http.ResponseWriter, r *http.Request) {
	a, ok := d.setupAccess(w, r)
	if !ok {
		return
	}
	if !backFromGitHub(w, r) {
		d.stepDone(w, r, refusal("state", "GitHub sent back an App this page didn't ask for: create it again from here"), stepBody{})
		return
	}
	_, err := d.ops.CreateApp(r.Context(), a.origin(), r.URL.Query().Get("code"))
	d.stepDone(w, r, err, stepBody{})
}

// installed saves the installation GitHub sent the browser back with,
// after the App was installed or its repositories changed.
func (d *Dashboard) installed(w http.ResponseWriter, r *http.Request) {
	a, err := d.access(r.Context(), r)
	if err != nil {
		d.failed(w, r, err)
		return
	}
	if !a.signedIn() {
		see(w, r, "/sign-in")
		return
	}
	id, perr := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if perr != nil {
		d.stepDone(w, r, refusal("not-installed", "GitHub sent back no installation"), stepBody{})
		return
	}
	_, err = d.ops.Install(r.Context(), a.origin(), id)
	if err == nil && a.progress.Finished() {
		see(w, r, "/")
		return
	}
	d.stepDone(w, r, err, stepBody{})
}

// target adds the target picked in Add a target.
func (d *Dashboard) target(w http.ResponseWriter, r *http.Request) {
	a, ok := d.setupAccess(w, r)
	if !ok {
		return
	}
	pick := r.PostFormValue("target")
	if pick == "" {
		d.stepDone(w, r, refusal("no-target", "choose a target first"), stepBody{})
		return
	}
	_, err := d.ops.AddTarget(r.Context(), a.origin(), pick)
	d.stepDone(w, r, err, stepBody{Pick: pick})
}

// finish ends setup, on to the fleet.
func (d *Dashboard) finish(w http.ResponseWriter, r *http.Request) {
	a, ok := d.setupAccess(w, r)
	if !ok {
		return
	}
	if err := d.ops.FinishSetup(r.Context(), a.origin()); err != nil {
		d.stepDone(w, r, err, stepBody{})
		return
	}
	see(w, r, "/")
}

// refusal is one of the dashboard's own refusals.
func refusal(code, message string) *hubops.Refusal {
	return &hubops.Refusal{Code: code, Message: message}
}

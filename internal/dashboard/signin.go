package dashboard

import (
	"net/http"

	"github.com/meschoening/silkpool/internal/hubops"
)

// A signInPage asks the operator to sign in with GitHub, saying why the
// last try was refused, if it was. With a reassigning setup code, the
// next sign-in becomes the operator.
type signInPage struct {
	page
	Refusal  string
	Reassign bool
}

// signInPage serves the sign-in page, unless the operator is signed in
// or setup hasn't reached sign-in.
func (d *Dashboard) signInPage(w http.ResponseWriter, r *http.Request) {
	a, err := d.access(r.Context(), r)
	switch {
	case err != nil:
		d.failed(w, r, err)
	case a.signedIn():
		see(w, r, "/")
	case a.progress.Operator == 0 && !a.reassign:
		see(w, r, hubops.SetupPath)
	default:
		d.render(w, r, http.StatusOK, "signin", signInPage{page: page{Title: "Sign in"}, Reassign: a.reassign})
	}
}

// signIn sends the browser to sign in through the sign-in App on GitHub.
func (d *Dashboard) signIn(w http.ResponseWriter, r *http.Request) {
	link, err := d.ops.SignInLink(r.Context(), d.newState(w).Reveal())
	if err != nil {
		text, status := refusalText(err)
		d.render(w, r, status, "signin", signInPage{page: page{Title: "Sign in"}, Refusal: text})
		return
	}
	see(w, r, link)
}

// signedIn takes the sign-in GitHub sent the browser back with: the
// operator gets a session, and anyone else is refused.
func (d *Dashboard) signedIn(w http.ResponseWriter, r *http.Request) {
	refused := func(status int, text string) {
		a, _ := d.access(r.Context(), r)
		d.render(w, r, status, "signin", signInPage{page: page{Title: "Sign in"}, Refusal: text, Reassign: a.reassign})
	}
	q := r.URL.Query()
	if !backFromGitHub(w, r) {
		refused(http.StatusForbidden, "GitHub sent back a sign-in this page didn't ask for. Sign in again.")
		return
	}
	if q.Get("error") != "" {
		refused(http.StatusForbidden, "GitHub didn't sign you in: "+q.Get("error")+". Sign in again.")
		return
	}
	who, err := d.ops.SignIn(r.Context(), q.Get("code"), cookie(r, setupCookie), address(r))
	if err != nil {
		text, status := refusalText(err)
		if status == http.StatusUnprocessableEntity {
			status = http.StatusForbidden
		}
		refused(status, text)
		return
	}
	token, ends := d.sessions.open(who)
	setCookie(w, sessionCookie, token, ends)
	clearCookie(w, setupCookie)
	p, err := d.ops.Setup(r.Context())
	switch {
	case err != nil:
		d.failed(w, r, err)
	case p.Finished():
		see(w, r, "/")
	case p.Step != "sign-in":
		see(w, r, hubops.SetupPath+"?resumed")
	default:
		see(w, r, hubops.SetupPath)
	}
}

// signOut ends the session.
func (d *Dashboard) signOut(w http.ResponseWriter, r *http.Request) {
	d.sessions.end(cookie(r, sessionCookie))
	clearCookie(w, sessionCookie)
	see(w, r, "/sign-in")
}

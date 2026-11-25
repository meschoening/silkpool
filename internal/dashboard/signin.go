package dashboard

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/meschoening/silkpool/internal/hubops"
)

// A signInPage asks the operator to sign in with GitHub, saying why the
// last try was refused, if it was. With a reassigning setup code, the
// next sign-in becomes the operator. Elsewhere is the Hub's address,
// when the page was posted at another, where sign-in can't start.
type signInPage struct {
	page
	Refusal   string
	Reassign  bool
	Elsewhere string
}

// signInPage serves the sign-in page, at the Hub's address, unless the
// operator is signed in or setup hasn't reached sign-in. It moves there
// once: behind a proxy that names the Hub its own way, the Host never
// matches, so the page is served where it was moved to.
func (d *Dashboard) signInPage(w http.ResponseWriter, r *http.Request) {
	a, err := d.access(r.Context(), r)
	switch {
	case err != nil:
		d.failed(w, r, err)
	case !atAddress("https", r.Host, a.progress.URL) && !r.URL.Query().Has("moved"):
		see(w, r, a.progress.URL+"/sign-in?moved")
	case a.signedIn():
		see(w, r, "/")
	case a.progress.Operator == 0 && !a.reassign:
		see(w, r, hubops.SetupPath)
	default:
		d.render(w, r, http.StatusOK, "signin", signInPage{page: page{Title: "Sign in"}, Reassign: a.reassign})
	}
}

// signIn sends the browser to sign in through the sign-in App on GitHub,
// from a page at the Hub's address only, as its Origin says, which a
// proxy leaves as the browser sent it. Elsewhere, it names the address:
// a redirect there would break the form-action of the page that posted.
// A form posted with no Origin, or null, as one from a page served with
// no-referrer is, is taken at its Host.
func (d *Dashboard) signIn(w http.ResponseWriter, r *http.Request) {
	p, err := d.ops.Setup(r.Context())
	if err != nil {
		d.failed(w, r, err)
		return
	}
	scheme, host := "https", r.Host
	if o, err := url.Parse(r.Header.Get("Origin")); err == nil && o.Host != "" {
		scheme, host = o.Scheme, o.Host
	}
	if !atAddress(scheme, host, p.URL) {
		d.render(w, r, http.StatusForbidden, "signin", signInPage{page: page{Title: "Sign in"}, Elsewhere: p.URL})
		return
	}
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

// atAddress reports whether scheme and host are those of address, the
// Hub's, where GitHub sends a sign-in back, so where its cookies are. An
// unknown host counts as at the address.
func atAddress(scheme, host, address string) bool {
	u, err := url.Parse(address)
	if host == "" || err != nil || u.Host == "" {
		return true
	}
	return strings.EqualFold(scheme, u.Scheme) && hostKey(scheme, host) == hostKey(u.Scheme, u.Host)
}

// hostKey returns host as a browser sends it for scheme: lowercase, an
// IP address in its shortest form, and with its port, the scheme's
// default if it has none.
func hostKey(scheme, host string) string {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name, port = strings.Trim(host, "[]"), ""
	}
	if port == "" {
		port = "443"
		if strings.EqualFold(scheme, "http") {
			port = "80"
		}
	}
	if ip, err := netip.ParseAddr(name); err == nil {
		name = ip.String()
	}
	return strings.ToLower(net.JoinHostPort(name, port))
}

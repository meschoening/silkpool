// Package dashboard is the Hub's dashboard: the pages the Hub serves on
// its one listener, beside the agents' paths, built from Go templates
// over the shared UI package (package ui), as ADR 0009 decided. Forms
// post to the Hub, which answers a change with a redirect to the page it
// changed, and a refusal with the page again, the refusal in its part.
//
// This is its frame and its first pages: first-run setup, the checklist
// of "Define how an operator sets up a Hub and connects GitHub",
// laid out as the First run section of "Prototype the Hub dashboard";
// signing in with GitHub (ADR 0002); and the fleet's frame, with the
// empty pool setup ends at.
//
// Access: until the operator is recorded, the setup code guards every
// page. The setup link carries it once: the dashboard swaps it for a
// cookie and redirects to a clean URL, so neither proxy logs nor the
// browser's history keep it. After that, every page needs a sign-in
// through GitHub, as the operator, and sessions live in the Hub's memory
// only. The code, the session cookies and the sign-in's state are each a
// secret.Value. A form posted from another origin is refused.
//
// A page with live parts stays current without a reload: the
// live-update script opens its stream, at its own URL, and the
// dashboard pushes the parts that change (see live.go).
//
// Every change goes through package hubops, which the Hub's CLI shares.
package dashboard

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/ui"
	"github.com/meschoening/silkpool/internal/wording"
)

// Config is what the dashboard needs.
type Config struct {
	Ops *hubops.Ops
	// Log is where it logs a failure, or nil for slog.Default. It never
	// logs a query string.
	Log *slog.Logger
	// Now reads the clock, for sessions, or is nil for time.Now.
	Now func() time.Time
	// LiveEvery is how often a page's stream renders the page again, or
	// zero for LiveEvery. Only tests set it.
	LiveEvery time.Duration
}

// A Dashboard serves the dashboard's pages. It's an http.Handler, safe
// for concurrent use.
type Dashboard struct {
	ops       *hubops.Ops
	log       *slog.Logger
	now       func() time.Time
	liveEvery time.Duration
	pages     map[string]*template.Template
	sessions  *sessions
	mux       *http.ServeMux
}

// New returns the dashboard over c.Ops.
func New(c Config) *Dashboard {
	d := &Dashboard{ops: c.Ops, log: c.Log, now: c.Now, liveEvery: c.LiveEvery, pages: parsePages()}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.liveEvery == 0 {
		d.liveEvery = LiveEvery
	}
	d.sessions = newSessions(d.now)
	d.routes()
	return d
}

func (d *Dashboard) routes() {
	m := http.NewServeMux()
	m.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(ui.Assets())))
	m.HandleFunc("GET /{$}", d.home)
	m.HandleFunc("GET "+hubops.SetupPath, d.setup)
	m.HandleFunc("POST /setup/checks", d.checks)
	m.HandleFunc("POST /setup/account", d.account)
	m.HandleFunc("GET "+hubops.AppCreatedPath, d.appCreated)
	m.HandleFunc("GET "+hubops.InstalledPath, d.installed)
	m.HandleFunc("POST /setup/target", d.target)
	m.HandleFunc("POST /setup/done", d.finish)
	m.HandleFunc("GET /sign-in", d.signInPage)
	m.HandleFunc("POST /sign-in", d.signIn)
	m.HandleFunc("GET "+hubops.SignInCallbackPath, d.signedIn)
	m.HandleFunc("POST /sign-out", d.signOut)
	m.HandleFunc("GET /nodes/{name}", d.node)
	m.HandleFunc("POST /nodes/{name}/command", d.command)
	m.HandleFunc("POST /nodes/{name}/slot-limit", d.slotLimit)
	m.HandleFunc("POST /nodes/{name}/budget", d.budget)
	m.HandleFunc("POST /nodes/{name}/auto-resume", d.autoResume)
	m.HandleFunc("POST /nodes/{name}/dismiss", d.dismiss)
	m.HandleFunc("POST /nodes/{name}/rename", d.rename)
	m.HandleFunc("POST /nodes/{name}/target", d.retarget)
	m.HandleFunc("POST /nodes/{name}/labels", d.addLabel)
	m.HandleFunc("POST /nodes/{name}/labels/remove", d.removeLabel)
	m.HandleFunc("POST /nodes/{name}/remove", d.remove)
	m.HandleFunc("GET /nodes/add", d.addNode)
	m.HandleFunc("POST /nodes/add", d.makeCode)
	m.HandleFunc("POST /nodes/add/revoke", d.revokeCode)
	m.HandleFunc("GET /targets", d.targets)
	m.HandleFunc("POST /targets", d.addTarget)
	m.HandleFunc("POST /targets/default", d.defaultTarget)
	m.HandleFunc("POST /targets/remove", d.removeTarget)
	m.HandleFunc("POST /targets/pool-label", d.poolLabel)
	m.HandleFunc("/", d.notFound)
	d.mux = m
}

// ServeHTTP serves r, with the headers every answer carries, refusing a
// form posted from another origin. A request for a page's stream gets
// the stream.
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self' "+origin(d.ops.GitHubWeb())+"; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	if wantsStream(r) {
		d.stream(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		http.Error(w, "A form from another site can't change this Hub.", http.StatusForbidden)
		return
	}
	d.mux.ServeHTTP(w, r)
}

// origin returns the origin of the URL u: its scheme and host.
func origin(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Scheme + "://" + p.Host
}

// sameOrigin reports whether r comes from a page of the Hub's own, as
// the browser tells it: Sec-Fetch-Site, or else Origin against the host
// r was sent to. A request with neither isn't a browser's cross-site
// form.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// A page is what every page's frame needs.
type page struct {
	// Title is the page's, before "· Silkpool".
	Title string
	// Login is who's signed in, which the top bar shows, or empty on
	// pages before sign-in, which have none.
	Login string
	// Tab is the top bar's tab of the page, if any.
	Tab string
	// Live names the page's live parts, which its stream renders again
	// as the Hub's state changes: templates of the page's, each one
	// element whose id is its name. A page with none has no stream.
	Live []string
}

// framed is the data of a page in its frame, which embeds a page.
type framed interface{ frame() page }

func (p page) frame() page { return p }

// render writes the page name, filled with data, with status. For the
// page's stream, it hands the page over instead.
func (d *Dashboard) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	if v, ok := w.(*viewWriter); ok {
		v.view = view{status: status, name: name, data: data}
		return
	}
	var b bytes.Buffer
	if err := d.pages[name].ExecuteTemplate(&b, "frame", data); err != nil {
		d.failed(w, r, fmt.Errorf("render %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

// failed answers a failure the operator can't act on, logging it by its
// method and path (logPath).
func (d *Dashboard) failed(w http.ResponseWriter, r *http.Request, err error) {
	d.log.Error("the dashboard couldn't answer", "method", r.Method, "path", logPath(r), "err", err)
	http.Error(w, "The Hub couldn't answer: "+err.Error(), http.StatusInternalServerError)
}

// logPath is r's path for the dashboard's log: the path alone, since a
// query string can carry a code, and scrubbed (package secret) once it's
// decoded, since whoever sent r wrote it. Its method needs no scrubbing:
// only a route's GET, HEAD or POST reaches a handler that logs.
func logPath(r *http.Request) string { return secret.Scrub(r.URL.Path) }

// see redirects to path, after a change.
func see(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// refusalText returns err's words, as a sentence, and the status a page
// carrying it answers with: 422 for a refusal, 502 for a failure, which
// is GitHub's or the Hub's.
func refusalText(err error) (string, int) {
	if r, ok := errors.AsType[*hubops.Refusal](err); ok {
		return wording.Sentence(r.Message), http.StatusUnprocessableEntity
	}
	return wording.Sentence("the Hub couldn't do that: " + err.Error()), http.StatusBadGateway
}

// address returns the remote address of r, for history: the peer's, or,
// from a proxy on loopback, the last address it appended to
// X-Forwarded-For.
func address(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if f := r.Header.Values("X-Forwarded-For"); len(f) > 0 {
			parts := strings.Split(f[len(f)-1], ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
				return last
			}
		}
	}
	return host
}

// newToken returns a random token: 32 bytes, base64url.
func newToken() secret.Value { return secret.New(rand.Text() + rand.Text()) }

func (d *Dashboard) notFound(w http.ResponseWriter, r *http.Request) {
	d.render(w, r, http.StatusNotFound, "notfound", page{Title: "Not found"})
}

// parsePages parses each page over the shared templates and the frame.
func parsePages() map[string]*template.Template {
	base := template.Must(ui.Templates().Funcs(funcs).ParseFS(templateFiles, "templates/frame.tmpl"))
	names, err := fs.Glob(templateFiles, "templates/*.tmpl")
	if err != nil {
		panic(err)
	}
	pages := map[string]*template.Template{}
	for _, n := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".tmpl")
		if name == "frame" {
			continue
		}
		pages[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFiles, n))
	}
	return pages
}

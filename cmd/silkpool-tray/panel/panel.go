// Package panel is the tray app's panel (ADR 0009): the page the
// system web view shows when the tray or menu-bar icon is clicked, built
// from the shared UI package's parts (the state chip and sentence, the
// slot rows, the notices, and auto-resume with its editor) and laid out
// as "Prototype the tray and menu-bar panel" decided. The app serves it
// through Wails' Go asset handler, so nothing listens on TCP, and pushes
// the parts that change as Wails events, since WebView2 holds
// server-sent events back until their response ends.
//
// It shows what the tray reads of the agent, and sends its forms' commands
// to the agent through a Sender, the tray's link, as "Connect the tray
// app to the agent's control channel" asks. While it can't read the
// agent, it says why: the service isn't running, or the account isn't an
// administrator's.
//
// It doesn't import Wails, so its tests run in the default lane.
package panel

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/cli"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/ui"
	"github.com/meschoening/silkpool/internal/wording"
	"github.com/meschoening/silkpool/internal/zone"
)

//go:embed templates/*.tmpl
var templateFiles embed.FS

//go:embed assets
var assetFiles embed.FS

// Path is the panel's page, and the path its forms post under.
const Path = "/panel"

// Width is the panel's width, in pixels. Its height is its page's.
const Width = 340

// Parts are the panel's live parts, top to bottom: templates of the
// page, each one element whose id is its name, which a push carries
// when it has changed.
var Parts = []string{"panel-head", "panel-state", "panel-slots", "panel-auto-resume", "panel-actions", "panel-foot"}

var parsed = sync.OnceValue(func() *template.Template {
	return template.Must(ui.Templates().ParseFS(templateFiles, "templates/*.tmpl"))
})

// A Sender sends the panel's commands to the agent, as the tray's link,
// cli.Link, does: each returns the agent's reply, which holds its
// refusal, or why there's none.
type Sender interface {
	Command(ctx context.Context, e availability.Event) (control.Reply, error)
	Dismiss(ctx context.Context, notices []int64) (control.Reply, error)
}

// sendFor is how long a command from the panel may take before the panel
// gives up waiting for its reply.
const sendFor = 30 * time.Second

// saidFor is how long the panel says what came of a command that didn't
// apply.
const saidFor = time.Minute

// A Panel is the panel of one node, as the source last reported it: the
// page, and what has changed of it since the window last loaded it or
// was last sent its changes. Its methods are safe to call at once.
type Panel struct {
	os   string
	mac  bool
	host string
	now  func() time.Time
	send Sender
	mux  *http.ServeMux

	mu    sync.Mutex
	shown shown
	// have reports that the panel has read a status, which shown holds,
	// and down is why it can't read one now, or nil.
	have bool
	down error
	// query is what the window's page asked for as it loaded: the open
	// editor or confirmation.
	query url.Values
	// sent are the live parts as last sent, by name.
	sent map[string]string
	// said is what came of the panel's last command, if it didn't apply,
	// which it says until saidUntil.
	said      string
	saidUntil time.Time
}

// shown is the node a status reports, read as the panel shows it.
type shown struct {
	node                    node.Node
	name, osVersion, hubURL string
	// zone is the zone the panel's times print in, and own the machine's
	// own, by its IANA name.
	zone     *time.Location
	own      string
	hubSince time.Time
}

// New returns the panel of what the tray first read of the agent, on
// goos, on the machine named host, whose times read now, and which sends
// its commands through send.
func New(goos, host string, now func() time.Time, send Sender, first cli.Reading) (*Panel, error) {
	os := map[string]string{"darwin": "mac", "windows": "win"}[goos]
	p := &Panel{os: os, mac: goos == "darwin", host: shortName(host), now: now, send: send, query: url.Values{}, sent: map[string]string{}}
	if err := p.Show(first); err != nil {
		return nil, err
	}
	p.mux = p.routes()
	return p, nil
}

// Show makes the panel show what the tray read: the node its status
// reports, or why it has none.
func (p *Panel) Show(r cli.Reading) error {
	if r.Err != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.down = r.Err
		return nil
	}
	s, err := read(r.Status)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shown, p.have, p.down = s, true, nil
	return nil
}

// shortName is a host name without its domain.
func shortName(host string) string {
	name, _, _ := strings.Cut(host, ".")
	if name == "" {
		return "this machine"
	}
	return name
}

// downWords are the panel's words while it can't read the agent. p.mu
// must be held, and p.down set.
func (p *Panel) downWords() wording.TrayDown {
	var notAdmin control.NotAdmin
	var notServer control.NotServer
	switch {
	case errors.Is(p.down, control.ErrNotRunning):
		return wording.TrayNotRunning(p.mac)
	case errors.As(p.down, &notAdmin):
		return wording.TrayNotAdmin(p.mac, notAdmin.Account)
	case errors.As(p.down, &notServer):
		return wording.TrayNotTheService(notServer.Path)
	}
	return wording.TrayCantAsk(p.down)
}

// name is the machine's name, as its status last gave it, or its host
// name. p.mu must be held.
func (p *Panel) name() string {
	if p.have {
		return p.shown.name
	}
	return p.host
}

// read reads st as the panel shows it, as the node CLI reads it, but
// for the clock: the panel keeps its times current itself.
func read(st control.Status) (shown, error) {
	n, err := st.Node.ToNode()
	if err != nil {
		return shown{}, err
	}
	since, err := store.Unstamp(st.Hub.Since)
	if err != nil {
		return shown{}, err
	}
	// Before its schedule has a zone, as a new install's hasn't, the node
	// shows times in its OS's, or in Central if the agent found none.
	z := n.Availability.AutoResume.Zone
	if z == nil {
		z = zone.Own(st.Machine.Zone)
	}
	return shown{node: n, name: shortName(st.Machine.Hostname), osVersion: st.Machine.OSVersion, hubURL: st.Hub.URL, zone: z, own: st.Machine.Zone, hubSince: since}, nil
}

// Icon is the glyph of the node's state as the panel shows it, which the
// tray icon draws, and the icon's tooltip.
func (p *Panel) Icon() (glyph, tooltip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down != nil {
		return ui.GlyphLastKnown, wording.TrayTooltipDown(p.name(), p.downWords())
	}
	n := p.node()
	return n.Glyph(), n.TrayTooltip()
}

// node is the node shown, now. p.mu must be held.
func (p *Panel) node() ui.Node {
	s := p.shown
	return ui.Node{
		View: wording.View{Node: s.node, Name: s.name, Clock: wording.Clock{Now: p.now(), Zone: s.zone, Own: s.own},
			HubSince: s.hubSince, Smooth: true},
		Post: Path,
	}
}

// A page is the panel's page and its parts.
type page struct {
	// OS is "mac" or "win", for the fonts and the corners each OS has.
	OS string
	// Name is the machine's name.
	Name      string
	Node      ui.Node
	OSVersion string
	// Down is what the panel says while it can't read the agent, when it
	// shows nothing else.
	Down *wording.TrayDown
	// Said is what came of the panel's last command, if it didn't apply.
	Said string
	// Limit is the slot limit, and Fewer and More the limits the
	// stepper's buttons set, or 0 where it stops.
	Limit, Fewer, More int
	Revision           int64
	Verbs              []wording.Verb
	// Jobs reports that runners are holding jobs, which Stop now cancels.
	Jobs bool
	// Confirm reports that Stop now's confirmation is open, which
	// StopConfirm words.
	Confirm     bool
	StopConfirm string
	// Editor is auto-resume's editor, while it's open.
	Editor *ui.Editor
	// Hub is the footer's Hub line, and HubDetail what it means while the
	// Hub is offline. Dashboard is the Hub's dashboard.
	Hub, HubDetail, Dashboard string
}

// page is the panel's page with the editor or confirmation q opens. p.mu
// must be held.
func (p *Panel) page(q url.Values) page {
	if p.down != nil {
		d := p.downWords()
		pg := page{OS: p.os, Name: p.name(), Down: &d, Hub: d.Foot}
		if p.have {
			pg.OSVersion, pg.Dashboard = p.shown.osVersion, dashboard(p.shown.hubURL)
		}
		return pg
	}
	n := p.node()
	a := n.Node.Availability
	pg := page{
		OS: p.os, Name: n.Name, Node: n, OSVersion: p.shown.osVersion,
		Limit: a.SlotLimit, Revision: a.Revision, Verbs: n.Verbs(),
		Jobs: len(n.RunningJobs()) > 0, StopConfirm: n.StopConfirm(),
	}
	if p.now().Before(p.saidUntil) {
		pg.Said = p.said
	}
	if a.SlotLimit > 1 {
		pg.Fewer = a.SlotLimit - 1
	}
	if a.SlotLimit < availability.MaxSlotLimit {
		pg.More = a.SlotLimit + 1
	}
	pg.Hub, pg.HubDetail = n.TrayHub(p.shown.hubURL)
	pg.Dashboard = dashboard(p.shown.hubURL)
	pg.Confirm = q.Get("confirm") == "stop" && pg.Jobs && !n.Locked
	if q.Get("edit") == "auto-resume" && !n.Locked {
		// The draft keeps the revision the editor opened at, while pushes
		// render the panel again, so saving it after a change made
		// elsewhere is refused as stale.
		rev, err := strconv.ParseInt(q.Get("revision"), 10, 64)
		if err != nil {
			rev = a.Revision
		}
		pg.Editor = ui.EditorFor(n, a.AutoResume, rev)
	}
	return pg
}

// dashboard is the Hub's dashboard at hubURL, or empty if hubURL isn't a
// web address.
func dashboard(hubURL string) string {
	if u, err := url.Parse(hubURL); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		return u.String()
	}
	return ""
}

// render renders the template name of pg.
func render(name string, pg page) (string, error) {
	var b bytes.Buffer
	if err := parsed().ExecuteTemplate(&b, name, pg); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Changes returns the live parts that differ from what was last sent,
// one after another, or nothing if none does: at first, every part. The
// window's script morphs them in, so a part the page already has as
// sent changes nothing. Their words change with the node and with the
// clock alone, such as a job's elapsed time, so the app asks every
// second, as well as after every status.
func (p *Panel) Changes() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pg := p.page(p.query)
	var b strings.Builder
	for _, name := range Parts {
		html, err := render(name, pg)
		if err != nil {
			return "", err
		}
		if p.sent[name] != html {
			p.sent[name] = html
			b.WriteString(html)
		}
	}
	return b.String(), nil
}

// ServeHTTP serves the panel's page and the files it loads.
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.mux.ServeHTTP(w, r) }

// routes are the panel's: its page, the files it loads, and its forms'
// posts, each of which sends its command and answers with the panel.
func (p *Panel) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(ui.Assets())))
	own := http.FileServerFS(Assets())
	m.Handle("GET /panel.css", own)
	m.Handle("GET /panel.js", own)
	m.HandleFunc("GET "+Path, p.load)
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Path, http.StatusSeeOther)
	})
	m.HandleFunc("POST "+Path+"/command", p.command)
	m.HandleFunc("POST "+Path+"/slot-limit", p.slotLimit)
	m.HandleFunc("POST "+Path+"/auto-resume", p.autoResume)
	m.HandleFunc("POST "+Path+"/dismiss", p.dismiss)
	return m
}

// load serves the page, with the editor or confirmation its query opens,
// which the window now has. Later pushes render the same page. What was
// sent stays as it was: a push a load overtakes may still land after it,
// so the next one compares with what that push carried, and carries the
// parts that differ from now, to set the page right.
func (p *Panel) load(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r.URL.Query())
}

// answer answers a form's post with the panel, its editor and
// confirmation closed, as loading Path shows it, which the window's
// script morphs in, and names Path as where it's from, so the window
// shows it there. It isn't a redirect to Path: WebKit doesn't follow a
// redirect that Wails' handler answers on macOS, so the script got an
// empty answer and blanked the panel (#286).
func (p *Panel) answer(w http.ResponseWriter) {
	w.Header().Set("Content-Location", Path)
	p.serve(w, url.Values{})
}

// serve serves the page with the editor or confirmation q opens.
func (p *Panel) serve(w http.ResponseWriter, q url.Values) {
	p.mu.Lock()
	var b bytes.Buffer
	err := parsed().ExecuteTemplate(&b, "page", p.page(q))
	if err == nil {
		p.query = q
	}
	p.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

// Assets are the files the panel loads beside the shared UI package's:
// its layout and its script.
func Assets() fs.FS {
	sub, err := fs.Sub(assetFiles, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

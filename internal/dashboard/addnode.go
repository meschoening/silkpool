package dashboard

import (
	"net/http"
	"slices"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/wording"
)

// Add node, as variant A of "Prototype the Hub dashboard" has it, with
// the commands of "Define how each binary is packaged, installed,
// upgraded and signed per OS":
// "Make a code" shows the code, single use, with its expiry, and per OS
// the one command that installs Silkpool, pinned to the Hub's version,
// and enrolls with it, then the plain enroll for a machine that has
// Silkpool already. The page waits, live, until a machine uses the code.
// Below, the other codes, with Revoke for the open ones.
//
// The Hub gives a code's text out once, when it's made: the session that
// made it keeps it in memory, and shows it on this page while the code
// is open.

// An addNodePage is Add node.
type addNodePage struct {
	page
	// Made is the code this session made last, while the Hub has it.
	Made *madeView
	// OS is the tab open, by its runner label, and Tabs every tab.
	OS   string
	Tabs []osTab
	// Install and Enroll are the made code's commands for OS.
	Install, Enroll wording.EnrollCommand
	// Others are the Hub's other codes, newest first.
	Others  []codeRow
	Refusal string
}

// A madeView is the code this session made: its text and state.
type madeView struct {
	ID, Text string
	State    hubops.CodeState
	// Expires says when an open code expires, and Node is the node a used
	// one enrolled, at NodePath.
	Expires        string
	Node, NodePath string
}

// An osTab is one of Add node's OS tabs.
type osTab struct {
	Key, Title string
	On         bool
}

// A codeRow is one of the other codes: its ID, its state in words, when
// it was made, and whether it's open, so it can be revoked.
type codeRow struct {
	ID, Line, Made string
	Open           bool
}

// osTabs are Add node's tabs, by runner label.
var osTabs = []osTab{{Key: "windows", Title: "Windows"}, {Key: "macos", Title: "macOS"}, {Key: "linux", Title: "Linux"}}

// addNode serves Add node.
func (d *Dashboard) addNode(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	d.showAddNode(w, r, a, http.StatusOK, "")
}

// showAddNode serves Add node, with refusal, if a form was refused.
func (d *Dashboard) showAddNode(w http.ResponseWriter, r *http.Request, a access, status int, refusal string) {
	codes, err := d.ops.Codes(r.Context())
	if err != nil {
		d.failed(w, r, err)
		return
	}
	v, err := d.ops.Version(r.Context())
	if err != nil {
		d.failed(w, r, err)
		return
	}
	p := d.addNodePage(codes, v.Hub, a.made, r.URL.Query().Get("os"))
	p.Login, p.Refusal = a.who.Login, refusal
	d.render(w, r, status, "addnode", p)
}

// addNodePage builds Add node from the Hub's codes and version, the code
// this session made, and the OS tab asked for.
func (d *Dashboard) addNodePage(codes []hubops.Code, version string, made madeCode, os string) addNodePage {
	c := d.hubClock()
	p := addNodePage{page: page{Title: "Add node", Tab: "nodes", Live: []string{"add-node"}}, OS: "linux"}
	if slices.ContainsFunc(osTabs, func(t osTab) bool { return t.Key == os }) {
		p.OS = os
	}
	for _, t := range osTabs {
		t.On = t.Key == p.OS
		p.Tabs = append(p.Tabs, t)
	}
	for _, code := range slices.Backward(codes) {
		if code.ID == made.id {
			text := made.text.Reveal()
			p.Made = &madeView{ID: code.ID, Text: text, State: code.State, Expires: c.When(code.Expires), Node: code.Node, NodePath: nodePath(code.Node)}
			p.Install, p.Enroll = wording.Install(p.OS, version, text), wording.Enroll(p.OS, text)
			continue
		}
		p.Others = append(p.Others, codeRow{
			ID: code.ID, Line: c.Code(wording.CodeState(code.State), code.Expires, code.Node),
			Made: "made " + c.When(code.Made), Open: code.State == hubops.Open,
		})
	}
	return p
}

// makeCode makes an enrollment code, which this session keeps to show.
func (d *Dashboard) makeCode(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	c, err := d.ops.MakeCode(r.Context(), a.origin())
	if err != nil {
		text, status := refusalText(err)
		d.showAddNode(w, r, a, status, text)
		return
	}
	d.sessions.keep(cookie(r, sessionCookie), madeCode{id: c.ID, text: c.Text})
	see(w, r, "/nodes/add")
}

// revokeCode revokes one of the open codes.
func (d *Dashboard) revokeCode(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	if _, err := d.ops.RevokeCode(r.Context(), a.origin(), r.PostFormValue("code")); err != nil {
		text, status := refusalText(err)
		d.showAddNode(w, r, a, status, text)
		return
	}
	see(w, r, "/nodes/add")
}

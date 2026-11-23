package dashboard

import (
	"errors"
	"net/http"
	"strings"

	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wording"
)

// A node's diagnostics report, from its page: the page's Diagnostics
// part asks for it, and the report's own page shows it as the node CLI
// prints it, part by part in package wording's words, then the end of
// the agent's own log. The Hub asks the node when the page is opened,
// and keeps nothing, so the page has no live parts, and opening it again
// asks again. Asking changes nothing, so it's a GET.
//
// The agent scrubs the report before it sends it (package diagnostics).
// The page scrubs what it shows again, so a secret in a report from an
// agent that didn't still never reaches the page.

// A diagnosticsPage is a node's diagnostics report, or why there's
// none.
type diagnosticsPage struct {
	page
	// Back is the node's page, and Ask this one's.
	Name, Back, Ask string
	// Kept says the Hub doesn't keep the report.
	Kept string
	// Parts are the report's, and Log the end of the agent's own log,
	// under LogHeading.
	Parts      []wording.Part
	LogHeading string
	Log        []string
	// Refusal is why there's no report. Gone reports that the node has
	// left the Hub, so there's no page to go back to, or to ask again
	// from.
	Refusal string
	Gone    bool
}

// diagnosticsPath is the path of the diagnostics report of the node
// named name.
func diagnosticsPath(name string) string { return nodePath(name) + "/diagnostics" }

// diagnostics asks a node for its diagnostics report, and shows it, or
// the refusal: for a node the Hub doesn't have, with 404.
func (d *Dashboard) diagnostics(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	f, err := d.ops.Fleet(r.Context())
	if err != nil {
		d.failed(w, r, err)
		return
	}
	name := r.PathValue("name")
	det, ok := f.Find(name)
	if !ok {
		// As from a link to a node since removed: the refusal, by the
		// name asked for.
		p := diagnosticsPageOf(hubops.Detail{Node: hubops.Node{Name: name}})
		p.Login = a.who.Login
		p.Refusal, p.Gone = wording.Sentence("the Hub has no node named "+name), true
		d.render(w, r, http.StatusNotFound, "diagnostics", p)
		return
	}
	p := diagnosticsPageOf(det)
	p.Login = a.who.Login
	if wantsStream(r) {
		// The page has no stream, which its answer says without asking
		// the node.
		d.render(w, r, http.StatusOK, "diagnostics", p)
		return
	}
	rep, err := d.ops.Diagnostics(r.Context(), det.ID)
	status := p.answer(rep, err)
	d.render(w, r, status, "diagnostics", p)
}

// diagnosticsPageOf is the page of det's diagnostics report, before the
// node answers.
func diagnosticsPageOf(det hubops.Detail) diagnosticsPage {
	return diagnosticsPage{
		page: page{Title: det.Name + " diagnostics", Tab: "nodes"},
		Name: det.Name, Back: nodePath(det.Name), Ask: diagnosticsPath(det.Name), Kept: wording.DiagnosticsKept,
	}
}

// answer puts the node's answer on p: its report, rep, or err, why
// there's none, as the refusal itself. It returns the status the page
// answers with.
func (p *diagnosticsPage) answer(rep diagnostics.Report, err error) int {
	if err != nil {
		var status int
		p.Refusal, status = refusalText(err)
		if ref, ok := errors.AsType[*hubops.Refusal](err); ok && ref.Code == "unknown-node" {
			p.Gone, status = true, http.StatusNotFound
		}
		return status
	}
	for _, part := range wording.Diagnostics(rep) {
		lines := make([]string, len(part.Lines))
		for i, l := range part.Lines {
			lines[i] = secret.Scrub(l)
		}
		p.Parts = append(p.Parts, wording.Part{Label: part.Label, Lines: lines})
	}
	if len(rep.Log) > 0 {
		// The lines are scrubbed together, as the agent does, so a
		// private key block over several is found.
		p.Log = strings.Split(secret.Scrub(strings.Join(rep.Log, "\n")), "\n")
	}
	p.LogHeading = wording.LogHeading(len(p.Log))
	return http.StatusOK
}

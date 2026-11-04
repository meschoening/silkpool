package dashboard

import (
	"net/http"
	"strings"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/wording"
)

// Targets, as variant A of "Prototype the Hub dashboard" has it, with
// the rules of "Define the registration scope model and labels":
// each target's kind, App and nodes, the default for new nodes, Remove,
// refused while a node uses it, an organization target's repositories,
// and a runs-on line per OS and architecture among its nodes. "Add a
// target…" lists what each GitHub connection offers, with what Silkpool
// can't serve shown disabled and why. The pool label follows, with its
// rules. The page isn't live: an organization target's group is read
// from GitHub each time it's shown.

// A targetsPage is Targets.
type targetsPage struct {
	page
	Targets   []targetRow
	PoolLabel string
	// Adding reports "Add a target…" open, with what each connection
	// offers, those GitHub didn't list, and the target picked.
	Adding bool
	Offers []offer
	Failed []string
	Pick   string
	// Renaming reports the pool label's Rename open, with Draft in it.
	Renaming bool
	Draft    string
	// Refusal is why the last form was refused, shown where RefusedIn
	// says: a target's key, "add" or "pool".
	Refusal   string
	RefusedIn string
}

// A targetRow is one target.
type targetRow struct {
	Key, Title string
	// Meta is its kind, its App, and the nodes that use it.
	Meta    string
	Default bool
	// Serves is what an organization target's group serves, or why
	// GitHub didn't say, in GroupFailed.
	Serves, GroupFailed string
	Lost                string
	RunsOn              []string
}

// An offer is what one GitHub connection offers as targets.
type offer struct {
	Account    string
	Candidates []candidate
}

// targets serves Targets.
func (d *Dashboard) targets(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	p := targetsPage{Adding: q.Has("add"), Renaming: q.Has("rename")}
	d.showTargets(w, r, a, http.StatusOK, p)
}

// showTargets serves Targets, as p opens its forms.
func (d *Dashboard) showTargets(w http.ResponseWriter, r *http.Request, a access, status int, p targetsPage) {
	ctx := r.Context()
	list, err := d.ops.Targets(ctx)
	if err != nil {
		d.failed(w, r, err)
		return
	}
	st, err := d.ops.Status(ctx)
	if err != nil {
		d.failed(w, r, err)
		return
	}
	var av *hubops.Available
	if p.Adding {
		offered, err := d.ops.Available(ctx)
		if err != nil {
			d.failed(w, r, err)
			return
		}
		av = &offered
	}
	d.fillTargets(&p, list, st.Connections, av)
	p.Login = a.who.Login
	d.render(w, r, status, "targets", p)
}

// fillTargets fills p in from the Hub's targets, its connections, and
// what they offer, if "Add a target…" is open.
func (d *Dashboard) fillTargets(p *targetsPage, list hubops.TargetList, conns []hubops.Connection, av *hubops.Available) {
	apps := map[string]string{}
	for _, c := range conns {
		apps[c.Account] = c.App
	}
	p.page = page{Title: "Targets", Tab: "targets"}
	p.PoolLabel = list.PoolLabel
	if p.Renaming && p.Draft == "" {
		p.Draft = list.PoolLabel
	}
	c := d.hubClock()
	for _, t := range list.Targets {
		p.Targets = append(p.Targets, targetRowOf(t, apps[t.Connection], c))
	}
	if av == nil {
		return
	}
	for _, cand := range av.Candidates {
		if len(p.Offers) == 0 || p.Offers[len(p.Offers)-1].Account != cand.Connection {
			p.Offers = append(p.Offers, offer{Account: cand.Connection})
		}
		o := &p.Offers[len(p.Offers)-1]
		o.Candidates = append(o.Candidates, candidateOf(cand))
	}
	for _, f := range av.Failed {
		p.Failed = append(p.Failed, wording.Sentence(f.Message))
	}
}

// targetRowOf returns how Targets shows t, served through app.
func targetRowOf(t hubops.Target, app string, c wording.Clock) targetRow {
	row := targetRow{Key: t.Target, Title: t.Target, Default: t.Default, RunsOn: t.RunsOn, GroupFailed: wording.Sentence(t.GroupFailed)}
	used := "no nodes yet"
	if len(t.Nodes) > 0 {
		used = "used by " + wording.Nodes(t.Nodes)
	}
	kind := "Repository"
	if t.Kind == hubops.Organization {
		kind = "Organization runner group"
		org, group, _ := strings.Cut(t.Target, ":")
		if g := t.Group; g != nil {
			group = g.Name
			row.Serves = "Serves " + wording.Serves(t.Connection, g.Visibility, g.Repositories) + ", as the group's repository access allows."
		}
		if group == "" {
			group = "Default"
		}
		row.Title = org + " · " + group + " group"
	}
	row.Meta = kind + " · " + app + " · " + used
	if !t.Lost.IsZero() {
		row.Lost = wording.TargetLost(c, t.Lost)
	}
	return row
}

// addTarget adds the target picked in "Add a target…".
func (d *Dashboard) addTarget(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	pick := r.PostFormValue("target")
	var err error = refusal("no-target", "choose a target first")
	if pick != "" {
		_, err = d.ops.AddTarget(r.Context(), a.origin(), pick)
	}
	d.targetAnswer(w, r, a, err, targetsPage{Adding: true, Pick: pick, RefusedIn: "add"})
}

// defaultTarget makes a target the default for new nodes.
func (d *Dashboard) defaultTarget(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	key := r.PostFormValue("target")
	_, err := d.ops.SetDefaultTarget(r.Context(), a.origin(), key)
	d.targetAnswer(w, r, a, err, targetsPage{RefusedIn: key})
}

// removeTarget removes a target, which is refused while a node uses it.
func (d *Dashboard) removeTarget(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	key := r.PostFormValue("target")
	_, err := d.ops.RemoveTarget(r.Context(), a.origin(), key)
	d.targetAnswer(w, r, a, err, targetsPage{RefusedIn: key})
}

// poolLabel renames the pool label.
func (d *Dashboard) poolLabel(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	label := r.PostFormValue("label")
	_, err := d.ops.SetPoolLabel(r.Context(), a.origin(), label)
	d.targetAnswer(w, r, a, err, targetsPage{Renaming: true, Draft: label, RefusedIn: "pool"})
}

// targetAnswer answers a form of Targets: on to Targets after a change,
// or the page again, open as p has it, with the refusal where p says.
func (d *Dashboard) targetAnswer(w http.ResponseWriter, r *http.Request, a access, err error, p targetsPage) {
	if err == nil {
		see(w, r, "/targets")
		return
	}
	var status int
	p.Refusal, status = refusalText(err)
	d.showTargets(w, r, a, status, p)
}

package dashboard

import (
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/ui"
	"github.com/meschoening/silkpool/internal/wording"
)

// The fleet's pages, as variant A of "Prototype the Hub dashboard"
// has them: Nodes, a table with one row per node, and each node's own
// page, with its state, slots and commands on the left, and auto-resume,
// what it runs jobs for and its removal on the right. Both are live:
// node reports and command replies push their parts again, and the
// words that move with the clock, such as the gap between jobs, follow
// it.
//
// Commands go through package hubops, from the dashboard and the
// signed-in login, and the page never runs ahead of the node: a command
// shows as sending until the node replies, as hubops.Command follows
// it. While a node isn't connected, every command control is disabled,
// and the Hub's own settings for it stay editable.

// appliedFor is how long a command's "Applied" line shows.
const appliedFor = 5 * time.Second

// fleetAccess returns who r comes from if they may see the fleet: the
// operator, signed in, once setup is done. Anyone else is sent to setup
// or to sign in.
func (d *Dashboard) fleetAccess(w http.ResponseWriter, r *http.Request) (access, bool) {
	a, err := d.access(r.Context(), r)
	switch {
	case err != nil:
		d.failed(w, r, err)
	case !a.signedIn() && a.progress.Operator == 0:
		see(w, r, hubops.SetupPath)
	case !a.signedIn():
		see(w, r, "/sign-in")
	case !a.progress.Finished():
		see(w, r, hubops.SetupPath)
	default:
		return a, true
	}
	return access{}, false
}

// nodePath is the path of the page of the node named name.
func nodePath(name string) string { return "/nodes/" + url.PathEscape(name) }

// clock returns the clock a node's words print times in: its own
// auto-resume zone, named when its clock reads other than the Hub's.
func (d *Dashboard) clock(zone *time.Location) wording.Clock {
	now := d.now()
	if zone == nil {
		zone = now.Location()
	}
	c := wording.Clock{Now: now, Zone: zone}
	_, ours := now.Zone()
	if _, theirs := now.In(zone).Zone(); ours == theirs {
		c.Own = zone.String()
	}
	return c
}

// hubClock is the clock the Hub's own times print in: its own zone.
func (d *Dashboard) hubClock() wording.Clock {
	now := d.now()
	return wording.Clock{Now: now, Zone: now.Location(), Own: now.Location().String()}
}

// joined is a node as it joins, which the dashboard shows of a node
// that hasn't reported yet: Paused indefinitely, with one free slot.
var joined = func() node.Node {
	n, err := node.New(time.UTC)
	if err != nil {
		panic(err)
	}
	return n
}()

// shown returns the node d describes as the shared parts show it, with
// the 10 s smoothing between jobs. A node that isn't connected shows
// its last report.
func (d *Dashboard) shown(det hubops.Detail, locked bool) ui.Node {
	m := det.Mirror
	if !m.Enrolled {
		m = joined
	}
	m.Connected = det.Connected
	since := det.ConnectedAt
	if !det.Connected {
		since = det.Seen
	}
	return ui.Node{
		View:      wording.View{Node: m, Name: det.Name, Clock: d.clock(m.Availability.AutoResume.Zone), HubSince: since, Smooth: true},
		LastKnown: !det.Connected, Since: det.Seen, Post: nodePath(det.Name), Locked: locked,
	}
}

// age is how long the node has shown its state, or, while it isn't
// connected, how long ago it was last seen.
func age(n ui.Node, det hubops.Detail) string {
	switch {
	case det.Connected:
		return n.For()
	case det.Seen.IsZero():
		return "Not connected yet"
	}
	return n.Clock.LastKnown(det.Seen)
}

// hubNotices are the Hub's own notices about the node, in words.
func hubNotices(n ui.Node, det hubops.Detail) []wording.NoticeText {
	var out []wording.NoticeText
	for _, x := range det.Notices {
		switch x.Kind {
		case pool.NoticeKeyInUse:
			out = append(out, wording.KeyInUse(det.Name))
		case pool.NoticeNoTarget:
			out = append(out, wording.NoTarget(det.Name))
		case pool.NoticeVersionRefused:
			out = append(out, wording.VersionRefused(n.Clock, x.Since, x.Detail))
		}
	}
	return out
}

// A nodesPage is Nodes, the fleet view: the pool in a sentence, and a
// row per node.
type nodesPage struct {
	page
	Pool string
	Rows []nodeRow
}

// A nodeRow is a node's row in the Nodes table.
type nodeRow struct {
	Node ui.Node
	ID   string
	Path string
	// Platform is its OS and architecture, or unknown, once it has
	// connected.
	Platform string
	Notices  int
	Removing bool
	// Age follows the state word: how long it has shown it, or how long
	// ago a node that isn't connected was last seen.
	Age string
	// Pips are a class per slot: busy, listening, amber while failing,
	// moving while in between, or none while free. Busy of Limit slots
	// run jobs.
	Pips        []string
	Busy, Limit int
	Doing       wording.Doing
	// Resume and Days are auto-resume's time and days while it's on.
	Resume, Days string
	Target       string
}

// home serves Nodes, the fleet view.
func (d *Dashboard) home(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	f, err := d.ops.Fleet(r.Context())
	if err != nil {
		d.failed(w, r, err)
		return
	}
	p := d.nodesPage(f)
	p.Login = a.who.Login
	d.render(w, r, http.StatusOK, "nodes", p)
}

// nodesPage builds Nodes from f.
func (d *Dashboard) nodesPage(f hubops.Fleet) nodesPage {
	p := nodesPage{page: page{Title: "Nodes", Tab: "nodes", Live: []string{"nodes"}}}
	var views []wording.View
	for _, det := range f.Nodes {
		row := d.row(det)
		p.Rows = append(p.Rows, row)
		views = append(views, row.Node.View)
	}
	p.Pool = wording.Pool(views)
	return p
}

// row returns det's row in the Nodes table.
func (d *Dashboard) row(det hubops.Detail) nodeRow {
	n := d.shown(det, false)
	m := n.Node
	row := nodeRow{
		Node: n, ID: det.ID, Path: nodePath(det.Name), Platform: det.Platform(),
		Notices: len(n.Notices()) + len(det.Notices), Removing: det.Removing, Age: age(n, det),
		Limit: m.Availability.SlotLimit, Doing: n.Doing(), Target: det.Target,
	}
	for _, s := range m.Slots {
		run := s.Runner
		pip := "is-moving"
		switch {
		case s.Degraded:
			pip = "is-amber"
		case s.Phase == node.Hosting && run.Job != "":
			pip = "is-busy"
			row.Busy++
		case s.Phase == node.Hosting && run.Listening && run.Retirement == node.NotRetiring:
			pip = "is-listening"
		case s.Phase == node.Free:
			pip = ""
		}
		row.Pips = append(row.Pips, pip)
	}
	if a := m.Availability.AutoResume; a.On {
		row.Resume, row.Days = wording.Hour(a.Time), wording.Days(a.Days)
	}
	return row
}

// A nodePage is a node's own page.
type nodePage struct {
	page
	Node     ui.Node
	Platform string
	Age      string
	Removing bool
	// HubNotices are the Hub's own notices about the node.
	HubNotices []wording.NoticeText
	// Limit is the slot limit, and Fewer and More the limits the
	// stepper's buttons set, or zero where it can't go.
	Limit, Fewer, More int
	Revision           int64
	// Budget is the Budget row, under the slots.
	Budget budgetRow
	Verbs  []wording.Verb
	// Status is where its last command is, and Pending reports one sent
	// and not yet settled, which locks the controls. Sending reports one
	// waiting for its reply, whose line turns.
	Status           string
	Pending, Sending bool
	// Jobs reports jobs running, which Stop now cancels.
	Jobs bool
	// Confirm is the confirmation open: "stop" or "remove".
	Confirm                    string
	StopConfirm, RemoveConfirm string
	// Editor is auto-resume's editor, while it's open.
	Editor *ui.Editor
	// Renaming reports Rename open, with Draft in it.
	Renaming bool
	Draft    string
	// Target is its target, and Targets those it can have.
	Target    string
	Targets   []string
	PoolLabel string
	OS, Arch  string
	Labels    []string
	// SelfHosted warns of the self-hosted label, if it has it.
	SelfHosted string
	RunsOn     string
	// NoRunsOn says why it has no runs-on line, if it's connected
	// without one.
	NoRunsOn string
	// Refusal is why the last form was refused, shown in the part named
	// RefusedIn: "commands", "auto-resume", "settings" or "remove".
	Refusal   string
	RefusedIn string
}

// A budgetRow is the node page's Budget row: a stepper for each half,
// bounded by the machine's size the node reports and able to go down to
// none, and the budget in words, or, on a Mac, which has no budget, no
// control at all.
type budgetRow struct {
	Mac          bool
	Memory, CPUs stepper
	Line         string
}

// A stepper is one half of the budget's stepper: what it shows, and the
// values its buttons post, or "" where it can't go.
type stepper struct {
	Shown, Fewer, More string
}

// steps returns the stepper of a half at set, which can go from none, 0,
// up to most, showing it with unit.
func steps(set, most int64, unit string) stepper {
	s := stepper{Shown: "none"}
	if set > 0 {
		s.Shown = strconv.FormatInt(set, 10) + unit
	}
	switch {
	case set == 1:
		s.Fewer = "none"
	case set > 1:
		s.Fewer = strconv.FormatInt(set-1, 10)
	}
	if set < most {
		s.More = strconv.FormatInt(set+1, 10)
	}
	return s
}

// budgetOf returns det's Budget row from m, its report.
func budgetOf(det hubops.Detail, n ui.Node) budgetRow {
	m := n.Node
	if m.Machine.MacOS || det.OS == "macos" {
		return budgetRow{Mac: true}
	}
	b := m.Availability.Budget
	return budgetRow{
		Memory: steps(b.Memory/availability.GB, m.Machine.RAM/availability.GB, " GB"),
		CPUs:   steps(int64(b.CPUs), int64(m.Machine.CPUs), ""),
		Line:   n.Budget(),
	}
}

// The node page's live parts.
var nodeParts = []string{"node-head", "node-state", "node-auto-resume", "node-settings", "node-remove"}

// An answer is what a form's answer adds to the node's page: why it was
// refused, and the form it was posted from, open as it was.
type answer struct {
	refusal, in string
	status      int
	// draft is auto-resume's editor's draft, based on revision, and name
	// Rename's.
	draft    *availability.AutoResume
	revision int64
	name     *string
}

// node serves a node's page.
func (d *Dashboard) node(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	d.showNode(w, r, a, r.PathValue("name"), answer{status: http.StatusOK})
}

// showNode serves the page of the node named name, with ans. A node the
// Hub doesn't have, or no longer has, isn't found.
func (d *Dashboard) showNode(w http.ResponseWriter, r *http.Request, a access, name string, ans answer) {
	f, err := d.ops.Fleet(r.Context())
	if err != nil {
		d.failed(w, r, err)
		return
	}
	det, ok := f.Find(name)
	if !ok {
		d.notFound(w, r)
		return
	}
	p := d.nodePage(f, det, r.URL.Query())
	p.Login = a.who.Login
	p.Refusal, p.RefusedIn = ans.refusal, ans.in
	if ans.draft != nil {
		p.Editor = d.editor(p.Node, *ans.draft, ans.revision)
	}
	if ans.name != nil {
		p.Renaming, p.Draft = true, *ans.name
	}
	d.render(w, r, ans.status, "node", p)
}

// nodePage builds det's page, with the forms q opens.
func (d *Dashboard) nodePage(f hubops.Fleet, det hubops.Detail, q url.Values) nodePage {
	n := d.shown(det, false)
	status, pending := d.status(n, det.Command)
	n.Locked = !det.Connected || det.Removing || pending
	m := n.Node
	limit := m.Availability.SlotLimit
	p := nodePage{
		page: page{Title: det.Name, Tab: "nodes", Live: nodeParts},
		Node: n, Platform: det.Platform(), Age: age(n, det), Removing: det.Removing,
		HubNotices: hubNotices(n, det), Limit: limit, Revision: m.Availability.Revision, Budget: budgetOf(det, n), Verbs: n.Verbs(),
		Status: status, Pending: pending, Sending: det.Command != nil && det.Command.State == hubops.CommandSending,
		Jobs:        len(n.RunningJobs()) > 0,
		StopConfirm: n.StopConfirm(), RemoveConfirm: wording.HubRemoveConfirm(det.Name, det.Connected, n.RunningJobs()),
		Target: det.Target, Targets: f.Targets, PoolLabel: f.PoolLabel, OS: det.OS, Arch: det.Arch,
		Labels: det.Labels, RunsOn: det.RunsOn,
	}
	if det.UnknownPlatform {
		p.NoRunsOn = wording.UnknownRunsOn
	}
	if limit > 1 {
		p.Fewer = limit - 1
	}
	if limit < availability.MaxSlotLimit {
		p.More = limit + 1
	}
	if slices.Contains(det.Labels, "self-hosted") {
		p.SelfHosted = wording.SelfHosted(det.Name)
	}
	switch c := q.Get("confirm"); {
	case c == "stop" && p.Jobs && det.Connected:
		p.Confirm = c
	case c == "remove" && !det.Removing:
		p.Confirm = c
	}
	if q.Get("edit") == "auto-resume" && !n.Locked {
		// The draft keeps the revision the editor opened at, while the
		// stream renders the page again, so saving it after a change
		// made elsewhere is refused as stale.
		rev, err := strconv.ParseInt(q.Get("revision"), 10, 64)
		if err != nil {
			rev = m.Availability.Revision
		}
		p.Editor = d.editor(n, m.Availability.AutoResume, rev)
	}
	if q.Has("rename") && !det.Removing {
		p.Renaming, p.Draft = true, det.Name
	}
	return p
}

// status is the line that says where c, n's last command, is, and
// whether it's still to settle. An applied command's line shows for
// appliedFor; a refusal's stays.
func (d *Dashboard) status(n ui.Node, c *hubops.Command) (string, bool) {
	if c == nil {
		return "", false
	}
	recent := d.now().Sub(c.Settled) < appliedFor
	switch c.State {
	case hubops.CommandSending:
		return n.Sending(string(c.Kind)), true
	case hubops.CommandNotConfirmed:
		return n.NotConfirmed(), true
	case hubops.CommandRefused:
		return n.RefusedBy(c.Reply.Refused), false
	case hubops.CommandNotApplied:
		return n.NotApplied(), false
	case hubops.CommandApplied:
		if recent {
			return n.Applied(), false
		}
	case hubops.CommandConfirmed:
		if recent {
			return n.Confirmed(), false
		}
	}
	return "", false
}

// zones are the time zones auto-resume's editor offers, beside the
// node's own.
var zones = []string{
	"UTC", "America/Los_Angeles", "America/Denver", "America/Chicago", "America/New_York", "America/Sao_Paulo",
	"Europe/London", "Europe/Berlin", "Europe/Helsinki", "Africa/Johannesburg", "Asia/Kolkata", "Asia/Singapore",
	"Asia/Tokyo", "Australia/Sydney", "Pacific/Auckland",
}

// editor returns auto-resume's editor for n, open on draft, based on
// revision.
func (d *Dashboard) editor(n ui.Node, draft availability.AutoResume, revision int64) *ui.Editor {
	offered := zones
	if z := draft.Zone; z != nil && !slices.Contains(zones, z.String()) {
		offered = append([]string{z.String()}, zones...)
	}
	return &ui.Editor{Post: n.Post, Draft: draft, Preview: n.DraftPreview(draft), Zones: offered, Revision: revision}
}

// answered answers a node's form: on to the node's page at path after a
// change, or the page again with the refusal in its part.
func (d *Dashboard) answered(w http.ResponseWriter, r *http.Request, a access, path, name string, err error, ans answer) {
	if err == nil {
		see(w, r, path)
		return
	}
	if ref, ok := errors.AsType[*hubops.Refusal](err); ok && ref.Code == "unknown-node" {
		d.notFound(w, r)
		return
	}
	ans.refusal, ans.status = refusalText(err)
	d.showNode(w, r, a, name, ans)
}

// command sends one of the node's verbs, or Emergency stop.
func (d *Dashboard) command(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var e availability.Event
	switch r.PostFormValue("verb") {
	case "pause":
		e = availability.Pause{}
	case "pause-indefinitely":
		e = availability.PauseIndefinitely{}
	case "resume":
		e = availability.Resume{Base: revision(r)}
	case "stop":
		e = availability.EmergencyStop{}
	default:
		http.Error(w, "That isn't one of a node's commands.", http.StatusBadRequest)
		return
	}
	_, err := d.ops.Command(r.Context(), a.origin(), name, e)
	d.answered(w, r, a, nodePath(name), name, err, answer{in: "commands"})
}

// revision is the node's revision the form was read at.
func revision(r *http.Request) int64 {
	v, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	return v
}

// slotLimit sets the slot limit the stepper's button gives.
func (d *Dashboard) slotLimit(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	limit, err := strconv.Atoi(r.PostFormValue("limit"))
	if err == nil {
		_, err = d.ops.Command(r.Context(), a.origin(), name, availability.SetSlotLimit{Base: revision(r), To: limit})
	} else {
		err = refusal("invalid-limit", "the slot limit is a number")
	}
	d.answered(w, r, a, nodePath(name), name, err, answer{in: "commands"})
}

// budget sets the half of the budget a stepper's button gives: memory,
// in GB, or CPUs, a whole number or none. Raising or clearing a half
// needs the revision the page showed, as for the slot limit.
func (d *Dashboard) budget(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	e := availability.SetBudget{Base: revision(r)}
	memory, cpus := r.PostFormValue("memory"), r.PostFormValue("cpus")
	var err error
	switch {
	case memory != "" && cpus == "":
		var gb int64
		gb, err = half(memory)
		e.SetMemory, e.Memory = true, gb*availability.GB
	case cpus != "" && memory == "":
		var n int64
		n, err = half(cpus)
		e.SetCPUs, e.CPUs = true, int(n)
	default:
		err = refusal("invalid-budget", "set the memory or the CPUs")
	}
	if err == nil {
		_, err = d.ops.Command(r.Context(), a.origin(), name, e)
	}
	d.answered(w, r, a, nodePath(name), name, err, answer{in: "commands"})
}

// half reads one half of the budget from a stepper: a whole number from
// 1, or none, which is 0.
func half(v string) (int64, error) {
	if v == "none" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 || n > 1<<20 {
		return 0, refusal("invalid-budget", "a budget is a whole number, or none")
	}
	return n, nil
}

// autoResume saves auto-resume, from its switch or its editor. Turning
// it off and changing nothing else is the off-only edit, which always
// applies; anything else needs the revision the form was read at.
func (d *Dashboard) autoResume(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	draft, err := parseAutoResume(r)
	if err != nil {
		d.answered(w, r, a, nodePath(name), name, err, answer{in: "auto-resume", draft: &draft, revision: revision(r)})
		return
	}
	var e availability.Event = availability.EditAutoResume{Base: revision(r), To: draft}
	if f, err := d.ops.Fleet(r.Context()); err == nil {
		if det, ok := f.Find(name); ok && !draft.On && sameSchedule(draft, det.Mirror.Availability.AutoResume) {
			e = availability.TurnOffAutoResume{}
		}
	}
	_, err = d.ops.Command(r.Context(), a.origin(), name, e)
	d.answered(w, r, a, nodePath(name), name, err, answer{in: "auto-resume"})
}

// sameSchedule reports whether a and b resume at the same times.
func sameSchedule(a, b availability.AutoResume) bool {
	return a.Time == b.Time && a.Days == b.Days && a.Zone.String() == b.Zone.String()
}

// parseAutoResume reads auto-resume's settings from the form: on, the
// time as HH:MM, each day as its number, and the zone by its IANA name.
// What it can read comes back with the refusal.
func parseAutoResume(r *http.Request) (availability.AutoResume, error) {
	var out availability.AutoResume
	_ = r.ParseForm()
	out.On = r.PostForm.Get("on") != ""
	for _, v := range r.PostForm["days"] {
		if day, err := strconv.Atoi(v); err == nil && day >= 0 && day <= 6 {
			out.Days |= availability.DaysOf(time.Weekday(day))
		}
	}
	t, err := time.Parse("15:04", r.PostForm.Get("time"))
	if err != nil {
		return out, refusal("invalid-time", "write the time as HH:MM, such as 07:30")
	}
	out.Time = availability.TimeOfDay{Hour: t.Hour(), Minute: t.Minute()}
	zone := r.PostForm.Get("zone")
	z, err := time.LoadLocation(zone)
	if err != nil || zone == "" || zone == "Local" {
		return out, refusal("invalid-zone", "choose a time zone")
	}
	out.Zone = z
	return out, nil
}

// dismiss dismisses one of the node's notices.
func (d *Dashboard) dismiss(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	id, err := strconv.ParseInt(r.PostFormValue("notice"), 10, 64)
	if err == nil {
		_, err = d.ops.Dismiss(r.Context(), a.origin(), name, id)
	} else {
		err = refusal("invalid-notice", "that isn't one of the node's notices")
	}
	d.answered(w, r, a, nodePath(name), name, err, answer{in: "commands"})
}

// rename renames the node, on to its page under the new name.
func (d *Dashboard) rename(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name, to := r.PathValue("name"), strings.TrimSpace(r.PostFormValue("name"))
	c, err := d.ops.Rename(r.Context(), a.origin(), name, to)
	d.answered(w, r, a, nodePath(c.Node.Name), name, err, answer{in: "settings", name: &to})
}

// retarget gives the node the target picked.
func (d *Dashboard) retarget(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	_, err := d.ops.SetTarget(r.Context(), a.origin(), name, r.PostFormValue("target"))
	d.answered(w, r, a, nodePath(name), name, err, answer{in: "settings"})
}

// addLabel gives the node a custom label.
func (d *Dashboard) addLabel(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	_, err := d.ops.AddLabels(r.Context(), a.origin(), name, []string{r.PostFormValue("label")})
	d.answered(w, r, a, nodePath(name), name, err, answer{in: "settings"})
}

// removeLabel takes a custom label from the node.
func (d *Dashboard) removeLabel(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	_, err := d.ops.RemoveLabels(r.Context(), a.origin(), name, []string{r.PostFormValue("label")})
	d.answered(w, r, a, nodePath(name), name, err, answer{in: "settings"})
}

// remove starts removing the node: on to its page while a connected
// node finishes, or to Nodes once it's gone.
func (d *Dashboard) remove(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	c, err := d.ops.RemoveNode(r.Context(), a.origin(), name, rand.Text())
	path := nodePath(name)
	if !c.Node.Removed.IsZero() {
		path = "/"
	}
	d.answered(w, r, a, path, name, err, answer{in: "remove"})
}

package dashboard

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/wording"
)

// Activity, as variant A of "Prototype the Hub dashboard" has it, with
// the filters of "Define Silkpool's events, audit trail, notices and
// redaction": every entry of the Hub's history, newest first and
// grouped by day, each with its time, its node, what happened and its
// origin. An entry a node delivered after it reconnected keeps its own
// time and carries the ↓ late mark. A filter shows one group of kinds,
// and a node's name shows only that node's history, which its own page
// links. The list is live, and a page of it ends with a link to older
// entries.
//
// A node's page and Nodes list history the same way, through events:
// the node's newest entries, and the pool's most recent.

// An activityPage is Activity.
type activityPage struct {
	page
	Filters []activityFilter
	// Node is the name of the node whose history it shows, or empty for
	// every node's and the Hub's own, and Every Activity's path for
	// every node's, under the same filter.
	Node, Every string
	Events      events
	// Empty is what the list says when it has no entries.
	Empty string
	// Older links the next page, and Newest the first, from a later
	// page. Either may be empty.
	Older, Newest string
}

// events are entries of history as the "events" template lists them.
type events struct {
	Days []activityDay
	// Bare leaves out each entry's node, in a list of one node's.
	Bare bool
}

// An activityFilter is one of Activity's filters.
type activityFilter struct {
	Title, Path string
	On          bool
}

// An activityDay is a day's entries, newest first.
type activityDay struct {
	// Key is the day's date, which keys it for the live-update script.
	Key, Title string
	Entries    []activityEntry
}

// An activityEntry is one entry of history, as a list shows it.
type activityEntry struct {
	wording.HistoryLine
	// Key keys it for the live-update script, and At is its time, in UTC.
	Key, At string
	// Node is its node's name, or empty for an entry about the Hub, and
	// Path its node's page, unless it's removed.
	Node, Path string
	// when is its time.
	when time.Time
}

// filters are Activity's filters, after All, in the prototype's order,
// by their keys.
var filters = []struct {
	key    string
	filter pool.Filter
}{
	{"commands", pool.FilterCommands},
	{"problems", pool.FilterProblems},
	{"jobs", pool.FilterJobs},
	{"settings", pool.FilterSettings},
	{"connections", pool.FilterConnections},
}

// beforeLayout is the layout of a page's before, a second in UTC.
const beforeLayout = "2006-01-02T15:04:05"

// activity serves Activity, under the filter, for the node, and before
// the second its query names. A filter, a node or a second it doesn't
// know isn't found.
func (d *Dashboard) activity(w http.ResponseWriter, r *http.Request) {
	a, ok := d.fleetAccess(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	key, query := q.Get("filter"), hubstore.HistoryQuery{}
	if key != "" {
		for _, f := range filters {
			if f.key == key {
				query.Filter = f.filter
			}
		}
		if query.Filter == 0 {
			d.notFound(w, r)
			return
		}
	}
	if b := q.Get("before"); b != "" {
		t, err := time.Parse(beforeLayout, b)
		if err != nil {
			d.notFound(w, r)
			return
		}
		query.Before = t
	}
	name := ""
	if q.Has("node") {
		f, err := d.ops.Fleet(r.Context())
		if err != nil {
			d.failed(w, r, err)
			return
		}
		det, ok := f.Find(q.Get("node"))
		if !ok {
			d.notFound(w, r)
			return
		}
		name, query.Node = det.Name, det.ID
	}
	act, err := d.ops.Activity(r.Context(), query)
	if err != nil {
		d.failed(w, r, err)
		return
	}
	p := d.activityPage(act, key, name, !query.Before.IsZero())
	p.Login = a.who.Login
	d.render(w, r, http.StatusOK, "activity", p)
}

// activityPage returns Activity showing act, under the filter key, or
// All for none, for the node named name, or every node for none, and
// whether it's a later page than the first.
func (d *Dashboard) activityPage(act hubops.Activity, key, name string, later bool) activityPage {
	p := activityPage{page: page{Title: "Activity", Tab: "activity", Live: []string{"activity"}}, Node: name}
	title := "All"
	p.Filters = append(p.Filters, activityFilter{Title: "All", Path: activityPath("", name, time.Time{}), On: key == ""})
	for _, f := range filters {
		on := f.key == key
		if on {
			title = f.filter.String()
		}
		p.Filters = append(p.Filters, activityFilter{Title: f.filter.String(), Path: activityPath(f.key, name, time.Time{}), On: on})
	}
	switch {
	case later:
		p.Empty = "Nothing older."
	case key == "" && name != "":
		p.Empty = "Nothing yet. What happens on " + name + " shows here as it happens."
	case key == "":
		p.Empty = "Nothing yet. What happens in the pool shows here as it happens."
	case name != "":
		p.Empty = "Nothing under " + title + " for " + name + " yet."
	default:
		p.Empty = "Nothing under " + title + " yet."
	}
	if name != "" {
		p.Every = activityPath(key, "", time.Time{})
	}
	if later {
		p.Newest = activityPath(key, name, time.Time{})
	}
	if !act.Older.IsZero() {
		p.Older = activityPath(key, name, act.Older)
	}
	p.Events = events{Days: d.days(d.entries(act.Entries))}
	return p
}

// entries returns es in words, newest first, as a list shows them.
func (d *Dashboard) entries(es []hubops.ActivityEntry) []activityEntry {
	c := d.hubClock()
	var out []activityEntry
	for _, e := range es {
		entry := activityEntry{HistoryLine: c.History(e.Entry, e.Name), Key: "e" + strconv.FormatInt(e.ID, 10), At: e.At.UTC().Format(time.RFC3339), Node: e.Name, when: e.At}
		if e.Name != "" && !e.Removed {
			entry.Path = nodePath(e.Name)
		}
		out = append(out, entry)
	}
	return out
}

// days returns entries, newest first, grouped by day in the Hub's zone.
func (d *Dashboard) days(entries []activityEntry) []activityDay {
	c := d.hubClock()
	var out []activityDay
	for _, e := range entries {
		day := e.when.In(c.Zone).Format(time.DateOnly)
		if len(out) == 0 || out[len(out)-1].Key != day {
			out = append(out, activityDay{Key: day, Title: c.Day(e.when)})
		}
		last := &out[len(out)-1]
		last.Entries = append(last.Entries, e)
	}
	return out
}

// activityPath is Activity's path under the filter key, or All for none,
// for the node named name, or every node for none, before the second
// before, or from the newest entries for zero.
func activityPath(key, name string, before time.Time) string {
	q := url.Values{}
	if key != "" {
		q.Set("filter", key)
	}
	if name != "" {
		q.Set("node", name)
	}
	if !before.IsZero() {
		q.Set("before", before.UTC().Format(beforeLayout))
	}
	if len(q) == 0 {
		return "/activity"
	}
	return "/activity?" + q.Encode()
}

package dashboard

import (
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/hubops"
)

// History beside the fleet, as variant A of "Prototype the Hub
// dashboard" has it: a node's page lists the node's newest entries at
// the foot of its left column, and Nodes the pool's most recent below
// its table, each linking Activity for the rest, where its filters are.
// Both list entries as Activity does, by day, with the late mark; a
// node's own list leaves out the node. While the node isn't connected,
// its list holds the gap the Hub writes once it reconnects, open since
// the node was last seen.

// historyLimit is how many of a node's newest entries its page lists,
// unless the last second of them holds more.
const historyLimit = 30

// recentLimit is how many of the pool's newest entries Nodes lists.
const recentLimit = 6

// A nodeHistory is a node's history, as its page lists it.
type nodeHistory struct {
	Events events
	// All is Activity's path for every entry of the node's, under its
	// filters.
	All string
}

// nodeHistory returns det's history from hist, the newest page of it.
func (d *Dashboard) nodeHistory(det hubops.Detail, hist hubops.Activity) nodeHistory {
	entries := d.entries(hist.Entries)
	if !det.Connected && !det.Seen.IsZero() {
		// The open gap sits at the time the node was last seen, above
		// what happened then, or last, when every entry is newer.
		i := 0
		for i < len(entries) && entries[i].when.After(det.Seen) {
			i++
		}
		open := activityEntry{HistoryLine: d.hubClock().OpenGap(det.Seen), Key: "open", At: det.Seen.UTC().Format(time.RFC3339), when: det.Seen}
		entries = slices.Insert(entries, i, open)
	}
	return nodeHistory{Events: events{Days: d.days(entries), Bare: true}, All: activityPath("", det.Name, time.Time{})}
}

// recent returns the pool's most recent entries from act, the newest
// page of its history, which may hold more, as its last second may.
func (d *Dashboard) recent(act hubops.Activity) events {
	return events{Days: d.days(d.entries(act.Entries[:min(len(act.Entries), recentLimit)]))}
}

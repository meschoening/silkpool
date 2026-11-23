package hubops

import (
	"context"
	"time"

	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/pool"
)

// An Activity is a page of the Hub's history, as the dashboard's
// Activity shows it: its entries, newest first, each with its node's
// name, and where the next page reads on.
type Activity struct {
	Entries []ActivityEntry
	// Older is the second the next page reads before, or zero if no
	// entry is older.
	Older time.Time
}

// An ActivityEntry is one entry of history, with its node as it is now.
type ActivityEntry struct {
	// ID rises with every entry the Hub keeps.
	ID int64
	pool.Entry
	// Name is the name of the entry's node, or empty for an entry about
	// the Hub.
	Name string
	// Removed reports that the entry's node is a tombstone, or is gone,
	// so it has no page.
	Removed bool
}

// Activity returns the page of the Hub's history q picks, newest first.
func (o *Ops) Activity(ctx context.Context, q hubstore.HistoryQuery) (Activity, error) {
	page, err := o.c.Hub.HistoryPage(ctx, q)
	if err != nil {
		return Activity{}, err
	}
	h, err := o.state(ctx)
	if err != nil {
		return Activity{}, err
	}
	out := Activity{Older: page.Older}
	for _, e := range page.Entries {
		a := ActivityEntry{ID: e.ID, Entry: e.Entry}
		if e.Entry.Node != "" {
			n, ok := byID(h, e.Entry.Node)
			a.Name, a.Removed = n.Name, !ok || !n.Removed.IsZero()
		}
		out.Entries = append(out.Entries, a)
	}
	return out, nil
}

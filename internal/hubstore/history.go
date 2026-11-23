package hubstore

import (
	"cmp"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/pool"
)

// The dashboard's Activity reads history a page at a time, newest
// first, through HistoryPage, which the history table's indexes on its
// time, node and kind serve.
//
// Newest first is by each entry's own time, then by its ID, so an entry
// a node delivered late sits at the time it happened. A time is stored
// as RFC 3339 in UTC, whose first 19 characters are its second, and
// whose fraction's trailing zeros are dropped: the stored form sorts as
// text by its second, but not within one. So a page is read in the
// index's order and ends at a whole second, with every entry of that
// second, and the entries within each second are put in order once
// they're read. The next page reads on before that second.

// HistoryPageSize is how many entries a page of history holds, unless
// its query asks for another number, and its last second holds more.
const HistoryPageSize = 100

// A HistoryQuery picks a page of the Hub's history.
type HistoryQuery struct {
	// Filter is the Activity filter whose kinds it reads, or 0 for every
	// kind.
	Filter pool.Filter
	// Node is the ID of the node whose entries it reads, or empty for
	// every node's and the Hub's own.
	Node string
	// Before is the second it reads before, from an earlier page's Older,
	// or zero for the newest entries. Only its whole second counts.
	Before time.Time
	// Limit is how many entries it reads, or 0 for HistoryPageSize. A
	// page holds more when its last second does.
	Limit int
}

// A HistoryPage is a page of the Hub's history.
type HistoryPage struct {
	// Entries are its entries, newest first: by their time, then by
	// their ID.
	Entries []HistoryEntry
	// Older is the second the next page reads before, the second of the
	// page's last entry, or zero if no entry is older.
	Older time.Time
}

// filterKinds are the stored kinds of entry under each Activity filter,
// as pool.Entry.Filter maps them. A node's kinds are package store's,
// which it writes for each node.Happening.
var filterKinds = map[pool.Filter][]string{
	pool.FilterCommands:    {"applied-command", removalStarted},
	pool.FilterJobs:        {"job"},
	pool.FilterProblems:    {"degraded-began", "degraded-ended", "notice-raised", "runner-version-refused", connectionRefused},
	pool.FilterConnections: {connectionGap, hubStarted, signedIn, signInRefused},
	pool.FilterSettings: {enrollment, tombstoned, lostTarget, regainedTarget, runnerVersionChanged,
		codeMade, codeRevoked, addedTarget, removedTarget, renamed, retargeted,
		labelsChanged, poolLabelChanged, connectionAdded, keepHistoryChanged},
}

// secondOf is the second of a stored time: its first 19 characters,
// "2006-01-02T15:04:05".
func secondOf(at string) string { return at[:min(len(at), len(secondLayout))] }

// secondLayout is the layout of a stored time's second, in UTC.
const secondLayout = "2006-01-02T15:04:05"

// HistoryPage returns the page of the Hub's history q picks.
func (s *Store) HistoryPage(q HistoryQuery) (HistoryPage, error) {
	var where []string
	var args []any
	if q.Filter != 0 {
		kinds, ok := filterKinds[q.Filter]
		if !ok {
			return HistoryPage{}, fmt.Errorf("history has no filter %v", q.Filter)
		}
		where = append(where, "kind IN (?"+strings.Repeat(", ?", len(kinds)-1)+")")
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	if q.Node != "" {
		where = append(where, "node = ?")
		args = append(args, q.Node)
	}
	// read reads up to limit of the entries the query picks that more
	// also picks, or every one for a limit of 0.
	read := func(limit int, more string, moreArgs ...any) ([]storedRead, error) {
		query := `SELECT id, at, node, origin, body FROM history WHERE ` + strings.Join(append(slices.Clone(where), more), " AND ") +
			` ORDER BY at DESC, id DESC`
		if limit > 0 {
			query += fmt.Sprintf(" LIMIT %d", limit)
		}
		return s.readHistory(query, append(slices.Clone(args), moreArgs...)...)
	}
	before := "9999-12-31T23:59:60"
	if !q.Before.IsZero() {
		before = q.Before.UTC().Format(secondLayout)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = HistoryPageSize
	}
	rows, err := read(limit, "at < ?", before)
	if err != nil {
		return HistoryPage{}, err
	}
	var page HistoryPage
	if len(rows) == limit {
		// The page ends at its last entry's second, with every entry of
		// that second, which the limit may have cut.
		last := secondOf(rows[limit-1].at)
		group, err := read(0, "at >= ? AND at <= ?", last, last+"Z")
		if err != nil {
			return HistoryPage{}, err
		}
		rows = append(slices.DeleteFunc(rows, func(r storedRead) bool { return secondOf(r.at) == last }), group...)
		older, err := read(1, "at < ?", last)
		if err != nil {
			return HistoryPage{}, err
		}
		if len(older) > 0 {
			if page.Older, err = time.Parse(secondLayout, last); err != nil {
				return HistoryPage{}, fmt.Errorf("reading history from %s: %w", s.db.Path(), err)
			}
		}
	}
	for _, r := range rows {
		page.Entries = append(page.Entries, r.HistoryEntry)
	}
	slices.SortFunc(page.Entries, func(a, b HistoryEntry) int {
		return cmp.Or(b.Entry.At.Compare(a.Entry.At), cmp.Compare(b.ID, a.ID))
	})
	return page, nil
}

// A storedRead is an entry read from history, with its stored time.
type storedRead struct {
	HistoryEntry
	at string
}

// readHistory reads the entries query selects, by their ID, time, node,
// origin and body, in the order it reads them.
func (s *Store) readHistory(query string, args ...any) ([]storedRead, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading history from %s: %w", s.db.Path(), err)
	}
	defer rows.Close()
	var out []storedRead
	for rows.Next() {
		var r storedRead
		var id, origin sql.NullString
		var body string
		if err := rows.Scan(&r.ID, &r.at, &id, &origin, &body); err != nil {
			return nil, fmt.Errorf("reading history from %s: %w", s.db.Path(), err)
		}
		if r.Entry, err = decodeEntry(id.String, origin.String, body); err != nil {
			return nil, fmt.Errorf("reading history entry %d from %s: %w", r.ID, s.db.Path(), err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

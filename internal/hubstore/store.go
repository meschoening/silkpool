package hubstore

import (
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/registration"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/store"
)

// File is the name of the Hub's database in its state folder.
const File = "hub.db"

// A Store is the Hub's state, open in memory and on disk. Only the Hub
// service opens it, and it applies one event at a time.
type Store struct {
	db  *store.DB
	hub Hub
	// wakes holds each core's latest WakeAt, or zero if it has none.
	wakes [cores]time.Time
	// broken is the error of the commit that failed, after which nothing
	// more is applied.
	broken error
}

// The cores whose WakeAts the store merges.
const (
	poolCore = iota
	recordsCore
	versionsCore
	cores
)

// Open opens the Hub's store in dir, its state folder, creating it if
// it doesn't exist, and loads everything. It refuses a database that's
// corrupt or unreadable, or that a newer Silkpool wrote, naming the
// file, and migrates an older one forward. The Hub then applies
// pool.Started before anything else.
func Open(dir string) (*Store, error) {
	db, err := store.Open(filepath.Join(dir, File), migrations, tooNew)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if s.hub, err = load(db); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// Hub returns the Hub's state as of the latest commit. The caller must
// treat it as read-only.
func (s *Store) Hub() Hub { return s.hub }

// Apply applies e at now to every core it reaches, commits the result in
// one transaction, and returns the effects the Hub must then carry out,
// in order, ending with a WakeAt if any core has something to do later.
//
// If the commit fails, Apply returns the error and no effects, and the
// store keeps the state of the last commit and refuses every later
// event: a store error ends the service, and restarting it is the only
// way back.
func (s *Store) Apply(e Event, now time.Time) ([]Effect, error) {
	if s.broken != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrBroken, s.broken)
	}
	a := &applying{hub: s.hub, now: now, wakes: s.wakes}
	if err := a.apply(e); err != nil {
		return nil, err
	}
	if err := s.commit(a.hub, a.history, a.prune); err != nil {
		s.broken = err
		return nil, err
	}
	s.hub, s.wakes = a.hub, a.wakes
	effects := a.effects
	var next time.Time
	for _, at := range s.wakes {
		if !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	if !next.IsZero() {
		effects = append(effects, WakeAt{At: next})
	}
	return effects, nil
}

// applying is one Apply under way.
type applying struct {
	hub     Hub
	now     time.Time
	wakes   [cores]time.Time
	effects []Effect
	history []pool.Store
	// prune is the latest Prune the pool asked for, or nil.
	prune *pool.Prune
}

// errOtherCore refuses an event that the pool's own event of the same
// name covers.
type errOtherCore struct{ event, use any }

func (e errOtherCore) Error() string {
	return fmt.Sprintf("hubstore: apply %T, which reaches every core it should, not %T", e.use, e.event)
}

func (a *applying) apply(e Event) error {
	switch e := e.(type) {
	case pool.Started:
		a.pool(e)
		a.records(registration.Started{})
		a.versions(runnerapp.Started{})
	case pool.Tick:
		a.pool(e)
		a.records(registration.Tick{})
		a.versions(runnerapp.Tick{})
	case pool.Connected:
		if a.accepted(e.Node, a.pool(e)) {
			a.records(registration.Connected{Node: e.Node})
		}
	case pool.Disconnected:
		a.pool(e)
		a.records(registration.Disconnected{Node: e.Node})
	case pool.Reported:
		a.pool(e)
		a.records(registration.Reported{Node: e.Node, Report: e.Report})
	case pool.TargetLost:
		a.pool(e)
		a.records(registration.TargetLost{Target: e.Target})
	case pool.Confirmed:
		a.pool(e)
		if i := slices.IndexFunc(a.hub.Pool.Nodes, func(n pool.Node) bool { return n.ID == e.Node }); i >= 0 && !a.hub.Pool.Nodes[i].Connected {
			a.records(registration.Disconnected{Node: e.Node})
		}
	case pool.Event:
		a.pool(e)
	case registration.Started:
		return errOtherCore{e, pool.Started{}}
	case registration.Tick:
		return errOtherCore{e, pool.Tick{}}
	case registration.Connected:
		return errOtherCore{e, pool.Connected{}}
	case registration.Disconnected:
		return errOtherCore{e, pool.Disconnected{}}
	case registration.Reported:
		return errOtherCore{e, pool.Reported{}}
	case registration.TargetLost:
		return errOtherCore{e, pool.TargetLost{}}
	case registration.Event:
		a.records(e)
	case runnerapp.Started:
		return errOtherCore{e, pool.Started{}}
	case runnerapp.Tick:
		return errOtherCore{e, pool.Tick{}}
	case runnerapp.Added:
		return errOtherCore{e, PutTarget{}}
	case runnerapp.Dropped:
		return errOtherCore{e, DropTarget{}}
	case runnerapp.Event:
		a.versions(e)
	case SetSettings:
		from, to := a.hub.Settings.PoolLabel, e.Settings.PoolLabel
		a.hub.Settings = e.Settings
		a.hub.Pool.DefaultTarget = e.Settings.DefaultTarget
		if from != to {
			a.keep(pool.Entry{At: a.now, Origin: e.Origin, What: pool.PoolLabelChanged{From: from, To: to}})
			// Every runner carries the pool label, so each node's runners
			// carry one they no longer should.
			for _, n := range a.hub.Pool.Nodes {
				if n.Removed.IsZero() {
					a.records(registration.RunnersOutdated{Node: n.ID})
				}
			}
		}
	case PutConnection:
		match := func(c Connection) bool { return c.Account == e.Connection.Account }
		if !slices.ContainsFunc(a.hub.Connections, match) {
			a.keep(pool.Entry{At: a.now, Origin: e.Origin, What: pool.GitHubConnectionAdded{Account: e.Connection.Account}})
		}
		a.hub.Connections = put(a.hub.Connections, e.Connection, match)
	case PutTarget:
		added := !slices.ContainsFunc(a.hub.Targets, func(t Target) bool { return t.Key == e.Target.Key })
		a.hub.Targets = put(a.hub.Targets, e.Target, func(t Target) bool { return t.Key == e.Target.Key })
		a.pool(pool.AddTarget{Target: e.Target.Key, Scope: e.Target.Scope(), Origin: e.Origin})
		if added {
			a.versions(runnerapp.Added{Target: e.Target.Key})
		}
		if a.hub.Settings.DefaultTarget == "" {
			a.setDefault(e.Target.Key)
		}
	case DropTarget:
		if users := a.users(e.Key); len(users) > 0 {
			a.effects = append(a.effects, pool.ChangeRefused{Reason: TargetInUse{Target: e.Key, Nodes: users}})
			return nil
		}
		a.hub.Targets = slices.DeleteFunc(slices.Clone(a.hub.Targets), func(t Target) bool { return t.Key == e.Key })
		a.versions(runnerapp.Dropped{Target: e.Key})
		a.pool(pool.DropTarget{Target: e.Key, Origin: e.Origin})
		if a.hub.Settings.DefaultTarget == e.Key {
			next := ""
			if len(a.hub.Targets) > 0 {
				next = a.hub.Targets[0].Key
			}
			a.setDefault(next)
		}
	case SetLabels:
		a.setLabels(e)
	default:
		return fmt.Errorf("hubstore: %T isn't an event the Hub applies", e)
	}
	return nil
}

// setDefault makes key the target new nodes get, in the settings and the
// pool.
func (a *applying) setDefault(key string) {
	a.hub.Settings.DefaultTarget = key
	a.hub.Pool.DefaultTarget = key
}

// users returns the names of the nodes that use the target key, in the
// order they enrolled. A tombstone uses none.
func (a *applying) users(key string) []string {
	var names []string
	for _, n := range a.hub.Pool.Nodes {
		if n.Target == key && n.Removed.IsZero() {
			names = append(names, n.Name)
		}
	}
	return names
}

// TargetInUse refuses removing a target that nodes use, naming them: the
// operator gives them another target first.
type TargetInUse struct {
	Target string
	Nodes  []string
}

func (e TargetInUse) Error() string {
	if len(e.Nodes) == 1 {
		return fmt.Sprintf("the target %s is used by %s: give it another target first", e.Target, e.Nodes[0])
	}
	names := strings.Join(e.Nodes[:len(e.Nodes)-1], ", ") + " and " + e.Nodes[len(e.Nodes)-1]
	return fmt.Sprintf("the target %s is used by %s: give them another target first", e.Target, names)
}

// put returns list with v in place of the element match finds, or
// appended if there's none.
func put[T any](list []T, v T, match func(T) bool) []T {
	list = slices.Clone(list)
	if i := slices.IndexFunc(list, match); i >= 0 {
		list[i] = v
		return list
	}
	return append(list, v)
}

// accepted reports whether the pool accepted node's connection, whose
// effects are effects.
func (a *applying) accepted(id string, effects []pool.Effect) bool {
	for _, eff := range effects {
		switch eff := eff.(type) {
		case pool.AnswerRefused:
			if eff.Node == id {
				return false
			}
		case pool.AnswerRemoved:
			if eff.Node == id {
				return false
			}
		}
	}
	i := slices.IndexFunc(a.hub.Pool.Nodes, func(n pool.Node) bool { return n.ID == id })
	return i >= 0 && a.hub.Pool.Nodes[i].Connected
}

// pool applies e to the pool, and returns its effects.
func (a *applying) pool(e pool.Event) []pool.Effect {
	var effects []pool.Effect
	a.hub.Pool, effects = a.hub.Pool.Apply(e, a.now)
	a.wakes[poolCore] = time.Time{}
	for _, eff := range effects {
		switch eff := eff.(type) {
		case pool.DeleteRegistrations:
			a.records(registration.Removing{Node: eff.Node})
		case pool.RetireRunners:
			a.records(registration.RunnersOutdated{Node: eff.Node})
		case pool.RunnerVersionRefused:
			a.versions(runnerapp.Refused{Target: eff.Target, Version: eff.Version})
		case pool.AnswerRemoved:
			// The records know which of its runners it must stop itself.
			eff.Lost = a.hub.Records.LostRunners(eff.Node)
			a.effects = append(a.effects, eff)
		case pool.Store:
			a.history = append(a.history, eff)
		case pool.Prune:
			a.prune = &eff
		case pool.WakeAt:
			a.wakes[poolCore] = eff.At
		default:
			a.effects = append(a.effects, eff)
		}
	}
	return effects
}

// records applies e to the registration records.
func (a *applying) records(e registration.Event) {
	var effects []registration.Effect
	a.hub.Records, effects = a.hub.Records.Apply(e, a.now)
	a.wakes[recordsCore] = time.Time{}
	for _, eff := range effects {
		if w, ok := eff.(registration.WakeAt); ok {
			a.wakes[recordsCore] = w.At
			continue
		}
		a.effects = append(a.effects, eff)
	}
}

// versions applies e to the runner versions.
func (a *applying) versions(e runnerapp.Event) {
	var effects []runnerapp.Effect
	a.hub.Versions, effects = a.hub.Versions.Apply(e, a.now)
	a.wakes[versionsCore] = time.Time{}
	for _, eff := range effects {
		switch eff := eff.(type) {
		case runnerapp.Changed:
			a.pool(pool.RunnerAppChanged{Target: eff.Target, Version: eff.Version})
		case runnerapp.WakeAt:
			a.wakes[versionsCore] = eff.At
		default:
			a.effects = append(a.effects, eff)
		}
	}
}

// setLabels sets a node's labels, and writes them and retires its
// runners if they changed. Labels for a node the pool doesn't hold are
// ignored.
func (a *applying) setLabels(e SetLabels) {
	if !slices.ContainsFunc(a.hub.Pool.Nodes, func(n pool.Node) bool { return n.ID == e.Node }) {
		return
	}
	from, to := a.hub.Labels[e.Node], slices.Clone(e.Labels)
	if len(to) == 0 {
		to = nil
	}
	if slices.Equal(from, to) {
		return
	}
	a.hub.Labels = maps.Clone(a.hub.Labels)
	if a.hub.Labels == nil {
		a.hub.Labels = map[string][]string{}
	}
	if to == nil {
		delete(a.hub.Labels, e.Node)
	} else {
		a.hub.Labels[e.Node] = to
	}
	a.keep(pool.Entry{Node: e.Node, At: a.now, Origin: e.Origin, What: pool.LabelsChanged{From: from, To: to}})
	a.records(registration.RunnersOutdated{Node: e.Node})
}

// keep keeps e in the Hub's history, in this commit: an entry about
// state the store keeps itself, which the pool doesn't hold.
func (a *applying) keep(e pool.Entry) { a.history = append(a.history, pool.Store{Entry: e}) }

// commit saves h, appends the history entries, and prunes history if
// prune asks, in one transaction.
func (s *Store) commit(h Hub, history []pool.Store, prune *pool.Prune) error {
	tables, err := rows(h)
	if err != nil {
		return err
	}
	entries, err := historyRows(history)
	if err != nil {
		return err
	}
	return s.db.Commit(func(tx *store.Tx) error {
		for _, name := range tableNames {
			if err := tx.Save(name, tables[name]); err != nil {
				return err
			}
		}
		for _, e := range entries {
			if err := tx.Exec(`INSERT INTO history (at, node, kind, origin, body, seq) VALUES (?, ?, ?, ?, ?, ?)`,
				e.at, e.node, e.kind, e.origin, e.body, e.seq); err != nil {
				return fmt.Errorf("adding to history: %w", err)
			}
		}
		// Times are compared as times: the stored form drops a fraction's
		// trailing zeros, so it doesn't sort as text.
		if prune != nil {
			if err := tx.Exec(`DELETE FROM history WHERE julianday(at) < julianday(?)`, store.Stamp(prune.Before)); err != nil {
				return fmt.Errorf("pruning history: %w", err)
			}
		}
		return nil
	})
}

// A HistoryEntry is one entry of the Hub's history, with its ID, which
// rises with every entry.
type HistoryEntry struct {
	ID    int64
	Entry pool.Entry
}

// History returns the Hub's history, oldest first. It's the one thing
// read from the database while the Hub runs.
func (s *Store) History() ([]HistoryEntry, error) {
	rows, err := s.db.Query(`SELECT id, node, origin, body FROM history ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading history from %s: %w", s.db.Path(), err)
	}
	defer rows.Close()
	var out []HistoryEntry
	for rows.Next() {
		var h HistoryEntry
		var id, origin sql.NullString
		var body string
		if err := rows.Scan(&h.ID, &id, &origin, &body); err != nil {
			return nil, fmt.Errorf("reading history from %s: %w", s.db.Path(), err)
		}
		if h.Entry, err = decodeEntry(id.String, origin.String, body); err != nil {
			return nil, fmt.Errorf("reading history entry %d from %s: %w", h.ID, s.db.Path(), err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

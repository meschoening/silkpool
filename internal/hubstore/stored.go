package hubstore

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/github"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/registration"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/store"
)

// migrations make the Hub's schema, one version each. A change to the
// stored form adds one, and a golden file of the version it makes.
var migrations = []store.Migration{createV1}

// tooNew is what the operator can do about a database a newer Silkpool
// wrote. The copy of the state folder made before upgrading is the
// Hub's way back.
const tooNew = "install that version again, or restore the state folder from the copy made before upgrading"

// The entity tables, in the order they're saved. Each holds one row per
// entity: its key, its position, and its stored form as JSON.
var tableNames = []string{"settings", "connections", "targets", "nodes", "codes", "runners", "registration_nodes"}

func createV1(tx *sql.Tx) error {
	for _, name := range tableNames {
		if _, err := tx.Exec(`CREATE TABLE ` + name + ` (key TEXT PRIMARY KEY, pos INTEGER NOT NULL, body TEXT NOT NULL) STRICT`); err != nil {
			return err
		}
	}
	for _, q := range []string{
		// The one table with real columns, which the dashboard's
		// Activity filters by node, kind and time. Seq is the node's
		// sequence number of an entry from its outbox, which the Hub
		// stores once.
		`CREATE TABLE history (id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, node TEXT, kind TEXT NOT NULL, ` +
			`origin TEXT, body TEXT NOT NULL, seq INTEGER, UNIQUE (node, seq)) STRICT`,
		`CREATE INDEX history_by_time ON history (at)`,
		`CREATE INDEX history_by_node ON history (node, at)`,
		`CREATE INDEX history_by_kind ON history (kind, at)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// settingsKey keys the one row of settings.
const settingsKey = "hub"

type storedSettings struct {
	HubID         string          `json:"hub_id,omitempty"`
	BaseURL       string          `json:"base_url,omitempty"`
	Listener      storedListener  `json:"listener,omitzero"`
	PoolLabel     string          `json:"pool_label,omitempty"`
	DefaultTarget string          `json:"default_target,omitempty"`
	KeepHistory   int             `json:"keep_history_days,omitempty"`
	Alive         string          `json:"alive,omitempty"`
	Operator      int64           `json:"operator,omitempty"`
	Setup         string          `json:"setup,omitempty"`
	SetupCode     storedSetupCode `json:"setup_code,omitzero"`
	Connecting    storedAccount   `json:"connecting,omitzero"`
	// SignInsRefused holds when the Hub last wrote a refused sign-in for
	// each GitHub login, for an hour after it.
	SignInsRefused map[string]string `json:"sign_ins_refused,omitempty"`
}

type storedListener struct {
	Mode    string `json:"mode,omitempty"`
	Address string `json:"address,omitempty"`
}

type storedSetupCode struct {
	Hash     string `json:"hash,omitempty"`
	Expires  string `json:"expires,omitempty"`
	Reassign bool   `json:"reassign,omitempty"`
}

type storedAccount struct {
	Kind  string `json:"kind,omitempty"`
	Login string `json:"login,omitempty"`
}

type storedConnection struct {
	Kind         string `json:"kind"`
	AppID        int64  `json:"app_id,omitempty"`
	AppSlug      string `json:"app_slug,omitempty"`
	Installation int64  `json:"installation,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	KeyFile      string `json:"key_file,omitempty"`
	SecretFile   string `json:"secret_file,omitempty"`
}

type storedTarget struct {
	Connection string        `json:"connection,omitempty"`
	Owner      string        `json:"owner,omitempty"`
	Repo       string        `json:"repo,omitempty"`
	RepoID     int64         `json:"repo_id,omitempty"`
	Group      int64         `json:"group,omitempty"`
	Added      string        `json:"added,omitempty"`
	Version    storedVersion `json:"runner_version,omitzero"`
	Lost       string        `json:"lost,omitempty"`
}

// storedVersion is a target's runner version: the release, and the
// SHA-256 of its package for each platform, by GitHub's platform name.
type storedVersion struct {
	Version string            `json:"version,omitempty"`
	SHA256  map[string]string `json:"sha256,omitempty"`
}

type storedNode struct {
	Name         string         `json:"name"`
	Key          string         `json:"key,omitempty"`
	Target       string         `json:"target,omitempty"`
	Labels       []string       `json:"labels,omitempty"`
	Connected    bool           `json:"connected,omitempty"`
	ConnectedAt  string         `json:"connected_at,omitempty"`
	Dropped      bool           `json:"dropped,omitempty"`
	Seen         string         `json:"seen,omitempty"`
	Platform     string         `json:"platform,omitempty"`
	Mirror       store.Node     `json:"mirror"`
	Sent         []string       `json:"sent,omitempty"`
	NotConfirmed []string       `json:"not_confirmed,omitempty"`
	Stored       int64          `json:"stored,omitempty"`
	Removing     bool           `json:"removing,omitempty"`
	Remove       store.Remove   `json:"remove,omitzero"`
	Removed      string         `json:"removed,omitempty"`
	Replaced     []string       `json:"replaced,omitempty"`
	Refused      string         `json:"refused,omitempty"`
	Mismatch     *storedRefusal `json:"mismatch,omitempty"`
	WrongKey     string         `json:"wrong_key,omitempty"`
	KeyInUse     string         `json:"key_in_use,omitempty"`
}

// storedRefusal is why the Hub refused a node's connection: its kind,
// and for a version refusal, both versions.
type storedRefusal struct {
	Kind  string `json:"kind"`
	Agent int    `json:"agent,omitempty"`
	Hub   int    `json:"hub,omitempty"`
}

// The kinds of refusal.
const (
	versionRefusal = "version"
	keyRefusal     = "key"
)

func (e *encoder) refusal(err error) *storedRefusal {
	switch err := err.(type) {
	case nil:
		return nil
	case pool.VersionMismatch:
		return &storedRefusal{Kind: versionRefusal, Agent: err.Agent, Hub: err.Hub}
	}
	if err == pool.ErrWrongKey {
		return &storedRefusal{Kind: keyRefusal}
	}
	e.fail(fmt.Errorf("the refusal %q has no stored form", err))
	return nil
}

// mismatch returns m's stored form, or nil for none.
func (e *encoder) mismatch(m pool.VersionMismatch) *storedRefusal {
	if m == (pool.VersionMismatch{}) {
		return nil
	}
	return e.refusal(m)
}

func (s *storedRefusal) toError() (error, error) {
	switch {
	case s == nil:
		return nil, nil
	case s.Kind == versionRefusal:
		return pool.VersionMismatch{Agent: s.Agent, Hub: s.Hub}, nil
	case s.Kind == keyRefusal:
		return pool.ErrWrongKey, nil
	}
	return nil, fmt.Errorf("%q isn't a stored kind of refusal", s.Kind)
}

type storedCode struct {
	Hash    string `json:"hash"`
	Made    string `json:"made"`
	Used    string `json:"used,omitempty"`
	Revoked bool   `json:"revoked,omitempty"`
}

type storedRecord struct {
	Node     string `json:"node"`
	Slot     int    `json:"slot"`
	Target   string `json:"target"`
	ID       int64  `json:"id,omitempty"`
	Phase    string `json:"phase"`
	Lost     bool   `json:"lost,omitempty"`
	Revive   string `json:"revive,omitempty"`
	Retiring bool   `json:"retiring,omitempty"`
	Deleting bool   `json:"deleting,omitempty"`
	Relay    bool   `json:"relay,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	RetryAt  string `json:"retry_at,omitempty"`
}

type storedConnectionState struct {
	Reconciled bool `json:"reconciled,omitempty"`
	Removing   bool `json:"removing,omitempty"`
}

var (
	recordPhases = store.NewCodes("registration phase", map[registration.Phase]string{
		registration.Waiting:    "waiting",
		registration.Minting:    "minting",
		registration.Abandoned:  "abandoned",
		registration.Delivering: "delivering",
		registration.Live:       "live",
		registration.Stale:      "stale",
	})
	// revivePhases are a stale record's Revive: none, or the phase it
	// goes back to if its target is lost.
	revivePhases = store.NewCodes("registration revive phase", map[registration.Phase]string{
		registration.Waiting:    "",
		registration.Delivering: "delivering",
		registration.Live:       "live",
	})
	listenerModes = store.NewCodes("listener mode", map[ListenerMode]string{
		"": "", SelfSigned: string(SelfSigned), CertificateFiles: string(CertificateFiles), BehindProxy: string(BehindProxy),
	})
	steps = store.NewCodes("setup step", map[Step]string{
		"": "", StepChecks: string(StepChecks), StepAccount: string(StepAccount), StepCreateApp: string(StepCreateApp),
		StepSignIn: string(StepSignIn), StepInstall: string(StepInstall), StepEnable: string(StepEnable), StepDone: string(StepDone),
	})
	kinds = store.NewCodes("connection kind", map[Kind]string{Personal: string(Personal), Organization: string(Organization)})
	// accountKinds are kinds and none, for the account setup connects.
	accountKinds = store.NewCodes("account kind", map[Kind]string{"": "", Personal: string(Personal), Organization: string(Organization)})
)

// encoder builds stored forms, keeping the first error.
type encoder struct{ err error }

func (e *encoder) fail(err error) {
	if e.err == nil && err != nil {
		e.err = err
	}
}

func code[T comparable](e *encoder, c store.Codes[T], v T) string {
	s, err := c.Code(v)
	e.fail(err)
	return s
}

func (e *encoder) row(key string, v any) store.Row {
	body, err := json.Marshal(v)
	e.fail(err)
	return store.Row{Key: key, Body: body}
}

// rows returns each table's rows for h.
func rows(h Hub) (map[string][]store.Row, error) {
	var e encoder
	t := map[string][]store.Row{}
	s := h.Settings
	settings := storedSettings{
		HubID:         s.HubID,
		BaseURL:       s.BaseURL,
		Listener:      storedListener{Mode: code(&e, listenerModes, s.Listener.Mode), Address: s.Listener.Address},
		PoolLabel:     s.PoolLabel,
		DefaultTarget: h.Pool.DefaultTarget,
		KeepHistory:   h.Pool.KeepHistory,
		Alive:         store.Stamp(h.Pool.Alive),
		Operator:      s.Operator,
		Setup:         code(&e, steps, s.Setup),
		SetupCode:     storedSetupCode{Hash: hash(s.SetupCode.Hash), Expires: store.Stamp(s.SetupCode.Expires), Reassign: s.SetupCode.Reassign},
		Connecting:    storedAccount{Kind: code(&e, accountKinds, s.Connecting.Kind), Login: s.Connecting.Login},
	}
	for login, at := range h.Pool.SignInsRefused {
		if settings.SignInsRefused == nil {
			settings.SignInsRefused = map[string]string{}
		}
		settings.SignInsRefused[login] = store.Stamp(at)
	}
	t["settings"] = []store.Row{e.row(settingsKey, settings)}
	for _, c := range h.Connections {
		t["connections"] = append(t["connections"], e.row(c.Account, storedConnection{
			Kind: code(&e, kinds, c.Kind), AppID: c.AppID, AppSlug: c.AppSlug, Installation: c.Installation,
			ClientID: c.ClientID, KeyFile: c.KeyFile, SecretFile: c.SecretFile,
		}))
	}
	for _, target := range h.Targets {
		g := target.GitHub
		v := h.Versions.Targets[target.Key].Version
		version := storedVersion{Version: v.Version}
		if len(v.SHA256) > 0 {
			version.SHA256 = map[string]string{}
			for p, sum := range v.SHA256 {
				version.SHA256[string(p)] = sum
			}
		}
		t["targets"] = append(t["targets"], e.row(target.Key, storedTarget{
			Connection: target.Connection, Owner: g.Owner, Repo: g.Repo, RepoID: g.RepoID, Group: g.Group,
			Added: store.Stamp(target.Added), Version: version, Lost: store.Stamp(h.Pool.Targets[target.Key].Lost),
		}))
	}
	for _, n := range h.Pool.Nodes {
		mirror, err := store.FromNode(n.Mirror)
		e.fail(err)
		remove, err := store.FromRemove(n.Remove)
		e.fail(err)
		t["nodes"] = append(t["nodes"], e.row(n.ID, storedNode{
			Name: n.Name, Key: n.Key, Target: n.Target, Labels: h.Labels[n.ID],
			Connected: n.Connected, ConnectedAt: store.Stamp(n.ConnectedAt), Dropped: n.Dropped, Seen: store.Stamp(n.Seen),
			Platform: string(n.Platform), Mirror: mirror,
			Sent: n.Sent, NotConfirmed: n.NotConfirmed, Stored: n.Stored,
			Removing: n.Removing, Remove: remove, Removed: store.Stamp(n.Removed),
			Replaced: stamps(n.Replaced), Refused: store.Stamp(n.Refused), Mismatch: e.mismatch(n.Mismatch), WrongKey: store.Stamp(n.WrongKey),
			KeyInUse: store.Stamp(n.KeyInUse),
		}))
	}
	for _, c := range h.Pool.Codes {
		t["codes"] = append(t["codes"], e.row(c.ID, storedCode{Hash: hash(c.Hash), Made: store.Stamp(c.Made), Used: c.Used, Revoked: c.Revoked}))
	}
	for _, r := range h.Records.Runners {
		t["runners"] = append(t["runners"], e.row(r.Name, storedRecord{
			Node: r.Node, Slot: r.Slot, Target: r.Target, ID: r.ID, Phase: code(&e, recordPhases, r.Phase),
			Lost: r.Lost, Revive: code(&e, revivePhases, r.Revive), Retiring: r.Retiring, Deleting: r.Deleting, Relay: r.Relay,
			Attempts: r.Attempts, RetryAt: store.Stamp(r.RetryAt),
		}))
	}
	for _, id := range slices.Sorted(maps.Keys(h.Records.Connected)) {
		c := h.Records.Connected[id]
		t["registration_nodes"] = append(t["registration_nodes"], e.row(id, storedConnectionState(c)))
	}
	return t, e.err
}

func hash(h [32]byte) string {
	if h == [32]byte{} {
		return ""
	}
	return hex.EncodeToString(h[:])
}

func stamps(ts []time.Time) []string {
	var out []string
	for _, t := range ts {
		out = append(out, store.Stamp(t))
	}
	return out
}

// historyRow is one history entry's columns.
type historyRow struct {
	at     string
	node   sql.NullString
	kind   string
	origin sql.NullString
	body   string
	seq    sql.NullInt64
}

// historyRows returns the columns of each history entry the pool asked
// to keep.
func historyRows(entries []pool.Store) ([]historyRow, error) {
	var out []historyRow
	for _, s := range entries {
		var e encoder
		entry := e.entry(s.Entry)
		if e.err != nil {
			return nil, e.err
		}
		body, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		row := historyRow{
			at:   entry.At,
			node: sql.NullString{String: s.Entry.Node, Valid: s.Entry.Node != ""},
			kind: entry.kind,
			body: string(body),
			seq:  sql.NullInt64{Int64: s.Entry.Seq, Valid: s.Entry.Seq != 0},
		}
		if o := s.Entry.Origin; o != (node.Origin{}) {
			stored, err := store.FromOrigin(o)
			if err != nil {
				return nil, err
			}
			origin, err := json.Marshal(stored)
			if err != nil {
				return nil, err
			}
			row.origin = sql.NullString{String: string(origin), Valid: true}
		}
		out = append(out, row)
	}
	return out, nil
}

// storedEntry is a history entry's body. Its node and origin are
// columns of their own, and its kind is a column too. What is a
// store.Happening for an entry from a node's outbox, and a
// storedHubHappening for one the Hub writes.
type storedEntry struct {
	Seq  int64           `json:"seq,omitempty"`
	At   string          `json:"at"`
	Late bool            `json:"late,omitempty"`
	What json.RawMessage `json:"what"`
	kind string
}

// storedHubHappening is the stored form of what an entry the Hub writes
// records: its kind, and the fields of that kind.
type storedHubHappening struct {
	Kind string `json:"kind"`
	// A connection gap's and a Hub start's.
	Since string `json:"since,omitempty"`
	Alive string `json:"alive,omitempty"`
	// A refused connection's.
	Refusal *storedRefusal `json:"refusal,omitempty"`
	// An enrollment's, and a target's.
	Name   string `json:"name,omitempty"`
	Target string `json:"target,omitempty"`
	// What a change changed from and to: a runner version, a node's name
	// or target, or the pool label.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// A change of a node's custom labels.
	FromLabels []string `json:"from_labels,omitempty"`
	ToLabels   []string `json:"to_labels,omitempty"`
	// A change of the history setting, in days.
	FromDays int `json:"from_days,omitempty"`
	ToDays   int `json:"to_days,omitempty"`
	// A code's ID, and a GitHub connection's account.
	Code    string `json:"code,omitempty"`
	Account string `json:"account,omitempty"`
	// A sign-in's remote address, and a refused one's GitHub login.
	Address string `json:"address,omitempty"`
	Login   string `json:"login,omitempty"`
}

// The kinds of entry the Hub writes.
const (
	connectionGap        = "connection-gap"
	hubStarted           = "hub-started"
	connectionRefused    = "connection-refused"
	enrollment           = "enrollment"
	removalStarted       = "removal-started"
	tombstoned           = "tombstoned"
	lostTarget           = "lost-target"
	regainedTarget       = "regained-target"
	runnerVersionChanged = "runner-version-changed"
	codeMade             = "code-made"
	codeRevoked          = "code-revoked"
	addedTarget          = "added-target"
	removedTarget        = "removed-target"
	renamed              = "renamed"
	retargeted           = "retargeted"
	labelsChanged        = "labels-changed"
	poolLabelChanged     = "pool-label-changed"
	connectionAdded      = "github-connection-added"
	keepHistoryChanged   = "keep-history-changed"
	signedIn             = "signed-in"
	signInRefused        = "sign-in-refused"
)

func (e *encoder) entry(entry pool.Entry) storedEntry {
	s := storedEntry{Seq: entry.Seq, At: store.Stamp(entry.At), Late: entry.Late}
	var what any
	if h, ok := entry.What.(node.Happening); ok {
		stored, err := store.FromEntry(node.Entry{What: h})
		e.fail(err)
		what, s.kind = stored.What, stored.What.Kind
	} else {
		h := e.hubHappening(entry.What)
		what, s.kind = h, h.Kind
	}
	s.What = e.json(what)
	return s
}

func (e *encoder) json(v any) json.RawMessage {
	b, err := json.Marshal(v)
	e.fail(err)
	return b
}

func (e *encoder) hubHappening(what any) storedHubHappening {
	switch h := what.(type) {
	case pool.ConnectionGap:
		return storedHubHappening{Kind: connectionGap, Since: store.Stamp(h.Since)}
	case pool.HubStarted:
		return storedHubHappening{Kind: hubStarted, Alive: store.Stamp(h.Alive)}
	case pool.ConnectionRefused:
		return storedHubHappening{Kind: connectionRefused, Refusal: e.refusal(h.Reason)}
	case pool.Enrollment:
		return storedHubHappening{Kind: enrollment, Name: h.Name, Target: h.Target}
	case pool.RemovalStarted:
		return storedHubHappening{Kind: removalStarted}
	case pool.Tombstoned:
		return storedHubHappening{Kind: tombstoned}
	case pool.LostTarget:
		return storedHubHappening{Kind: lostTarget, Target: h.Target}
	case pool.RegainedTarget:
		return storedHubHappening{Kind: regainedTarget, Target: h.Target}
	case pool.RunnerVersionChanged:
		return storedHubHappening{Kind: runnerVersionChanged, Target: h.Target, From: h.From, To: h.To}
	case pool.CodeMade:
		return storedHubHappening{Kind: codeMade, Code: h.Code}
	case pool.CodeRevoked:
		return storedHubHappening{Kind: codeRevoked, Code: h.Code}
	case pool.AddedTarget:
		return storedHubHappening{Kind: addedTarget, Target: h.Target}
	case pool.RemovedTarget:
		return storedHubHappening{Kind: removedTarget, Target: h.Target}
	case pool.Renamed:
		return storedHubHappening{Kind: renamed, From: h.From, To: h.To}
	case pool.Retargeted:
		return storedHubHappening{Kind: retargeted, From: h.From, To: h.To}
	case pool.LabelsChanged:
		return storedHubHappening{Kind: labelsChanged, FromLabels: h.From, ToLabels: h.To}
	case pool.PoolLabelChanged:
		return storedHubHappening{Kind: poolLabelChanged, From: h.From, To: h.To}
	case pool.GitHubConnectionAdded:
		return storedHubHappening{Kind: connectionAdded, Account: h.Account}
	case pool.KeepHistoryChanged:
		return storedHubHappening{Kind: keepHistoryChanged, FromDays: h.From, ToDays: h.To}
	case pool.SignedIn:
		return storedHubHappening{Kind: signedIn, Address: h.Address}
	case pool.SignInRefused:
		return storedHubHappening{Kind: signInRefused, Login: h.Login, Address: h.Address}
	}
	e.fail(fmt.Errorf("%T has no stored form", what))
	return storedHubHappening{}
}

// decodeEntry returns the history entry about the node id, from origin,
// whose body is body.
func decodeEntry(id, origin, body string) (pool.Entry, error) {
	var s storedEntry
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		return pool.Entry{}, err
	}
	at, err := store.Unstamp(s.At)
	if err != nil {
		return pool.Entry{}, err
	}
	entry := pool.Entry{Node: id, At: at, Seq: s.Seq, Late: s.Late}
	if origin != "" {
		var o store.Origin
		if err := json.Unmarshal([]byte(origin), &o); err != nil {
			return pool.Entry{}, err
		}
		if entry.Origin, err = o.ToOrigin(); err != nil {
			return pool.Entry{}, err
		}
	}
	var h storedHubHappening
	if err := json.Unmarshal(s.What, &h); err != nil {
		return pool.Entry{}, err
	}
	if entry.What, err = h.toHappening(); err != errNotTheHubs {
		return entry, err
	}
	var what store.Happening
	if err := json.Unmarshal(s.What, &what); err != nil {
		return pool.Entry{}, err
	}
	fromNode, err := store.Entry{At: s.At, What: what}.ToEntry()
	entry.What = fromNode.What
	return entry, err
}

// errNotTheHubs reports a stored happening of a kind the Hub doesn't
// write, which is a node's.
var errNotTheHubs = errors.New("not a kind of entry the Hub writes")

func (s storedHubHappening) toHappening() (any, error) {
	stamp := func(v string) (time.Time, error) { return store.Unstamp(v) }
	switch s.Kind {
	case connectionGap:
		since, err := stamp(s.Since)
		return pool.ConnectionGap{Since: since}, err
	case hubStarted:
		alive, err := stamp(s.Alive)
		return pool.HubStarted{Alive: alive}, err
	case connectionRefused:
		reason, err := s.Refusal.toError()
		if err == nil && reason == nil {
			err = errors.New("a stored refusal entry has no refusal")
		}
		return pool.ConnectionRefused{Reason: reason}, err
	case enrollment:
		return pool.Enrollment{Name: s.Name, Target: s.Target}, nil
	case removalStarted:
		return pool.RemovalStarted{}, nil
	case tombstoned:
		return pool.Tombstoned{}, nil
	case lostTarget:
		return pool.LostTarget{Target: s.Target}, nil
	case regainedTarget:
		return pool.RegainedTarget{Target: s.Target}, nil
	case runnerVersionChanged:
		return pool.RunnerVersionChanged{Target: s.Target, From: s.From, To: s.To}, nil
	case codeMade:
		return pool.CodeMade{Code: s.Code}, nil
	case codeRevoked:
		return pool.CodeRevoked{Code: s.Code}, nil
	case addedTarget:
		return pool.AddedTarget{Target: s.Target}, nil
	case removedTarget:
		return pool.RemovedTarget{Target: s.Target}, nil
	case renamed:
		return pool.Renamed{From: s.From, To: s.To}, nil
	case retargeted:
		return pool.Retargeted{From: s.From, To: s.To}, nil
	case labelsChanged:
		return pool.LabelsChanged{From: s.FromLabels, To: s.ToLabels}, nil
	case poolLabelChanged:
		return pool.PoolLabelChanged{From: s.From, To: s.To}, nil
	case connectionAdded:
		return pool.GitHubConnectionAdded{Account: s.Account}, nil
	case keepHistoryChanged:
		return pool.KeepHistoryChanged{From: s.FromDays, To: s.ToDays}, nil
	case signedIn:
		return pool.SignedIn{Address: s.Address}, nil
	case signInRefused:
		return pool.SignInRefused{Login: s.Login, Address: s.Address}, nil
	}
	return nil, errNotTheHubs
}

// decoder reads stored forms, keeping the first error, and naming the
// row it came from.
type decoder struct {
	path string
	err  error
}

func (d *decoder) fail(table, key string, err error) {
	if d.err == nil && err != nil {
		d.err = fmt.Errorf("reading %s %q from %s: %w", table, key, d.path, err)
	}
}

func value[T comparable](d *decoder, table, key string, c store.Codes[T], s string) T {
	v, err := c.Value(s)
	d.fail(table, key, err)
	return v
}

func (d *decoder) time(table, key, s string) time.Time {
	t, err := store.Unstamp(s)
	d.fail(table, key, err)
	return t
}

func (d *decoder) hash(table, key, s string) [32]byte {
	var h [32]byte
	if s == "" {
		return h
	}
	b, err := hex.DecodeString(s)
	if err == nil && len(b) != len(h) {
		err = fmt.Errorf("a hash has %d bytes", len(b))
	}
	d.fail(table, key, err)
	copy(h[:], b)
	return h
}

// load reads the Hub's state from db.
func load(db *store.DB) (Hub, error) {
	d := &decoder{path: db.Path()}
	tables := map[string][]store.Row{}
	for _, name := range tableNames {
		rows, err := db.Load(name)
		if err != nil {
			return Hub{}, err
		}
		tables[name] = rows
	}
	read := func(table string, r store.Row, v any) {
		d.fail(table, r.Key, json.Unmarshal(r.Body, v))
	}
	var h Hub
	for _, r := range tables["settings"] {
		var s storedSettings
		read("settings", r, &s)
		h.Settings = Settings{
			HubID:         s.HubID,
			BaseURL:       s.BaseURL,
			Listener:      Listener{Mode: value(d, "settings", r.Key, listenerModes, s.Listener.Mode), Address: s.Listener.Address},
			PoolLabel:     s.PoolLabel,
			DefaultTarget: s.DefaultTarget,
			Operator:      s.Operator,
			Setup:         value(d, "settings", r.Key, steps, s.Setup),
			SetupCode:     SetupCode{Hash: d.hash("settings", r.Key, s.SetupCode.Hash), Expires: d.time("settings", r.Key, s.SetupCode.Expires), Reassign: s.SetupCode.Reassign},
			Connecting:    Account{Kind: value(d, "settings", r.Key, accountKinds, s.Connecting.Kind), Login: s.Connecting.Login},
		}
		h.Pool.DefaultTarget, h.Pool.KeepHistory = s.DefaultTarget, s.KeepHistory
		h.Pool.Alive = d.time("settings", r.Key, s.Alive)
		for login, at := range s.SignInsRefused {
			if h.Pool.SignInsRefused == nil {
				h.Pool.SignInsRefused = map[string]time.Time{}
			}
			h.Pool.SignInsRefused[login] = d.time("settings", r.Key, at)
		}
	}
	for _, r := range tables["connections"] {
		var s storedConnection
		read("connections", r, &s)
		h.Connections = append(h.Connections, Connection{
			Account: r.Key, Kind: value(d, "connections", r.Key, kinds, s.Kind), AppID: s.AppID, AppSlug: s.AppSlug,
			Installation: s.Installation, ClientID: s.ClientID, KeyFile: s.KeyFile, SecretFile: s.SecretFile,
		})
	}
	for _, r := range tables["targets"] {
		var s storedTarget
		read("targets", r, &s)
		h.Targets = append(h.Targets, Target{
			Key: r.Key, Connection: s.Connection,
			GitHub: github.Target{Owner: s.Owner, Repo: s.Repo, RepoID: s.RepoID, Group: s.Group},
			Added:  d.time("targets", r.Key, s.Added),
		})
		if h.Versions.Targets == nil {
			h.Versions.Targets = map[string]runnerapp.Target{}
		}
		v := runnerapp.Version{Version: s.Version.Version}
		if len(s.Version.SHA256) > 0 {
			v.SHA256 = map[runnerapp.Platform]string{}
			for p, sum := range s.Version.SHA256 {
				v.SHA256[runnerapp.Platform(p)] = sum
			}
		}
		h.Versions.Targets[r.Key] = runnerapp.Target{Version: v}
		if h.Pool.Targets == nil {
			h.Pool.Targets = map[string]pool.Target{}
		}
		target := h.Targets[len(h.Targets)-1]
		h.Pool.Targets[r.Key] = pool.Target{Scope: target.Scope(), RunnerApp: v, Lost: d.time("targets", r.Key, s.Lost)}
	}
	for _, r := range tables["nodes"] {
		var s storedNode
		read("nodes", r, &s)
		mirror, err := s.Mirror.ToNode()
		d.fail("nodes", r.Key, err)
		remove, err := s.Remove.ToRemove()
		d.fail("nodes", r.Key, err)
		var mismatch pool.VersionMismatch
		if s.Mismatch != nil {
			m, err := s.Mismatch.toError()
			mismatch, _ = m.(pool.VersionMismatch)
			if err == nil && mismatch == (pool.VersionMismatch{}) {
				err = fmt.Errorf("%q isn't a stored version mismatch", s.Mismatch.Kind)
			}
			d.fail("nodes", r.Key, err)
		}
		n := pool.Node{
			ID: r.Key, Name: s.Name, Key: s.Key, Target: s.Target,
			Connected: s.Connected, ConnectedAt: d.time("nodes", r.Key, s.ConnectedAt), Dropped: s.Dropped,
			Seen: d.time("nodes", r.Key, s.Seen), Platform: runnerapp.Platform(s.Platform), Mirror: mirror,
			Sent: s.Sent, NotConfirmed: s.NotConfirmed, Stored: s.Stored,
			Removing: s.Removing, Remove: remove, Removed: d.time("nodes", r.Key, s.Removed),
			Refused: d.time("nodes", r.Key, s.Refused), Mismatch: mismatch, WrongKey: d.time("nodes", r.Key, s.WrongKey),
			KeyInUse: d.time("nodes", r.Key, s.KeyInUse),
		}
		for _, at := range s.Replaced {
			n.Replaced = append(n.Replaced, d.time("nodes", r.Key, at))
		}
		h.Pool.Nodes = append(h.Pool.Nodes, n)
		if len(s.Labels) > 0 {
			if h.Labels == nil {
				h.Labels = map[string][]string{}
			}
			h.Labels[r.Key] = s.Labels
		}
	}
	for _, r := range tables["codes"] {
		var s storedCode
		read("codes", r, &s)
		h.Pool.Codes = append(h.Pool.Codes, pool.Code{
			ID: r.Key, Hash: d.hash("codes", r.Key, s.Hash), Made: d.time("codes", r.Key, s.Made), Used: s.Used, Revoked: s.Revoked,
		})
	}
	for _, r := range tables["runners"] {
		var s storedRecord
		read("runners", r, &s)
		h.Records.Runners = append(h.Records.Runners, registration.Record{
			Name: r.Key, Node: s.Node, Slot: s.Slot, Target: s.Target, ID: s.ID,
			Phase: value(d, "runners", r.Key, recordPhases, s.Phase),
			Lost:  s.Lost, Revive: value(d, "runners", r.Key, revivePhases, s.Revive), Retiring: s.Retiring, Deleting: s.Deleting, Relay: s.Relay,
			Attempts: s.Attempts, RetryAt: d.time("runners", r.Key, s.RetryAt),
		})
	}
	for _, r := range tables["registration_nodes"] {
		var s storedConnectionState
		read("registration_nodes", r, &s)
		if h.Records.Connected == nil {
			h.Records.Connected = map[string]registration.Connection{}
		}
		h.Records.Connected[r.Key] = registration.Connection(s)
	}
	return h, d.err
}

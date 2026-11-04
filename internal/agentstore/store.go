// Package agentstore is the agent's store: its node in memory, which is
// the copy it acts on, its SQLite database, which is the durable copy,
// and its node key beside it. The decisions behind it are in "Define
// what the Hub and the agent persist",
// recorded as ADR 0010.
//
// The agent opens its store once at start, which loads everything, and
// hands it every event. Apply applies the event to the node and commits
// the result, with any change to the agent's settings, in one
// transaction. Only then does it return the effects the agent must carry
// out: the wire, the OS and timers.
package agentstore

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/store"
)

// The files the store keeps in the agent's state folder.
const (
	// File is the agent's database.
	File = "agent.db"
	// KeyFile is the node key: the Ed25519 private key the node proves
	// itself with. It never leaves the machine, and the database never
	// holds it.
	KeyFile = "node.key"
)

// Settings are what the agent keeps beside its node.
type Settings struct {
	// Node is the node's ID, which the Hub gave it, or empty while the
	// machine isn't enrolled.
	Node string
	// Hub is the Hub the node is enrolled in.
	Hub Hub
	// Sweep is the saved sweep set: the shared places each reset
	// sweeps, which the sweep's walk computes.
	Sweep []string
	// Pending is the SHA-256 hash of the enrollment code of an enroll
	// under way, or zero if there's none. The agent keeps it only to
	// tell a retry from a new code: a retry reuses the node key.
	Pending [sha256.Size]byte
}

// Hub is the Hub a node is enrolled in, as its enrollment code named it.
type Hub struct {
	ID  string
	URL string
	// Pin is the Hub's key that the code pinned, or empty for a Hub with
	// a publicly trusted certificate.
	Pin node.Pin
}

// An Event is something the agent applies: a node.Event, or one of this
// package's own. The Hub's acceptance of an enroll comes as Accepted,
// never as a bare node.EnrollAccepted.
type Event any

// Accepted is the Hub's acceptance of an enroll: the node.EnrollAccepted
// the node applies, with the node's ID and its Hub, which the agent keeps
// once the node is enrolled.
type Accepted struct {
	EnrollAccepted node.EnrollAccepted
	Node           string
	Hub            Hub
}

// SetHubURL re-points the node at its Hub's new URL.
type SetHubURL struct{ URL string }

// SetSweep saves the sweep set.
type SetSweep struct{ Paths []string }

// A Store is the agent's state, open in memory and on disk. Only the
// agent service opens it, and it applies one event at a time.
type Store struct {
	db       *store.DB
	dir      string
	node     node.Node
	settings Settings
	// broken is the error of the commit that failed, after which nothing
	// more is applied.
	broken error
}

// Open opens the agent's store in dir, its state folder, creating it if
// it doesn't exist, and loads everything. It refuses a database that's
// corrupt or unreadable, or that a newer Silkpool wrote, naming the
// file, and migrates an older one forward.
//
// It then tidies the node key: it deletes a node key that neither an
// enrolled node nor an enroll under way holds, and forgets an enroll
// under way whose node key is gone. The agent then applies what dead
// runners' logs say, and node.Started, before anything else.
func Open(dir string) (*Store, error) {
	db, err := store.Open(filepath.Join(dir, File), migrations, tooNew)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, dir: dir}
	if err := s.open(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) open() error {
	var err error
	if s.node, s.settings, err = load(s.db); err != nil {
		return err
	}
	_, err = os.Stat(s.keyFile())
	switch {
	case err == nil && !s.node.Enrolled && s.settings.Pending == [sha256.Size]byte{}:
		if err := os.Remove(s.keyFile()); err != nil {
			return fmt.Errorf("deleting a node key nothing holds: %w", err)
		}
	case errors.Is(err, os.ErrNotExist) && s.settings.Pending != [sha256.Size]byte{}:
		settings := s.settings
		settings.Pending = [sha256.Size]byte{}
		if err := s.commit(s.node, settings); err != nil {
			return err
		}
		s.settings = settings
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("reading the node key: %w", err)
	}
	return nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// Node returns the node as of the latest commit. The caller must treat
// it as read-only.
func (s *Store) Node() node.Node { return s.node }

// Settings returns the agent's settings as of the latest commit. The
// caller must treat them as read-only.
func (s *Store) Settings() Settings { return s.settings }

func (s *Store) keyFile() string { return filepath.Join(s.dir, KeyFile) }

// Key returns the node key, or an error wrapping os.ErrNotExist if
// there's none.
func (s *Store) Key() (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(s.keyFile())
	if err != nil {
		return nil, fmt.Errorf("reading the node key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("the node key %s isn't a PEM private key", s.keyFile())
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("reading the node key %s: %w", s.keyFile(), err)
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the node key %s isn't an Ed25519 key", s.keyFile())
	}
	return ed, nil
}

// Enrolling readies an enroll with the enrollment code whose secret
// hashes to code, before the agent applies node.Enroll: it returns the
// node key to enroll with, which it has written, with code kept as the
// enroll under way. A retry with the same code gets the same key, and a
// different code discards it for a new one, so two nodes never share a
// key. It's refused while the machine is enrolled.
func (s *Store) Enrolling(code [sha256.Size]byte) (ed25519.PrivateKey, error) {
	if s.broken != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrBroken, s.broken)
	}
	if s.node.Enrolled {
		return nil, node.ErrEnrolled
	}
	if code == s.settings.Pending {
		if key, err := s.Key(); err == nil {
			return key, nil
		}
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := s.writeKey(key); err != nil {
		return nil, err
	}
	settings := s.settings
	settings.Pending = code
	if err := s.commit(s.node, settings); err != nil {
		s.broken = err
		return nil, err
	}
	s.settings = settings
	return key, nil
}

// writeKey writes key as the node key, mode 0600, replacing any other in
// one rename, and makes it durable before returning.
func (s *Store) writeKey(key ed25519.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, KeyFile+".*")
	if err != nil {
		return fmt.Errorf("writing the node key: %w", err)
	}
	// CreateTemp makes the file mode 0600.
	defer os.Remove(tmp.Name())
	if err := pem.Encode(tmp, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the node key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the node key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the node key: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.keyFile()); err != nil {
		return fmt.Errorf("writing the node key: %w", err)
	}
	return syncDir(s.dir)
}

// Apply applies e at now, commits the result in one transaction, and
// returns the effects the agent must then carry out, in order.
//
// A wipe commits the node that isn't enrolled, and settings with no
// node, Hub or enroll under way, and only then deletes the node key.
//
// If the commit fails, Apply returns the error and no effects, and the
// store keeps the state of the last commit and refuses every later
// event: a store error ends the service, and restarting it is the only
// way back.
func (s *Store) Apply(e Event, now time.Time) ([]node.Effect, error) {
	if s.broken != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrBroken, s.broken)
	}
	n, settings := s.node, s.settings
	var effects []node.Effect
	switch e := e.(type) {
	case node.EnrollAccepted:
		return nil, fmt.Errorf("agentstore: apply Accepted, with the node's ID and its Hub, not a bare %T", e)
	case Accepted:
		n, effects = n.Apply(e.EnrollAccepted, now)
		if n.Enrolled && !s.node.Enrolled {
			settings.Node, settings.Hub, settings.Pending = e.Node, e.Hub, [sha256.Size]byte{}
		}
	case node.Event:
		n, effects = n.Apply(e, now)
	case SetHubURL:
		settings.Hub.URL = e.URL
	case SetSweep:
		settings.Sweep = slices.Clone(e.Paths)
	default:
		return nil, fmt.Errorf("agentstore: %T isn't an event the agent applies", e)
	}
	wiped := slices.ContainsFunc(effects, func(e node.Effect) bool { return e == node.Wipe{} })
	if wiped {
		settings.Node, settings.Hub, settings.Pending = "", Hub{}, [sha256.Size]byte{}
	}
	if err := s.commit(n, settings); err != nil {
		s.broken = err
		return nil, err
	}
	s.node, s.settings = n, settings
	if wiped {
		if err := os.Remove(s.keyFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.broken = fmt.Errorf("deleting the node key after the wipe: %w", err)
			return nil, s.broken
		}
	}
	return effects, nil
}

// syncDir makes a rename in dir durable. Windows can't flush a folder:
// NTFS journals the rename itself.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flushing %s: %w", dir, err)
	}
	return nil
}

// migrations make the agent's schema, one version each. A change to the
// stored form adds one, and a golden file of the version it makes.
var migrations = []store.Migration{createV1}

// tooNew is what the operator can do about a database a newer Silkpool
// wrote. The copy of the state folder made before upgrading isn't a way
// back for a node: the agent numbers each event it reports, and the
// Hub drops any whose number it has already stored.
const tooNew = "install that version again, and don't put back the copy of the state folder made before upgrading: " +
	"the Hub drops any event whose number it has already stored, so it would drop the events this node records next"

func createV1(tx *sql.Tx) error {
	for _, name := range []string{"settings", "node"} {
		if _, err := tx.Exec(`CREATE TABLE ` + name + ` (key TEXT PRIMARY KEY, pos INTEGER NOT NULL, body TEXT NOT NULL) STRICT`); err != nil {
			return err
		}
	}
	return nil
}

// The keys of the tables' one row each.
const (
	settingsKey = "agent"
	nodeKey     = "node"
)

type storedSettings struct {
	Node    string   `json:"node,omitempty"`
	HubID   string   `json:"hub_id,omitempty"`
	HubURL  string   `json:"hub_url,omitempty"`
	Pin     string   `json:"pin,omitempty"`
	Sweep   []string `json:"sweep,omitempty"`
	Pending string   `json:"pending,omitempty"`
}

// commit saves n and settings in one transaction. The node's runner
// application isn't kept: the Hub sends it on every connection, and
// node.Started brings the installs the agent finds.
func (s *Store) commit(n node.Node, settings Settings) error {
	n.RunnerApp, n.Installed, n.Discarded = node.RunnerApp{}, "", false
	stored, err := store.FromNode(n)
	if err != nil {
		return err
	}
	nodeBody, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	ss := storedSettings{
		Node: settings.Node, HubID: settings.Hub.ID, HubURL: settings.Hub.URL, Pin: string(settings.Hub.Pin),
		Sweep: settings.Sweep,
	}
	if settings.Pending != [sha256.Size]byte{} {
		ss.Pending = hex.EncodeToString(settings.Pending[:])
	}
	settingsBody, err := json.Marshal(ss)
	if err != nil {
		return err
	}
	return s.db.Commit(func(tx *store.Tx) error {
		if err := tx.Save("settings", []store.Row{{Key: settingsKey, Body: settingsBody}}); err != nil {
			return err
		}
		return tx.Save("node", []store.Row{{Key: nodeKey, Body: nodeBody}})
	})
}

// load reads the node and the settings from db.
func load(db *store.DB) (node.Node, Settings, error) {
	var n node.Node
	var settings Settings
	fail := func(table string, err error) error {
		return fmt.Errorf("reading %s from %s: %w", table, db.Path(), err)
	}
	rows, err := db.Load("settings")
	if err != nil {
		return n, settings, err
	}
	for _, r := range rows {
		var ss storedSettings
		if err := json.Unmarshal(r.Body, &ss); err != nil {
			return n, settings, fail("settings", err)
		}
		settings = Settings{Node: ss.Node, Hub: Hub{ID: ss.HubID, URL: ss.HubURL, Pin: node.Pin(ss.Pin)}, Sweep: ss.Sweep}
		if ss.Pending != "" {
			b, err := hex.DecodeString(ss.Pending)
			if err == nil && len(b) != sha256.Size {
				err = fmt.Errorf("the pending code's hash has %d bytes", len(b))
			}
			if err != nil {
				return n, settings, fail("settings", err)
			}
			copy(settings.Pending[:], b)
		}
	}
	if rows, err = db.Load("node"); err != nil {
		return n, settings, err
	}
	for _, r := range rows {
		var stored store.Node
		if err := json.Unmarshal(r.Body, &stored); err != nil {
			return n, settings, fail("node", err)
		}
		if n, err = stored.ToNode(); err != nil {
			return n, settings, fail("node", err)
		}
	}
	return n, settings, nil
}

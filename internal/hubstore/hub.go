// Package hubstore is the Hub's store: the Hub's state in memory, which
// is the copy it acts on, and its SQLite database, which is the durable
// copy. The decisions behind it are in "Define what the Hub and the
// agent persist",
// recorded as ADR 0010.
//
// The Hub opens its store once at start, which loads everything, and
// hands it every event, from its nodes, its operator, GitHub and its
// clock. Apply applies the event to every core it reaches (package
// pool, package registration and package runnerapp), the effects that
// pass between them, the history entries the effects ask to keep, and
// the entries for the operator's changes to what the store keeps itself
// (the pool label, custom labels and GitHub connections), and commits
// them all in one transaction. Only then does it return the
// effects the Hub must carry out: the wire, GitHub and timers. Nothing
// reads the database while the Hub runs, except history.
package hubstore

import (
	"crypto/sha256"
	"time"

	"github.com/meschoening/silkpool/internal/github"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/registration"
	"github.com/meschoening/silkpool/internal/runnerapp"
)

// Hub is the Hub's state: its settings, its GitHub connections and
// targets, and the cores' state.
type Hub struct {
	Settings Settings
	// Connections are the Hub's GitHub connections, in the order they
	// were added.
	Connections []Connection
	// Targets are the targets the operator has added, the Hub's
	// allowlist, in the order they were added. Each target's runner
	// version is in Versions.
	Targets []Target
	// Pool is the Hub's nodes and codes. Its DefaultTarget is the one in
	// Settings, its Targets hold each of Targets' scope and runner
	// version, and its Protocol is set by each Started. It holds the
	// history setting, which pool.SetKeepHistory sets.
	Pool pool.Pool
	// Labels are each node's custom labels, by node ID.
	Labels map[string][]string
	// Records are the Hub's registration records.
	Records registration.Records
	// Versions are the targets' runner versions. Only each target's
	// Version is stored: the rest lasts until the Hub restarts.
	Versions runnerapp.Targets
}

// Settings are the Hub's own settings. There's no config file: init
// writes them, and later commands change them through the Hub.
type Settings struct {
	// HubID is the Hub's ID, which nodes sign their connections with.
	HubID   string
	BaseURL string
	// Listener is how the Hub's one listener serves TLS, and where.
	Listener Listener
	// PoolLabel is the label every runner of the Hub carries.
	PoolLabel string
	// DefaultTarget is the target a new node starts with, or empty.
	DefaultTarget string
	// Operator is the operator's GitHub user ID, or 0 until the first
	// sign-in records it.
	Operator int64
	// Setup is the latest setup step the operator has finished, or
	// empty before the first.
	Setup Step
	// SetupCode is the one-time setup code, or zero once it's spent.
	SetupCode SetupCode
	// Connecting is the GitHub account setup connects, chosen at its
	// account step, or zero before it.
	Connecting Account
}

// An Account is a GitHub account a connection serves: its kind and its
// login. A personal account's login comes with its App, since setup
// can't know who the operator is on GitHub until then.
type Account struct {
	Kind  Kind
	Login string
}

// A Listener is the Hub's listener: its mode, and the address it listens
// on.
type Listener struct {
	Mode    ListenerMode
	Address string
}

// A ListenerMode is how the listener serves TLS (ADR 0004).
type ListenerMode string

// The listener's modes.
const (
	// SelfSigned serves the Hub's own self-signed certificate, which
	// enrollment codes pin.
	SelfSigned ListenerMode = "self-signed"
	// CertificateFiles serves certificate files the operator provides.
	CertificateFiles ListenerMode = "certificate-files"
	// BehindProxy listens on loopback behind a TLS-terminating proxy.
	BehindProxy ListenerMode = "behind-proxy"
)

// A Step is a step of the Hub's first-run setup in the browser.
type Step string

// The setup steps, in order.
const (
	StepChecks    Step = "checks"
	StepAccount   Step = "account"
	StepCreateApp Step = "create-app"
	StepSignIn    Step = "sign-in"
	StepInstall   Step = "install"
	StepEnable    Step = "enable"
	StepDone      Step = "done"
)

// A SetupCode is the one-time setup code: the SHA-256 hash of its
// secret, and when it expires. The Hub keeps only the hash.
type SetupCode struct {
	Hash    [sha256.Size]byte
	Expires time.Time
	// Reassign reports a code from setup-code --reassign, whose next
	// GitHub sign-in becomes the operator, in place of the one recorded.
	Reassign bool
}

// A Connection is one GitHub connection: an account and the private App
// it owns and has installed.
type Connection struct {
	// Account is the GitHub account's login, which keys the connection.
	Account string
	Kind    Kind
	AppID   int64
	AppSlug string
	// Installation is the App's installation ID on the account, or 0
	// until the App is installed.
	Installation int64
	// ClientID signs the operator in through the App, with the client
	// secret in SecretFile.
	ClientID string
	// KeyFile and SecretFile name the App's private key and client
	// secret, files in the Hub's state folder: the database holds no
	// secret.
	KeyFile, SecretFile string
}

// A Kind is the kind of account a connection serves.
type Kind string

// The kinds of account.
const (
	Personal     Kind = "personal"
	Organization Kind = "organization"
)

// A Target is one target the operator added.
type Target struct {
	// Key is the target as the Hub keys it, in the pool, the records and
	// the runner versions.
	Key string
	// Connection is the account of the target's connection.
	Connection string
	// GitHub is the repository, or the organization's runner group.
	GitHub github.Target
	Added  time.Time
}

// Scope returns what t covers, which its nodes need to tell their own
// runs from others'.
func (t Target) Scope() node.Scope { return node.Scope{Owner: t.GitHub.Owner, Repo: t.GitHub.Repo} }

// An Event is something the Hub applies: a pool.Event, a
// registration.Event, a runnerapp.Event, or one of this package's own.
//
// The pool's Started, Tick, Connected, Disconnected, Reported and
// TargetLost reach the registration records too, and its Started and
// Tick the runner versions, so the Hub applies those and never the other
// cores' own. A Connected reaches the records only if the pool accepts
// it, and a Confirmed, which makes the node a tombstone and so ends its
// connection, reaches them as Disconnected.
//
// This package's own events are the operator's changes, from init,
// setup, the dashboard or the Hub's CLI, and each carries its origin for
// history. A change that history records writes one entry, unless it
// changes nothing.
type Event any

// SetSettings replaces the Hub's settings, the pool's default target
// among them, from Origin. A change of the pool label is written, and
// retires every node's runners, which carry the old one.
type SetSettings struct {
	Settings Settings
	Origin   node.Origin
}

// PutConnection adds a connection, or replaces the one with its account,
// from Origin. A connection added is written.
type PutConnection struct {
	Connection Connection
	Origin     node.Origin
}

// PutTarget adds a target, or replaces the one with its key, from
// Origin. A new target is added to the runner versions too, which check
// it at once, and the pool learns what it covers and writes it. While
// the Hub has no default target, the target becomes it.
type PutTarget struct {
	Target Target
	Origin node.Origin
}

// DropTarget drops a target, and its runner version and the pool's
// record of it with it, from Origin. The pool writes it. It's answered
// with pool.ChangeRefused, a TargetInUse, while a node that isn't a
// tombstone uses the target. Dropping the default target makes the first
// target left the default, or none.
type DropTarget struct {
	Key    string
	Origin node.Origin
}

// SetLabels sets a node's custom labels, from Origin. If they change,
// they're written, and the node's runners carry labels it no longer has,
// so the records retire them.
type SetLabels struct {
	Node   string
	Labels []string
	Origin node.Origin
}

// An Effect is something the Hub must carry out after Apply has
// committed: a pool.Effect, a registration.Effect or a runnerapp.Effect,
// except the ones the store carries out itself, or a WakeAt. The store
// carries out the pool's DeleteRegistrations, RetireRunners, Store, Prune
// and RunnerVersionRefused and the runner versions' Changed, and merges
// the cores' WakeAts into its own. It adds to the pool's AnswerRemoved
// the node's runners in lost targets, from the records.
type Effect any

// WakeAt asks the Hub to apply a pool.Tick at At. It comes last,
// whenever any core has something to do later, and replaces any earlier
// WakeAt.
type WakeAt struct{ At time.Time }

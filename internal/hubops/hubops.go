// Package hubops is the Hub's operations layer: every change and read
// the operator makes, from the Hub's CLI over its socket (package
// hubsocket) and from the dashboard, which share it. The decisions
// behind it are in "Define what the Hub and the agent persist",
// "Name the CLI and define its commands"
// and "Define the registration scope model and labels".
//
// Each operation takes a request and returns its reply, or a *Refusal
// naming the rule that refused it, in which case nothing changed. Any
// other error means the operation couldn't be carried out: the Hub
// stopped, or GitHub didn't answer a read a target needs. A change to
// what's already so changes nothing, and its reply says so. The replies
// are the --json automation contract, and change only by adding fields.
//
// The operations change the Hub only by applying its events, from the
// origin they're given, so the Hub stays the only writer of its store.
// Changes of the Hub's settings read them, change a field and apply them
// whole, so the Ops serializes its changes: every change of the settings
// goes through it.
package hubops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/github"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wire"
)

// Hub is the running Hub the operations act on: package hub's.
type Hub interface {
	// Apply applies an event from the operator, and returns its effects.
	Apply(ctx context.Context, e hubstore.Event) ([]hubstore.Effect, error)
	// State returns the Hub's state as of its latest commit.
	State(ctx context.Context) (hubstore.Hub, error)
	// Diagnostics asks a connected node for its diagnostics report, and
	// waits for it. A node that isn't connected, or whose connection
	// ends first, fails with diagnostics.ErrNotConnected.
	Diagnostics(ctx context.Context, node string) (diagnostics.Report, error)
}

// GitHub reads what a GitHub connection offers as targets, and makes
// setup's calls: package hubgithub's.
type GitHub interface {
	Repositories(ctx context.Context, c hubstore.Connection) ([]github.Repository, error)
	RunnerGroups(ctx context.Context, c hubstore.Connection) ([]github.Group, error)
	Group(ctx context.Context, c hubstore.Connection, t github.Target) (github.Group, error)

	// Clock reads GitHub's clock, and fails if GitHub's API can't be
	// reached.
	Clock(ctx context.Context) (time.Time, error)
	// CreateApp converts the code of an App the manifest's flow created,
	// keeps its private key and client secret in the Hub's state folder,
	// and returns the connection to its owner, not yet installed, with
	// the owner.
	CreateApp(ctx context.Context, code string) (hubstore.Connection, github.Account, error)
	// Installation looks up an installation with the JWT of c's App.
	Installation(ctx context.Context, c hubstore.Connection, id int64) (github.Installation, error)
	// SignIn exchanges the code of a sign-in through c's App, and returns
	// who signed in.
	SignIn(ctx context.Context, c hubstore.Connection, code, redirect string) (github.Account, error)
}

// Config is what the operations need.
type Config struct {
	Hub    Hub
	GitHub GitHub
	// Web is github.com, where setup sends the browser, or empty for
	// github.Web. Only tests set it, to a fake's URL.
	Web string
	// Pin is the pin of the Hub's own key, which enrollment codes carry
	// when it serves its self-signed certificate, or empty.
	Pin node.Pin
	// Version is the Hub's version, and Protocol the version of the
	// Hub-agent protocol it speaks.
	Version  string
	Protocol int
	// Now reads the clock, or is nil for time.Now.
	Now func() time.Time
	// NewCode returns a new enrollment code for the Hub at url, or is nil
	// for wire.NewCode. NewSecret returns a new setup code, or is nil for
	// a random one, and NewID a code's short ID, or nil for a random one.
	// Only tests set them.
	NewCode   func(url string, pin node.Pin) (wire.Code, error)
	NewSecret func() secret.Value
	NewID     func() string
}

// Ops carries out the operations. It's safe for concurrent use.
type Ops struct {
	c Config
	// changing serializes the changes, each of which reads the Hub's
	// state before it applies an event.
	changing sync.Mutex
	// mu guards commands, the last command each node was sent, by its ID.
	mu       sync.Mutex
	commands map[string]*sentCommand
}

// New returns the operations over c.Hub.
func New(c Config) *Ops {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.NewCode == nil {
		c.NewCode = wire.NewCode
	}
	if c.NewSecret == nil {
		c.NewSecret = newSecret
	}
	if c.NewID == nil {
		c.NewID = newID
	}
	return &Ops{c: c}
}

// A Refusal refuses an operation, naming the rule: Code is stable, for
// scripts, and Message says it in words. Nothing changed.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Suggestion is a name to use instead, for name-taken.
	Suggestion string `json:"suggestion,omitempty"`
}

func (r *Refusal) Error() string { return r.Message }

func refuse(code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, args...)}
}

// state returns the Hub's state.
func (o *Ops) state(ctx context.Context) (hubstore.Hub, error) { return o.c.Hub.State(ctx) }

// apply applies e, and returns the refusal among its effects, if any.
func (o *Ops) apply(ctx context.Context, e hubstore.Event) error {
	effects, err := o.c.Hub.Apply(ctx, e)
	if err != nil {
		return err
	}
	for _, eff := range effects {
		if r, ok := eff.(pool.ChangeRefused); ok {
			return refusalOf(r.Reason)
		}
	}
	return nil
}

// refusalOf returns the refusal for a core's reason.
func refusalOf(reason error) *Refusal {
	var taken pool.RenameTaken
	var invalid node.InvalidName
	var unknown pool.UnknownTarget
	var inUse hubstore.TargetInUse
	switch {
	case errors.As(reason, &taken):
		return &Refusal{Code: "name-taken", Message: reason.Error(), Suggestion: taken.Suggestion}
	case errors.As(reason, &invalid):
		return &Refusal{Code: "invalid-name", Message: reason.Error()}
	case errors.As(reason, &unknown):
		return &Refusal{Code: "unknown-target", Message: reason.Error()}
	case errors.As(reason, &inUse):
		return &Refusal{Code: "target-in-use", Message: reason.Error()}
	case errors.Is(reason, node.ErrNoBudget):
		return &Refusal{Code: "no-budget", Message: reason.Error()}
	}
	return &Refusal{Code: "refused", Message: reason.Error()}
}

// changeSettings applies f's change to the Hub's settings, from origin,
// under the Ops' lock, and reports whether it changed them. f returns a
// refusal to change nothing.
func (o *Ops) changeSettings(ctx context.Context, from node.Origin, f func(*hubstore.Settings) error) (bool, error) {
	h, err := o.state(ctx)
	if err != nil {
		return false, err
	}
	s := h.Settings
	if err := f(&s); err != nil {
		return false, err
	}
	if s == h.Settings {
		return false, nil
	}
	return true, o.apply(ctx, hubstore.SetSettings{Settings: s, Origin: from})
}

// findNode returns the node named name, ignoring case, among those that
// aren't tombstones.
func findNode(h hubstore.Hub, name string) (pool.Node, error) {
	i := slices.IndexFunc(h.Pool.Nodes, func(n pool.Node) bool { return n.Removed.IsZero() && strings.EqualFold(n.Name, name) })
	if i < 0 {
		return pool.Node{}, refuse("unknown-node", "the Hub has no node named %s", name)
	}
	return h.Pool.Nodes[i], nil
}

// findTarget returns the target written as written, ignoring case.
func findTarget(h hubstore.Hub, written string) (hubstore.Target, error) {
	i := slices.IndexFunc(h.Targets, func(t hubstore.Target) bool { return strings.EqualFold(t.Key, strings.TrimSpace(written)) })
	if i < 0 {
		return hubstore.Target{}, refuse("unknown-target", "the Hub has no target %s: add it with target add first", written)
	}
	return h.Targets[i], nil
}

// newID returns a code's short ID: six random lowercase letters and
// digits, which aren't secret.
func newID() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// newSecret returns a new setup code: secret.CodePrefix, then 26 random
// base32 characters, 130 bits, so the log scrubber finds it.
func newSecret() secret.Value { return secret.New(secret.CodePrefix + rand.Text()) }

// SetupCodeHash is the hash the Hub keeps of a setup code, the whole of
// what the setup link carries, and matches one by.
func SetupCodeHash(code secret.Value) [sha256.Size]byte { return sha256.Sum256([]byte(code.Reveal())) }

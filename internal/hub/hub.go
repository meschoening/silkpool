// Package hub is the Hub service's loop and its end of the Hub-agent
// connection. The decisions behind it are in "Define node enrollment,
// removal, identity and Hub-agent transport"
// and "Define what the Hub and the agent persist".
//
// One goroutine, Run, takes every event the Hub learns, from its
// agents, its operator, GitHub and its clock, and hands each to the
// store, which applies every core the event reaches and commits. Only
// then does it carry out the effects: messages to agents, answers to
// enrollments, GitHub's work, and the next wake. A store error ends the
// service.
//
// Run also polls GitHub for what the cores leave to the Hub's loop: the
// comparison of stale registrations with each target's runners, every
// ListEvery, and whether the Hub can still see its targets, every
// CheckTargetsEvery. Both run at start too. The cores ask for every other call
// themselves, the runner version's hourly check included.
//
// Handler serves the agents: the WebSocket at wire.ConnectPath, and the
// enrollment endpoint at wire.EnrollPath. The listener it's served on
// belongs to the caller.
package hub

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/registration"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wire"
)

// GitHub carries out the Hub's GitHub work: registration's
// GenerateJITConfig and DeleteRunner, runnerapp's CheckDownloads and
// FetchLatest, and the Hub's own polls, ListRunners and CheckTargets.
// The Hub calls Carry on a goroutine of its own for each, with its state
// as of the commit that asked for the work, which Carry must treat as
// read-only. It applies the events Carry returns, in order: for a JIT
// config, a Minted. Carry returns once ctx ends.
type GitHub interface {
	Carry(ctx context.Context, h hubstore.Hub, work hubstore.Effect) []hubstore.Event
}

// ListRunners asks GitHub for the runners registered in Target, and
// returns them as registration.Listed, for the comparison.
type ListRunners struct{ Target string }

// CheckTargets asks GitHub whether the Hub can still see the targets of
// the connection with Account, and returns pool.TargetLost for each
// target it has positive evidence the Hub no longer sees, and
// pool.TargetRegained for each lost target it sees again.
type CheckTargets struct{ Account string }

// The polls' cadences. ListEvery is how often the Hub compares its stale
// registrations with GitHub's list of each target's runners, which finds
// those whose mint lost its answer. CheckTargetsEvery is how often it checks it
// can still see its targets: a lost target's runners must stop soon, as
// they may serve a new owner.
const (
	ListEvery         = 5 * time.Minute
	CheckTargetsEvery = time.Minute
)

// Minted is registration.Generated with the JIT config GitHub generated,
// which the Hub keeps in memory only, until it sends it with the grant.
type Minted struct {
	Generated registration.Generated
	JITConfig secret.Value
}

// Applied is one event the loop applied: the event, the effects it
// carried out, and the Hub's state after it.
type Applied struct {
	Event   hubstore.Event
	Effects []hubstore.Effect
	Hub     hubstore.Hub
}

// Config is what a Hub needs besides its store.
type Config struct {
	// Protocol is the protocol version the Hub speaks, or zero for
	// wire.Protocol.
	Protocol int
	// GitHub carries out GitHub effects. Without one they're dropped.
	GitHub GitHub
	// Log is where the Hub logs, or nil for slog.Default.
	Log *slog.Logger
	// OnApply, if set, is told of each event the loop applies, on the
	// loop's goroutine, after its effects are carried out.
	OnApply func(Applied)
	// Crash, if set, is asked after each commit whether to end the
	// service right there, before carrying out any effect, as a crash
	// would. Run then returns ErrCrashed. Only tests set it.
	Crash func(hubstore.Event) bool
	// Now, if set, reads the clock each event is applied at, in place of
	// time.Now. Only tests set it.
	Now func() time.Time
}

// ErrCrashed is what Run returns after Config.Crash ended it.
var ErrCrashed = errors.New("hub: crashed on purpose")

// ErrStopped refuses what reaches a Hub that isn't running.
var ErrStopped = errors.New("hub: the Hub isn't running")

// A Hub is the Hub service. New makes it, Run runs it once, and Handler
// serves its agents while it runs.
type Hub struct {
	store *hubstore.Store
	c     Config
	log   *slog.Logger
	jobs  chan func()
	// running is closed once Run has applied Started, and done once Run
	// has returned.
	running, done chan struct{}
	ctx           context.Context
	// tasks are the goroutines Run waits for before it returns.
	tasks sync.WaitGroup

	// The loop's own state.
	err   error
	links map[string]*link
	jit   map[string]secret.Value
	wake  *time.Timer
	// listing and checking hold the polls that are out: the targets
	// being listed, and the connections whose targets are being checked.
	listing, checking map[string]bool
	// enrolling is the enrollment being applied, which its effects
	// answer.
	enrolling *enrollment
	// asks are the asks for diagnostics reports that are out, by ID, and
	// asked counts them, for their IDs.
	asks  map[string]*ask
	asked int
}

// New returns a Hub over s, which must be freshly opened: Run applies
// pool.Started first.
func New(s *hubstore.Store, c Config) *Hub {
	if c.Protocol == 0 {
		c.Protocol = wire.Protocol
	}
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Hub{
		store: s, c: c, log: log,
		jobs:     make(chan func()),
		running:  make(chan struct{}),
		done:     make(chan struct{}),
		links:    map[string]*link{},
		jit:      map[string]secret.Value{},
		listing:  map[string]bool{},
		checking: map[string]bool{},
		asks:     map[string]*ask{},
	}
}

// Run runs the Hub until ctx ends or the store fails, which it returns.
// It applies pool.Started first, and ends every connection when it
// returns.
func (h *Hub) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	h.ctx = ctx
	h.wake = time.NewTimer(time.Hour)
	h.wake.Stop()
	list, check := time.NewTicker(ListEvery), time.NewTicker(CheckTargetsEvery)
	defer func() {
		cancel()
		h.wake.Stop()
		list.Stop()
		check.Stop()
		for _, l := range h.links {
			l.conn.Close()
		}
		close(h.done)
		h.tasks.Wait()
	}()
	h.apply(pool.Started{Protocol: h.c.Protocol})
	close(h.running)
	h.checkTargets()
	h.compare()
	for h.err == nil {
		select {
		case <-ctx.Done():
			return nil
		case job := <-h.jobs:
			job()
		case <-h.wake.C:
			h.apply(pool.Tick{})
		case <-list.C:
			h.compare()
		case <-check.C:
			h.checkTargets()
		}
	}
	return h.err
}

// do runs f on the loop's goroutine, and reports false if the Hub isn't
// running.
func (h *Hub) do(ctx context.Context, f func()) bool {
	select {
	case <-h.running:
	case <-h.done:
		return false
	case <-ctx.Done():
		return false
	}
	select {
	case h.jobs <- f:
		return true
	case <-h.done:
		return false
	case <-ctx.Done():
		return false
	}
}

// Apply applies e, from the Hub's operator or another source, carries
// out its effects, and returns them: a pool.NotSent among them refuses a
// command for a node that isn't connected.
func (h *Hub) Apply(ctx context.Context, e hubstore.Event) ([]hubstore.Effect, error) {
	type result struct {
		effects []hubstore.Effect
		err     error
	}
	done := make(chan result, 1)
	if !h.do(ctx, func() {
		effects := h.apply(e)
		done <- result{effects, h.err}
	}) {
		return nil, ErrStopped
	}
	select {
	case r := <-done:
		return r.effects, r.err
	case <-h.done:
		return nil, ErrStopped
	}
}

// State returns the Hub's state as of its latest commit, read on the
// loop's goroutine. The caller must treat it as read-only.
func (h *Hub) State(ctx context.Context) (hubstore.Hub, error) {
	var state hubstore.Hub
	if !h.query(ctx, func() { state = h.store.Hub() }) {
		return hubstore.Hub{}, ErrStopped
	}
	return state, nil
}

// History returns the Hub's history, oldest first, read on the loop's
// goroutine, which is the only one that uses the store.
func (h *Hub) History(ctx context.Context) ([]hubstore.HistoryEntry, error) {
	var entries []hubstore.HistoryEntry
	var err error
	if !h.query(ctx, func() { entries, err = h.store.History() }) {
		return nil, ErrStopped
	}
	return entries, err
}

// apply applies e through the store, then carries out its effects, and
// returns them. A store error ends the service.
func (h *Hub) apply(e hubstore.Event) []hubstore.Effect { return h.applyThen(e, nil) }

// applyThen is apply, calling first, if it's set, with the effects
// between the commit and carrying them out.
func (h *Hub) applyThen(e hubstore.Event, first func([]hubstore.Effect)) []hubstore.Effect {
	if h.err != nil {
		return nil
	}
	effects, err := h.store.Apply(e, h.c.Now())
	if err != nil {
		h.err = err
		h.log.Error("the Hub's store failed, so the Hub stops", "event", fmt.Sprintf("%T", e), "err", err)
		return nil
	}
	if h.c.Crash != nil && h.c.Crash(e) {
		h.err = ErrCrashed
		return nil
	}
	h.logged("applied an event", "event", e)
	if first != nil {
		first(effects)
	}
	h.wake.Stop()
	for _, eff := range effects {
		h.logged("carried out an effect", "effect", eff)
		h.carry(eff)
	}
	if h.c.OnApply != nil {
		h.c.OnApply(Applied{Event: e, Effects: effects, Hub: h.store.Hub()})
	}
	return effects
}

// carry carries out one effect.
func (h *Hub) carry(eff hubstore.Effect) {
	switch eff := eff.(type) {
	case pool.Send:
		h.send(eff.Node, eff.Message)
	case pool.Acknowledge:
		h.send(eff.Node, node.Acknowledged{Through: eff.Through})
	case registration.Granted:
		config, ok := h.jit[eff.Runner]
		delete(h.jit, eff.Runner)
		if !ok {
			h.log.Error("granting a runner whose JIT config the Hub doesn't hold", "node", eff.Node, "runner", eff.Runner)
			h.send(eff.Node, node.GrantFailed{Slot: eff.Slot, Reason: node.GitHubUnavailable, Detail: "the Hub lost the runner's JIT config"})
			return
		}
		h.send(eff.Node, wire.Granted{Slot: eff.Slot, Runner: eff.Runner, JITConfig: config})
	case registration.GrantFailed:
		h.send(eff.Node, node.GrantFailed{Slot: eff.Slot, Reason: eff.Reason, Detail: eff.Detail})
	case registration.Retired:
		h.send(eff.Node, node.Retired{Runner: eff.Runner, Status: eff.Status, TargetVisible: eff.TargetVisible})
	case registration.StopLostRunners:
		h.send(eff.Node, node.StopLostRunners{Runners: eff.Runners})
	case pool.AnswerRemoved, pool.AnswerRefused:
		// hello answered the connection before the effects.
	case pool.Enrolled, pool.EnrollRefused:
		if h.enrolling != nil {
			h.enrolling.effects = append(h.enrolling.effects, eff)
		}
	case pool.NotSent:
		// Returned to whoever applied the command.
	case registration.DeleteRunner:
		// A runner deleted before its grant never needs its JIT config.
		delete(h.jit, eff.Runner)
		h.github(eff)
	case registration.GenerateJITConfig, runnerapp.CheckDownloads, runnerapp.FetchLatest:
		h.github(eff)
	case hubstore.WakeAt:
		h.wake.Reset(max(eff.At.Sub(h.c.Now()), 0))
	default:
		h.log.Error("an effect the Hub doesn't carry out", "effect", fmt.Sprintf("%T", eff))
	}
}

// github has GitHub carry out work on a goroutine of its own, with the
// Hub's state now, and applies what comes of it. A poll is no longer out
// once its events are applied.
func (h *Hub) github(work hubstore.Effect) {
	if h.c.GitHub == nil {
		h.log.Warn("no GitHub to carry out an effect", "effect", fmt.Sprintf("%T", work))
		return
	}
	ctx, state := h.ctx, h.store.Hub()
	h.tasks.Go(func() {
		events := h.c.GitHub.Carry(ctx, state, work)
		h.do(ctx, func() {
			switch w := work.(type) {
			case ListRunners:
				delete(h.listing, w.Target)
				if newlyStale(state, h.store.Hub(), w.Target) {
					// GitHub may have read the list before that record's
					// runner was registered, so it can't tell it's gone.
					h.list(w.Target)
					return
				}
			case CheckTargets:
				delete(h.checking, w.Account)
			}
			for _, e := range events {
				if m, ok := e.(Minted); ok {
					h.jit[m.Generated.Runner] = m.JITConfig
					e = m.Generated
				}
				h.apply(e)
			}
		})
	})
}

// compare lists the runners of each target that holds a stale record,
// unless a list of it is still out, for the comparison.
func (h *Hub) compare() {
	if h.c.GitHub == nil {
		return
	}
	for _, rec := range h.store.Hub().Records.Runners {
		if rec.Phase == registration.Stale {
			h.list(rec.Target)
		}
	}
}

// list lists target's runners for the comparison, unless a list of it
// is still out.
func (h *Hub) list(target string) {
	if !h.listing[target] {
		h.listing[target] = true
		h.github(ListRunners{Target: target})
	}
}

// newlyStale reports whether target holds a stale record now that it
// didn't hold then.
func newlyStale(then, now hubstore.Hub, target string) bool {
	was := map[string]bool{}
	for _, rec := range then.Records.Runners {
		if rec.Target == target && rec.Phase == registration.Stale {
			was[rec.Name] = true
		}
	}
	for _, rec := range now.Records.Runners {
		if rec.Target == target && rec.Phase == registration.Stale && !was[rec.Name] {
			return true
		}
	}
	return false
}

// checkTargets checks the targets of each connection that has any,
// unless a check of them is still out.
func (h *Hub) checkTargets() {
	if h.c.GitHub == nil {
		return
	}
	state := h.store.Hub()
	for _, c := range state.Connections {
		has := slices.ContainsFunc(state.Targets, func(t hubstore.Target) bool { return t.Connection == c.Account })
		if has && !h.checking[c.Account] {
			h.checking[c.Account] = true
			h.github(CheckTargets{Account: c.Account})
		}
	}
}

// logged logs one line at info about an event applied or an effect
// carried out, as kind: its type, and the node, runner, target, slot or
// name it's about. Nothing else of it is logged, so no secret it holds
// can be.
func (h *Hub) logged(msg, kind string, v any) {
	if !h.log.Enabled(h.ctx, slog.LevelInfo) {
		return
	}
	args := []any{kind, fmt.Sprintf("%T", v)}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Struct {
		for _, name := range aboutFields {
			f := rv.FieldByName(name)
			switch {
			case !f.IsValid() || f.IsZero():
			case f.Kind() == reflect.String && !keyFields[name]:
				args = append(args, strings.ToLower(name), secret.Scrub(f.String()))
			case f.Kind() == reflect.String || f.Kind() == reflect.Int:
				args = append(args, strings.ToLower(name), f.Interface())
			}
		}
	}
	h.log.Info(msg, args...)
}

// aboutFields are the fields of events and effects the log names them
// by: none holds a secret. Most are the Hub's own, such as a node's ID
// or a runner's name, or a node's name under node.CheckName's rule, and
// can't hold a secret's pattern. But one can be what a machine sent
// before the Hub checked it, such as the name an enrollment asks for or
// a runner a node asks to retire, so a string is scrubbed (package
// secret) as it's copied into the log. keyFields, the target keys and
// the GitHub accounts targets are chosen by, stay as they are, as
// everywhere the Hub names them.
var (
	aboutFields = []string{"Node", "Runner", "Target", "Slot", "Name", "Account"}
	keyFields   = map[string]bool{"Target": true, "Account": true}
)

// newID returns a new node ID, drawn at random.
func newID() string { return rand.Text() }

// random returns a random number for a runner's name.
func random() uint32 { return mrand.Uint32() }

// nodeOf returns the pool's node id, if it holds one.
func (h *Hub) nodeOf(id string) (pool.Node, bool) {
	nodes := h.store.Hub().Pool.Nodes
	i := slices.IndexFunc(nodes, func(n pool.Node) bool { return n.ID == id })
	if i < 0 {
		return pool.Node{}, false
	}
	return nodes[i], true
}

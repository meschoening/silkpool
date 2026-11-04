// Package agent is the agent service's loop and its end of the
// Hub-agent connection. The decisions behind it are in "Define node
// enrollment, removal, identity and Hub-agent transport"
// and "Define what the Hub and the agent persist".
//
// One goroutine, Run, takes every event the agent learns, from its Hub,
// its administrators, the machine and its clock, and hands each to the
// store, which applies it to the node and commits. Only then does it
// carry out the effects: messages to the Hub, work on the machine, and
// the next wake. A store error ends the service.
//
// While the machine is enrolled, the agent keeps one connection to its
// Hub, dialing it again after each drop with a backoff, and after each
// "removed" too, until the wipe.
package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/meschoening/silkpool/internal/agentstore"
	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wire"
)

// OS carries out the node's effects on the machine: its slots, their
// accounts and runners, and the runner application's installs.
type OS interface {
	// Find reports what the agent finds on the machine as it starts. It
	// may go on reporting through report, from any goroutine, about the
	// runners it found alive, such as how their listeners end: the agent
	// applies those reports after node.Started, in the order they're made.
	Find(ctx context.Context, report func(node.Event)) (Found, error)
	// Carry starts carrying out an effect and returns at once. It
	// reports what comes of it through report, from any goroutine, and
	// the agent applies the reports in the order they're made. The
	// effects are node.PrepareSlot, a Listen, node.Interrupt, node.Kill,
	// node.ResetSlot, node.DeleteAccount, node.InstallRunnerApp,
	// node.DeleteInstall, node.Wipe and node.ApplyBudget.
	Carry(effect any, report func(node.Event))
}

// Found is what the agent finds on the machine as it starts: what dead
// runners' logs say, which it applies first, the runners whose
// listeners still run, and the runner application's installs, which it
// applies with node.Started.
type Found struct {
	Logs            []node.Event
	Alive, Installs []string
}

// Listen is node.StartListener with the runner's JIT config, which the
// agent keeps in memory only, from the Hub's grant to the start.
type Listen struct {
	node.StartListener
	JITConfig secret.Value
}

// Applied is one event the loop applied: the event, the effects it
// carried out, and the node and the agent's settings after it.
type Applied struct {
	Event    agentstore.Event
	Effects  []node.Effect
	Node     node.Node
	Settings agentstore.Settings
}

// Config is what an agent needs besides its store.
type Config struct {
	// Protocol is the protocol version the agent speaks, or zero for
	// wire.Protocol.
	Protocol int
	// Platform is the platform the agent was built for.
	Platform runnerapp.Platform
	// Machine is what an enroll tells the Hub about the machine. Its
	// Zone is the OS's time zone by its IANA name, which the node is
	// enrolled in.
	Machine wire.Machine
	// Resources is what the node needs to know of the machine for its
	// budget: its RAM and logical CPU count, and whether it's a Mac,
	// which has none. The agent tells the node as it starts and enrolls.
	Resources node.Machine
	// OS carries out the node's effects on the machine. Without one
	// they're dropped.
	OS OS
	// Roots verifies a Hub with a publicly trusted certificate, or nil
	// for the system's trusted roots. A pinned Hub is verified by its
	// pin alone.
	Roots *x509.CertPool
	// Dial, if set, makes the network connections to the Hub.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Log is where the agent logs, or nil for slog.Default.
	Log *slog.Logger
	// OnApply, if set, is told of each event the loop applies, on the
	// loop's goroutine, after its effects are carried out.
	OnApply func(Applied)
	// Crash, if set, is asked after each commit whether to end the
	// service right there, before carrying out any effect, as a crash
	// would. Run then returns ErrCrashed. Only tests set it.
	Crash func(agentstore.Event) bool
	// FreeDisk, if set, returns the free space where the agent keeps its
	// state, for the diagnostics report.
	FreeDisk func() (uint64, error)
	// LogTail, if set, returns the last n lines of the agent's own log,
	// for the diagnostics report.
	LogTail func(n int) ([]string, error)
}

// ErrCrashed is what Run returns after Config.Crash ended it.
var ErrCrashed = errors.New("agent: crashed on purpose")

// ErrStopped refuses what reaches an agent that isn't running.
var ErrStopped = errors.New("agent: the agent isn't running")

// ErrEnrolling refuses an enroll while another is under way.
var ErrEnrolling = errors.New("another enroll is under way: wait for it to finish")

// An Agent is the agent service. New makes it, and Run runs it once.
type Agent struct {
	store *agentstore.Store
	c     Config
	log   *slog.Logger
	jobs  chan func()
	inbox inbox
	// running is closed once Run has applied Started, and done once Run
	// has returned.
	running, done chan struct{}
	ctx           context.Context
	tasks         sync.WaitGroup

	// The loop's own state.
	err   error
	queue []agentstore.Event
	wake  *time.Timer
	// dialer keeps the connection to the Hub, and link is the one it
	// holds now, or nil.
	dialer *dialer
	link   *wire.Conn
	// refusal is why the latest connection failed, while it's the Hub
	// refusing the agent: VersionMismatch, HubIdentityChanged or
	// WrongNodeKey.
	refusal node.Reason
	// asked and retiring are the runner requests and retirements sent on
	// the link and not yet answered, and fromHub the commands the Hub
	// sent on it that haven't been replied to.
	asked    map[int]bool
	retiring map[string]bool
	fromHub  map[string]bool
	// reported is the latest report sent on the link.
	reported []byte
	jit      map[string]secret.Value
	waiters  map[string][]chan node.Reply
	// enrolling is the enroll under way at the Hub, or nil.
	enrolling *enrolling
	// hubSince is when the node last connected to its Hub or lost its
	// connection, or when the agent started.
	hubSince time.Time
	// changed is closed, and replaced, after each event the loop
	// applies, which wakes the control channel's watchers.
	changed chan struct{}
	// checks are the slots' latest exposure checks since the agent
	// started, and clock the clock's offset from the Hub's as the agent
	// last measured it, for the diagnostics report.
	checks map[int]diagnostics.Check
	clock  diagnostics.Clock
}

// New returns an agent over s, which must be freshly opened: Run applies
// what it finds on the machine and node.Started first.
func New(s *agentstore.Store, c Config) *Agent {
	if c.Protocol == 0 {
		c.Protocol = wire.Protocol
	}
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	return &Agent{
		store: s, c: c, log: log,
		jobs:     make(chan func()),
		inbox:    inbox{ready: make(chan struct{}, 1)},
		running:  make(chan struct{}),
		done:     make(chan struct{}),
		asked:    map[int]bool{},
		retiring: map[string]bool{},
		fromHub:  map[string]bool{},
		jit:      map[string]secret.Value{},
		waiters:  map[string][]chan node.Reply{},
		changed:  make(chan struct{}),
		checks:   map[int]diagnostics.Check{},
	}
}

// Run runs the agent until ctx ends or the store fails, which it
// returns. It applies what it finds on the machine and node.Started
// first, and ends its connection when it returns.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	a.ctx = ctx
	a.wake = time.NewTimer(time.Hour)
	a.wake.Stop()
	defer func() {
		cancel()
		a.wake.Stop()
		if a.link != nil {
			a.link.Close()
		}
		close(a.done)
		a.tasks.Wait()
	}()
	var found Found
	if a.c.OS != nil {
		var err error
		if found, err = a.c.OS.Find(ctx, a.inbox.put); err != nil {
			return fmt.Errorf("agent: finding what the machine holds: %w", err)
		}
	}
	a.hubSince = time.Now()
	for _, e := range found.Logs {
		a.apply(e)
	}
	a.apply(node.Started{Alive: found.Alive, Installs: found.Installs, Machine: a.c.Resources})
	close(a.running)
	for a.err == nil {
		select {
		case <-ctx.Done():
			return nil
		case job := <-a.jobs:
			job()
		case <-a.inbox.ready:
			for _, e := range a.inbox.take() {
				a.apply(e)
			}
		case <-a.wake.C:
			a.apply(node.Tick{})
		}
	}
	return a.err
}

// do runs f on the loop's goroutine, and reports false if the agent
// isn't running.
func (a *Agent) do(ctx context.Context, f func()) bool {
	select {
	case <-a.running:
	case <-a.done:
		return false
	case <-ctx.Done():
		return false
	}
	select {
	case a.jobs <- f:
		return true
	case <-a.done:
		return false
	case <-ctx.Done():
		return false
	}
}

// query runs f on the loop's goroutine and waits for it, reporting false
// if the agent isn't running.
func (a *Agent) query(ctx context.Context, f func()) bool {
	ran := make(chan struct{})
	if !a.do(ctx, func() { f(); close(ran) }) {
		return false
	}
	select {
	case <-ran:
		return true
	case <-a.done:
		return false
	}
}

// Apply applies e, from the machine's administrator or another source,
// carries out its effects, and returns them.
func (a *Agent) Apply(ctx context.Context, e agentstore.Event) ([]node.Effect, error) {
	var effects []node.Effect
	var err error
	if !a.query(ctx, func() { effects, err = a.apply(e), a.err }) {
		return nil, ErrStopped
	}
	return effects, err
}

// Do applies a node.Command, node.Remove or node.Dismiss from the
// machine itself, and waits for its reply. The reply to the node's own
// remove comes once the Hub has started the removal.
func (a *Agent) Do(ctx context.Context, e node.Event) (node.Reply, error) {
	var id string
	switch e := e.(type) {
	case node.Command:
		id = e.ID
	case node.Remove:
		id = e.ID
	case node.Dismiss:
		id = e.ID
	default:
		return node.Reply{}, fmt.Errorf("agent: %T isn't a command", e)
	}
	reply := make(chan node.Reply, 1)
	if !a.query(ctx, func() {
		a.waiters[id] = append(a.waiters[id], reply)
		a.apply(e)
	}) {
		return node.Reply{}, ErrStopped
	}
	return a.await(ctx, reply)
}

// await waits for a reply.
func (a *Agent) await(ctx context.Context, reply chan node.Reply) (node.Reply, error) {
	select {
	case r := <-reply:
		return r, nil
	case <-ctx.Done():
		return node.Reply{}, ctx.Err()
	case <-a.done:
		if a.err != nil {
			return node.Reply{}, a.err
		}
		return node.Reply{}, ErrStopped
	}
}

// apply applies e, then each event the agent queued for itself while
// carrying out its effects, and returns e's effects.
func (a *Agent) apply(e agentstore.Event) []node.Effect {
	effects := a.applyOne(e)
	for len(a.queue) > 0 && a.err == nil {
		next := a.queue[0]
		a.queue = a.queue[1:]
		a.applyOne(next)
	}
	return effects
}

// later queues an event the agent applies itself once the current one's
// effects are carried out.
func (a *Agent) later(e agentstore.Event) { a.queue = append(a.queue, e) }

// applyOne applies e through the store, reports the node to the Hub if
// it changed, carries out the effects, and keeps the connection as the
// settings now say. A store error ends the service.
func (a *Agent) applyOne(e agentstore.Event) []node.Effect {
	if a.err != nil {
		return nil
	}
	now := time.Now()
	effects, err := a.store.Apply(e, now)
	if err != nil {
		a.fail(err)
		return nil
	}
	a.log.Info("applied " + describe(e))
	if e, ok := e.(node.Event); ok {
		if c, ok := diagnostics.CheckOf(e, now); ok {
			a.checks[c.Slot] = c
		}
	}
	if a.c.Crash != nil && a.c.Crash(e) {
		a.err = ErrCrashed
		return nil
	}
	a.report()
	switch e.(type) {
	case agentstore.SetHubURL, agentstore.SetSweep:
		// They don't reach the node, so its wake stands.
	default:
		a.wake.Stop()
	}
	for _, eff := range effects {
		a.carry(eff)
	}
	a.connect()
	if a.c.OnApply != nil {
		a.c.OnApply(Applied{Event: e, Effects: effects, Node: a.store.Node(), Settings: a.store.Settings()})
	}
	close(a.changed)
	a.changed = make(chan struct{})
	return effects
}

// fail ends the service with a store error.
func (a *Agent) fail(err error) {
	a.err = err
	a.log.Error("the agent's store failed, so the agent stops", "err", err)
}

// carry carries out one effect.
func (a *Agent) carry(eff node.Effect) {
	a.log.Info("carrying out " + describe(eff))
	switch eff := eff.(type) {
	case node.Reply:
		if a.fromHub[eff.ID] {
			delete(a.fromHub, eff.ID)
			a.toHub(eff)
		}
		for _, w := range a.waiters[eff.ID] {
			w <- eff
		}
		delete(a.waiters, eff.ID)
		if a.enrolling != nil && a.enrolling.id == eff.ID {
			a.enrolling = nil
		}
	case node.RequestRunner:
		if a.link == nil {
			reason, detail := a.unreachable()
			a.later(node.GrantFailed{Slot: eff.Slot, Reason: reason, Detail: detail})
			return
		}
		a.asked[eff.Slot] = true
		a.toHub(eff)
	case node.RetireRunner:
		// A runner retired before it started never uses its JIT config.
		delete(a.jit, eff.Runner)
		if a.link == nil {
			a.later(node.Retired{Runner: eff.Runner})
			return
		}
		a.retiring[eff.Runner] = true
		a.toHub(eff)
	case node.Deliver, node.RemoveAtHub, node.ConfirmRemoved:
		a.toHub(eff)
	case node.EnrollAtHub:
		a.enrollAtHub(eff)
	case node.StartListener:
		config := a.jit[eff.Runner]
		delete(a.jit, eff.Runner)
		a.machine(Listen{StartListener: eff, JITConfig: config})
	case node.Wipe:
		// The slots' checks were of the accounts the wipe deletes.
		clear(a.checks)
		a.machine(eff)
	case node.PrepareSlot, node.Interrupt, node.Kill, node.ResetSlot, node.DeleteAccount,
		node.InstallRunnerApp, node.DeleteInstall, node.ApplyBudget:
		a.machine(eff)
	case node.WakeAt:
		a.wake.Reset(max(time.Until(eff.At), 0))
	case node.AutoResumed, node.NotCleanedUp:
		// The node's history and Applied carry them.
	default:
		a.log.Error("an effect the agent doesn't carry out", "effect", fmt.Sprintf("%T", eff))
	}
}

// WrongKeyFix is the detail of a slot's WrongNodeKey failure: what the
// machine's administrator does about it. Node keys don't rotate (ADR
// 0004), so the machine enrolls again as a new node. A machine the Hub
// refuses never hears "removed", so its node is removed here with
// --force. Only the operator can tell whether the Hub's node is still
// another machine's, as when this machine's state was copied from it;
// if not, removing it in the Hub frees its name for the new enroll.
const WrongKeyFix = "This machine's node key isn't the one the Hub holds for this node, " +
	"as when the machine's state was restored or copied. Node keys can't be changed, " +
	"so enroll the machine again: remove the node here with --force, and in the Hub too " +
	"unless another machine is this node, then enroll with a new code."

// unreachable returns why a request can't reach the Hub, and its
// detail: the Hub's refusal of the agent, or HubUnreachable.
func (a *Agent) unreachable() (node.Reason, string) {
	switch a.refusal {
	case 0:
		return node.HubUnreachable, ""
	case node.WrongNodeKey:
		return node.WrongNodeKey, WrongKeyFix
	}
	return a.refusal, ""
}

// toHub sends the Hub a message on the link, if there is one.
func (a *Agent) toHub(m any) {
	if a.link == nil {
		return
	}
	b, err := wire.EncodeToHub(m)
	if err != nil {
		a.log.Error("couldn't encode a message for the Hub", "err", err)
		return
	}
	if !a.link.Send(b) {
		a.log.Warn("the connection couldn't take a message for the Hub", "message", fmt.Sprintf("%T", m))
	}
}

// report sends the Hub the node, if it's connected and the node changed
// since the last report on this connection.
func (a *Agent) report() {
	n := a.store.Node()
	if a.link == nil || !n.Enrolled {
		return
	}
	b, err := wire.EncodeToHub(n)
	if err != nil {
		a.log.Error("couldn't encode the node's report", "err", err)
		return
	}
	if slices.Equal(b, a.reported) {
		return
	}
	a.reported = b
	a.link.Send(b)
}

// machine hands an effect to the OS.
func (a *Agent) machine(eff any) {
	if a.c.OS == nil {
		a.log.Warn("no OS to carry out an effect", "effect", fmt.Sprintf("%T", eff))
		return
	}
	a.c.OS.Carry(eff, a.inbox.put)
}

// received applies a message from the Hub on the link. Messages from a
// connection that has since ended are dropped.
func (a *Agent) received(conn *wire.Conn, m any) {
	if a.link != conn {
		return
	}
	switch m := m.(type) {
	case node.Command:
		a.fromHub[m.ID] = true
		a.apply(m)
	case node.Dismiss:
		a.fromHub[m.ID] = true
		a.apply(m)
	case node.Remove:
		a.fromHub[m.ID] = true
		a.apply(m)
	case wire.Granted:
		delete(a.asked, m.Slot)
		a.jit[m.Runner] = m.JITConfig
		a.apply(node.Granted{Slot: m.Slot, Runner: m.Runner})
	case node.GrantFailed:
		delete(a.asked, m.Slot)
		a.apply(m)
	case node.Retired:
		delete(a.retiring, m.Runner)
		a.apply(m)
	case node.HubSettings, node.StopLostRunners, node.Acknowledged:
		a.apply(m.(node.Event))
	case wire.AskDiagnostics:
		a.answer(conn, m)
	default:
		a.log.Warn("rejected a message from the Hub out of turn", "message", fmt.Sprintf("%T", m))
	}
}

// answer answers the Hub's ask for the diagnostics report on the
// connection it came on, once the report is made, off the loop. An
// answer whose connection has ended is dropped: the Hub has failed the
// ask.
func (a *Agent) answer(conn *wire.Conn, ask wire.AskDiagnostics) {
	a.log.Info("the Hub asked for the diagnostics report")
	src := a.sources()
	a.tasks.Go(func() {
		r, err := a.diagnose(src)
		if err != nil {
			a.log.Error("couldn't make the diagnostics report", "err", err)
			return
		}
		b, err := wire.EncodeToHub(wire.DiagnosticsReport{ID: ask.ID, Report: r})
		if err != nil {
			a.log.Error("couldn't encode the diagnostics report", "err", err)
			return
		}
		if !conn.Send(b) {
			a.log.Warn("the connection ended before the diagnostics report was sent")
		}
	})
}

// connected takes conn as the link, unless its dialer has since been
// stopped, and clock as the clock's offset from the Hub's.
func (a *Agent) connected(d *dialer, conn *wire.Conn, clock diagnostics.Clock) {
	if a.dialer != d {
		conn.Close()
		return
	}
	if clock != (diagnostics.Clock{}) {
		a.clock = clock
	}
	a.link, a.refusal, a.reported = conn, 0, nil
	a.hubSince = time.Now()
	clear(a.fromHub)
	a.apply(node.Connected{})
}

// ended applies the end of the link. Whatever the agent asked the Hub
// on it and hasn't heard back about has failed, as package node expects
// the agent to say.
func (a *Agent) ended(conn *wire.Conn) {
	if a.link != conn {
		return
	}
	a.link = nil
	a.hubSince = time.Now()
	clear(a.fromHub)
	for _, slot := range slices.Sorted(maps.Keys(a.asked)) {
		a.later(node.GrantFailed{Slot: slot, Reason: node.HubUnreachable})
	}
	for _, runner := range slices.Sorted(maps.Keys(a.retiring)) {
		a.later(node.Retired{Runner: runner})
	}
	clear(a.asked)
	clear(a.retiring)
	a.apply(node.Disconnected{})
}

// failed records why a connection failed.
func (a *Agent) failed(d *dialer, reason node.Reason, err error) {
	if a.dialer != d {
		return
	}
	a.refusal = 0
	if reason != node.HubUnreachable {
		a.refusal = reason
	}
	if reason == node.WrongNodeKey {
		a.log.Warn("couldn't connect to the Hub", "err", err, "fix", WrongKeyFix)
		return
	}
	a.log.Warn("couldn't connect to the Hub", "err", err)
}

// removed applies the Hub's "removed". The runners it names are in lost
// targets, which the Hub can't delete, so the node stops them first, as
// it does a lost target's while enrolled. The agent hears "removed" on
// every attempt until it wipes, and applies each the same way: the node
// takes a repeat, and a runner named again, as changing nothing.
func (a *Agent) removed(d *dialer, m wire.Removed) {
	if a.dialer != d {
		return
	}
	a.log.Info("the Hub has removed this node", "lost_runners", m.Lost)
	if len(m.Lost) > 0 {
		a.apply(node.StopLostRunners{Runners: m.Lost})
	}
	a.apply(node.Removed{})
}

// connect starts, restarts or stops the dialer as the settings say:
// while the machine is enrolled, the agent keeps a connection to its
// Hub. Having heard "removed" doesn't stop it, nor keep an agent that
// restarts from dialing: only the wipe does.
func (a *Agent) connect() {
	s, n := a.store.Settings(), a.store.Node()
	want := ""
	if n.Enrolled && s.Node != "" {
		want = fmt.Sprintf("%s\x00%s\x00%s\x00%s", s.Node, s.Hub.ID, s.Hub.URL, s.Hub.Pin)
	}
	if a.dialer != nil && a.dialer.key == want {
		return
	}
	if a.dialer != nil {
		a.dialer.stop()
		a.dialer = nil
		if a.link != nil {
			// Whatever is queued, such as a confirmation, goes first.
			a.link.Finish()
		}
	}
	if want == "" {
		return
	}
	key, err := a.store.Key()
	if err != nil {
		a.log.Error("can't connect to the Hub without the node key", "err", err)
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.dialer = &dialer{key: want, stop: cancel}
	d := a.dialer
	a.tasks.Go(func() { a.dial(ctx, d, s, key) })
}

// enrolling is an enroll under way: the administrator's code and the
// name they asked for, and the node key it enrolls with.
type enrolling struct {
	id   string
	code wire.Code
	name string
	key  ed25519.PrivateKey
}

// Enrollment is an administrator's enroll of the machine.
type Enrollment struct {
	ID     string
	Origin node.Origin
	// Code is the enrollment code, as the administrator gave it.
	Code string
	// Name is the name to ask for.
	Name string
}

// Enroll enrolls the machine with the Hub e's code names, and returns
// the reply once the Hub has answered. One enroll runs at a time, and
// one again with the same code reuses the node key, so the Hub answers a
// retry as the first. The same enroll, by its ID, sent while it's under
// way gets its reply too. A code that isn't one is refused at once.
func (a *Agent) Enroll(ctx context.Context, e Enrollment) (node.Reply, error) {
	code, err := wire.ParseCode(e.Code)
	if err != nil {
		return node.Reply{}, err
	}
	reply := make(chan node.Reply, 1)
	var refused error
	if !a.query(ctx, func() {
		if a.enrolling != nil {
			// The same enroll sent again, as the CLI does when its
			// connection drops, waits for the one under way.
			if a.enrolling.id == e.ID {
				a.waiters[e.ID] = append(a.waiters[e.ID], reply)
				return
			}
			refused = ErrEnrolling
			return
		}
		key, err := a.store.Enrolling(sha256.Sum256([]byte(code.Text().Reveal())))
		switch {
		case err == nil:
			a.enrolling = &enrolling{id: e.ID, code: code, name: e.Name, key: key}
		case !errors.Is(err, node.ErrEnrolled):
			// The node refuses an enrolled machine's enroll itself.
			a.fail(err)
			refused = err
			return
		}
		a.waiters[e.ID] = append(a.waiters[e.ID], reply)
		a.apply(node.Enroll{ID: e.ID, Origin: e.Origin, Zone: a.zone()})
	}) {
		return node.Reply{}, ErrStopped
	}
	if refused != nil {
		return node.Reply{}, refused
	}
	return a.await(ctx, reply)
}

// zone loads the OS's zone by name, or returns nil, which the node
// refuses, if it can't be loaded.
func (a *Agent) zone() *time.Location {
	name := a.c.Machine.Zone
	if name == "" || name == "Local" {
		return nil
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		a.log.Warn("couldn't load the OS's time zone", "zone", name, "err", err)
		return nil
	}
	return zone
}

// enrollAtHub sends the enroll under way to the Hub's enrollment
// endpoint, and applies its answer.
func (a *Agent) enrollAtHub(eff node.EnrollAtHub) {
	en := a.enrolling
	if en == nil || en.id != eff.ID {
		a.later(node.EnrollFailed{ID: eff.ID, Reason: errors.New("no enroll is under way")})
		return
	}
	ctx := a.ctx
	a.tasks.Go(func() {
		answer, err := a.post(ctx, en, eff.Origin)
		a.do(ctx, func() {
			switch {
			case err != nil:
				a.apply(node.EnrollFailed{ID: eff.ID, Reason: err})
			case answer.Refused != nil:
				a.apply(node.EnrollFailed{ID: eff.ID, Reason: answer.Refused})
			default:
				a.apply(agentstore.Accepted{
					EnrollAccepted: node.EnrollAccepted{ID: eff.ID, Zone: eff.Zone, Target: answer.Target, Machine: a.c.Resources},
					Node:           answer.Node,
					Hub:            agentstore.Hub{ID: answer.Hub, URL: en.code.URL, Pin: en.code.Pin},
				})
			}
		})
	})
}

// inbox holds the machine's reports until the loop applies them, in the
// order they came.
type inbox struct {
	mu     sync.Mutex
	events []node.Event
	ready  chan struct{}
}

func (in *inbox) put(e node.Event) {
	in.mu.Lock()
	in.events = append(in.events, e)
	in.mu.Unlock()
	select {
	case in.ready <- struct{}{}:
	default:
	}
}

func (in *inbox) take() []node.Event {
	in.mu.Lock()
	defer in.mu.Unlock()
	events := in.events
	in.events = nil
	return events
}

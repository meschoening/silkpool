package agentos

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os/user"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/meschoening/silkpool/internal/agent"
	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/slot"
)

// Slots is what the OS needs of a Linux node's slots, as *slot.Slots
// runs them.
type Slots interface {
	Ready(ctx context.Context, n int) node.Event
	Account(n int) string
	RunnerRoot(n int) string
	ListenerLog(runner string) string
	Start(ctx context.Context, n int, runner string, config secret.Value) error
	Stop(ctx context.Context, n int, runner string) error
	Kill(ctx context.Context, n int, runner string) error
	Wait(ctx context.Context, n int, runner string) (slot.Exit, error)
	WorkerUp(ctx context.Context, n int, runner string) (bool, error)
	Alive(ctx context.Context) ([]string, error)
	Recorded() (map[int]string, error)
	Reset(ctx context.Context, n int) []node.Event
	DeleteAccount(n int) node.Event
	SetBudget(ctx context.Context, b availability.Budget) error
}

// Installs is what the OS needs of the runner application's installs,
// as runnerinstall.Installs keeps them. Install's InstallFailed comes
// with its reason scrubbed, which the OS copies into the failure of each
// slot waiting on the install.
type Installs interface {
	Install(ctx context.Context, app node.InstallRunnerApp) node.Event
	Found() ([]string, error)
	Delete(version string) error
	Link(version, root string, uid, gid int) error
}

// Config is what the OS needs.
type Config struct {
	Slots    Slots
	Installs Installs
	// Lookup returns the UID and primary group of the account name, or is
	// nil for the OS's account database.
	Lookup func(name string) (uid, gid int, err error)
	// Log is where the OS logs what fails without an event, or nil for
	// slog.Default.
	Log *slog.Logger
	// Poll is how often a listener's log is read, or zero for
	// DefaultPoll.
	Poll time.Duration
	// WorkerPoll is how often the OS looks for a job's worker, from the
	// job's start until it's up, or zero for DefaultWorkerPoll.
	WorkerPoll time.Duration
}

// DefaultPoll is how often a listener's log is read for new lines,
// DefaultWorkerPoll how often the OS looks for a job's worker, and
// waitRetry how long the OS waits to ask systemd again how a runner
// ended when it couldn't say.
const (
	DefaultPoll       = 250 * time.Millisecond
	DefaultWorkerPoll = 100 * time.Millisecond
	waitRetry         = 5 * time.Second
)

// An OS carries out a Linux node's effects. It's an agent.OS.
type OS struct {
	c   Config
	ctx context.Context
	log *slog.Logger

	tasks sync.WaitGroup
	mu    sync.Mutex
	// slots holds each slot's queue of effects, installs the installs'
	// queue, and budgets the budgets'.
	slots    map[int]*queue
	installs queue
	budgets  queue
	// installed are the versions installed that the node may use, and
	// installing those whose install is under way, each with a channel
	// closed when it ends. failed is the latest install that failed, and
	// why, until one succeeds. wipes counts the wipes carried out: an
	// install asked for before the latest one is the wiped node's, and
	// none of the three holds it.
	installed  map[string]bool
	installing map[string]chan struct{}
	failed     node.InstallFailed
	wipes      int
	// reporting is held while an install's result is reported, so that a
	// wipe carried out meanwhile waits for it: once Carry has taken the
	// wipe, nothing more of the wiped node's installs is reported. The
	// agent's report never waits on its loop, and no report carries a
	// wipe.
	reporting sync.Mutex
}

var _ agent.OS = (*OS)(nil)

// New returns an OS that carries out effects until ctx ends. The
// runners it starts outlive it.
func New(ctx context.Context, c Config) *OS {
	if c.Lookup == nil {
		c.Lookup = lookup
	}
	if c.Poll == 0 {
		c.Poll = DefaultPoll
	}
	if c.WorkerPoll == 0 {
		c.WorkerPoll = DefaultWorkerPoll
	}
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	return &OS{
		c: c, ctx: ctx, log: log,
		slots:      map[int]*queue{},
		installed:  map[string]bool{},
		installing: map[string]chan struct{}{},
	}
}

// Wait waits, once the OS's context has ended, for everything it was
// carrying out to stop.
func (o *OS) Wait() { o.tasks.Wait() }

// Find returns what the machine holds as the agent starts: the installs,
// what the logs of the runners that died while the agent was down say,
// and the runners still alive, which it follows again from the start of
// their logs, reporting through report.
func (o *OS) Find(ctx context.Context, report func(node.Event)) (agent.Found, error) {
	installs, err := o.c.Installs.Found()
	if err != nil {
		return agent.Found{}, fmt.Errorf("can't read the runner application's installs: %w", err)
	}
	alive, err := o.c.Slots.Alive(ctx)
	if err != nil {
		return agent.Found{}, err
	}
	recorded, err := o.c.Slots.Recorded()
	if err != nil {
		return agent.Found{}, fmt.Errorf("can't read which runners the slots hold: %w", err)
	}
	o.mu.Lock()
	for _, v := range installs {
		o.installed[v] = true
	}
	o.mu.Unlock()
	found := agent.Found{Alive: alive, Installs: installs}
	for _, n := range slices.Sorted(maps.Keys(recorded)) {
		runner := recorded[n]
		if slices.Contains(alive, runner) {
			o.follow(n, runner, report)
			continue
		}
		path := o.c.Slots.ListenerLog(runner)
		l, err := readLog(path)
		if err != nil {
			o.log.Warn("couldn't read a dead runner's listener log", "runner", runner, "err", scrubbed(err))
			continue
		}
		found.Logs = append(found.Logs, l.dead(runner)...)
		o.keep(runner, path)
	}
	return found, nil
}

// Carry starts carrying out effect, and reports what comes of it.
func (o *OS) Carry(effect any, report func(node.Event)) {
	switch e := effect.(type) {
	case node.PrepareSlot:
		o.slot(e.Slot, func() { o.prepare(e.Slot, report) })
	case agent.Listen:
		o.slot(e.Slot, func() { o.start(e, report) })
	case node.Interrupt:
		o.slot(e.Slot, func() {
			if err := o.c.Slots.Stop(o.ctx, e.Slot, e.Runner); err != nil {
				o.log.Warn("couldn't interrupt a runner's listener", "slot", e.Slot, "runner", e.Runner, "err", scrubbed(err))
			}
		})
	case node.Kill:
		o.slot(e.Slot, func() {
			if err := o.c.Slots.Kill(o.ctx, e.Slot, e.Runner); err != nil {
				o.log.Warn("couldn't kill a runner's listener", "slot", e.Slot, "runner", e.Runner, "err", scrubbed(err))
			}
		})
	case node.ResetSlot:
		o.slot(e.Slot, func() {
			for _, ev := range o.c.Slots.Reset(o.ctx, e.Slot) {
				report(ev)
			}
		})
	case node.DeleteAccount:
		o.slot(e.Slot, func() { report(o.c.Slots.DeleteAccount(e.Slot)) })
	case node.InstallRunnerApp:
		o.install(e, report)
	case node.ApplyBudget:
		o.enqueue(&o.budgets, func() {
			if err := o.c.Slots.SetBudget(o.ctx, availability.Budget(e)); err != nil {
				o.log.Warn("couldn't hold the slots to the node's budget", "memory", e.Memory, "cpus", e.CPUs, "err", scrubbed(err))
			}
		})
	case node.DeleteInstall:
		o.mu.Lock()
		delete(o.installed, e.Version)
		o.mu.Unlock()
		o.enqueue(&o.installs, func() {
			// Again after the installs queued before it: the node can ask as
			// soon as it hears an install is ready, before it's usable here.
			o.mu.Lock()
			delete(o.installed, e.Version)
			o.mu.Unlock()
			o.delete(e.Version)
		})
	case node.Wipe:
		// The wiped node's installs go, those under way too, and no slot
		// prepared after the wipe waits for one or starts from one. The
		// node enrolled again hears nothing of them.
		o.reporting.Lock()
		o.mu.Lock()
		versions := slices.Collect(maps.Keys(o.installed))
		versions = slices.AppendSeq(versions, maps.Keys(o.installing))
		clear(o.installed)
		o.installing, o.failed = map[string]chan struct{}{}, node.InstallFailed{}
		o.wipes++
		o.mu.Unlock()
		o.reporting.Unlock()
		slices.Sort(versions)
		versions = slices.Compact(versions)
		o.enqueue(&o.installs, func() {
			// After the installs queued before it, so those under way when
			// the wipe was asked for are done now.
			for _, v := range versions {
				o.delete(v)
			}
		})
	default:
		o.log.Error("an effect the OS doesn't carry out", "effect", fmt.Sprintf("%T", effect))
	}
}

// slot queues job on slot n's queue.
func (o *OS) slot(n int, job func()) {
	o.mu.Lock()
	q := o.slots[n]
	if q == nil {
		q = &queue{}
		o.slots[n] = q
	}
	o.mu.Unlock()
	o.enqueue(q, job)
}

// prepare carries out PrepareSlot: the slot's account and exposure
// check, then an install of the runner application to start from.
func (o *OS) prepare(n int, report func(node.Event)) {
	e := o.c.Slots.Ready(o.ctx, n)
	if _, ok := e.(node.Prepared); !ok {
		report(e)
		return
	}
	if e := o.usable(n); e != nil {
		report(e)
	}
}

// usable returns Prepared once slot n has an install to start from, as
// soon as there is one, waiting for an install under way, or
// PrepareFailed with RunnerAppUnavailable if there's none. It returns
// nil if the OS stops meanwhile. When the node asks for the first
// install, it asks in the same Apply as the preparation, just after it:
// the install counts as under way as soon as it's carried, long before
// Ready, which runs the exposure check in a unit, gets here.
func (o *OS) usable(n int) node.Event {
	for {
		o.mu.Lock()
		if len(o.installed) > 0 {
			o.mu.Unlock()
			return node.Prepared{Slot: n}
		}
		var done chan struct{}
		for _, v := range slices.Sorted(maps.Keys(o.installing)) {
			done = o.installing[v]
			break
		}
		failed := o.failed
		o.mu.Unlock()
		switch {
		case done != nil:
		case failed.Version != "":
			return node.PrepareFailed{Slot: n, Reason: node.RunnerAppUnavailable,
				Detail: fmt.Sprintf("Couldn't install the runner application %s: %s", failed.Version, failed.Reason)}
		default:
			return node.PrepareFailed{Slot: n, Reason: node.RunnerAppUnavailable, Detail: "No runner application is installed yet"}
		}
		select {
		case <-done:
		case <-o.ctx.Done():
			return nil
		}
	}
}

// install carries out InstallRunnerApp. The install counts as under way
// from now, so that a slot being prepared waits for it. If the node
// wipes before it's done, it's the wiped node's: nothing comes of it but
// the wipe's deletion.
func (o *OS) install(e node.InstallRunnerApp, report func(node.Event)) {
	o.mu.Lock()
	done := o.installing[e.Version]
	if done == nil {
		done = make(chan struct{})
		o.installing[e.Version] = done
	}
	wipes := o.wipes
	o.mu.Unlock()
	o.enqueue(&o.installs, func() {
		defer close(done)
		result := o.c.Installs.Install(o.ctx, e)
		// The node hears of the install before any slot can be prepared
		// from it: until then, it's still under way.
		o.reporting.Lock()
		o.mu.Lock()
		wiped := o.wipes != wipes
		o.mu.Unlock()
		if !wiped {
			report(result)
		}
		o.reporting.Unlock()
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.wipes != wipes {
			return
		}
		switch r := result.(type) {
		case node.InstallReady:
			o.installed[e.Version], o.failed = true, node.InstallFailed{}
		case node.InstallFailed:
			o.failed = r
		}
		delete(o.installing, e.Version)
	})
}

// delete deletes the install of version.
func (o *OS) delete(version string) {
	if err := o.c.Installs.Delete(version); err != nil {
		o.log.Warn("couldn't delete an install of the runner application", "version", version, "err", scrubbed(err))
	}
}

// start carries out StartListener: it links the runner's root to its
// install, starts its listener with its JIT config, and follows it.
func (o *OS) start(e agent.Listen, report func(node.Event)) {
	failed := func(reason node.Reason, err error) {
		report(node.StartFailed{Runner: e.Runner, Reason: reason, Detail: sentence(err)})
	}
	if e.Version == "" {
		failed(node.RunnerAppUnavailable, errors.New("no runner application is installed yet"))
		return
	}
	uid, gid, err := o.c.Lookup(o.c.Slots.Account(e.Slot))
	if err != nil {
		failed(node.SlotCantStart, fmt.Errorf("can't find slot %d's account: %w", e.Slot, err))
		return
	}
	if err := o.c.Installs.Link(e.Version, o.c.Slots.RunnerRoot(e.Slot), uid, gid); err != nil {
		reason := node.SlotCantStart
		if errors.Is(err, fs.ErrNotExist) {
			reason = node.RunnerAppUnavailable
		}
		failed(reason, err)
		return
	}
	if err := o.c.Slots.Start(o.ctx, e.Slot, e.Runner, e.JITConfig); err != nil {
		failed(node.SlotCantStart, err)
		return
	}
	o.follow(e.Slot, e.Runner, report)
}

// follow follows runner's listener in slot n, on a goroutine of its own:
// it reports each of the listener's lines in its log as it's written,
// and from the job's start looks for its worker (watchWorker). Once the
// unit has ended and every line is read, it keeps the log scrubbed and
// reports how the listener exited. A runner whose unit is gone already
// ended unwatched, so its log says how. If the OS stops first, the
// runner runs on, and nothing more is reported.
func (o *OS) follow(n int, runner string, report func(node.Event)) {
	o.tasks.Go(func() {
		path := o.c.Slots.ListenerLog(runner)
		var (
			exit slot.Exit
			err  error
		)
		ended := make(chan struct{})
		o.tasks.Go(func() {
			defer close(ended)
			for {
				exit, err = o.c.Slots.Wait(o.ctx, n, runner)
				if err == nil || errors.Is(err, slot.ErrGone) || o.ctx.Err() != nil {
					return
				}
				// Systemd couldn't say, as when the bus hiccups: ask again.
				o.log.Warn("couldn't wait for a runner's listener", "slot", n, "runner", runner, "err", scrubbed(err))
				select {
				case <-time.After(waitRetry):
				case <-o.ctx.Done():
					return
				}
			}
		})
		t := tail{path: path}
		var watching chan struct{}
		line := func(e node.Event) {
			report(e)
			if _, ok := e.(node.JobStarted); ok && watching == nil {
				done := make(chan struct{})
				watching = done
				o.tasks.Go(func() {
					defer close(done)
					o.watchWorker(n, runner, ended, report)
				})
			}
		}
		warned := false
		read := func() {
			if err := t.read(runner, line); err != nil && !warned {
				warned = true
				o.log.Warn("couldn't read a runner's listener log", "runner", runner, "err", scrubbed(err))
			}
		}
		tick := time.NewTicker(o.c.Poll)
		defer tick.Stop()
		for {
			read()
			select {
			case <-tick.C:
				continue
			case <-ended:
			}
			break
		}
		if watching != nil {
			<-watching
		}
		if o.ctx.Err() != nil {
			return
		}
		read()
		if errors.Is(err, slot.ErrGone) {
			exit.Code = 1
			if t.log.completed {
				exit.Code = 0
			}
		}
		o.keep(runner, path)
		report(node.ListenerExited{Runner: runner, Code: exit.Code, OutOfMemory: exit.OutOfMemory})
	})
}

// watchWorker looks for the worker of runner's job in slot n every
// WorkerPoll, from the job's start, among the processes of the runner's
// unit, and reports WorkerStarted once it's up. It stops then, once the
// listener has ended, or if the OS stops.
func (o *OS) watchWorker(n int, runner string, ended <-chan struct{}, report func(node.Event)) {
	tick := time.NewTicker(o.c.WorkerPoll)
	defer tick.Stop()
	warned := false
	for {
		select {
		case <-ended:
			// The job started and the listener ended between two reads.
			return
		default:
		}
		up, err := o.c.Slots.WorkerUp(o.ctx, n, runner)
		switch {
		case err != nil && o.ctx.Err() == nil && !warned:
			warned = true
			o.log.Warn("couldn't look for a runner's worker", "slot", n, "runner", runner, "err", scrubbed(err))
		case up:
			report(node.WorkerStarted{Runner: runner})
			return
		}
		select {
		case <-tick.C:
		case <-ended:
			return
		case <-o.ctx.Done():
			return
		}
	}
}

// keep keeps the log of runner's listener, which has ended, scrubbed.
func (o *OS) keep(runner, path string) {
	if err := scrubFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		o.log.Warn("couldn't scrub a runner's listener log", "runner", runner, "err", scrubbed(err))
	}
}

// lookup returns the UID and primary group of the account name, from the
// OS's account database.
func lookup(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, err
	}
	gid, err = strconv.Atoi(u.Gid)
	return uid, gid, err
}

// sentence is err as a failure's detail: a sentence, scrubbed.
func sentence(err error) string {
	d := scrubbed(err)
	r, size := utf8.DecodeRuneInString(d)
	return string(unicode.ToUpper(r)) + d[size:]
}

// scrubbed is err's text with every secret in it scrubbed, for a
// failure's detail or the OS's log: what the machine and GitHub say in
// it isn't Silkpool's.
func scrubbed(err error) string { return secret.Scrub(err.Error()) }

// A queue runs jobs one at a time, in order, on a goroutine that lasts
// while there are jobs.
type queue struct {
	mu      sync.Mutex
	jobs    []func()
	running bool
}

// enqueue adds job to q, and starts q's goroutine unless it's running.
func (o *OS) enqueue(q *queue, job func()) {
	q.mu.Lock()
	q.jobs = append(q.jobs, job)
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.mu.Unlock()
	o.tasks.Go(func() {
		for {
			q.mu.Lock()
			if len(q.jobs) == 0 || o.ctx.Err() != nil {
				q.jobs, q.running = nil, false
				q.mu.Unlock()
				return
			}
			job := q.jobs[0]
			q.jobs = q.jobs[1:]
			q.mu.Unlock()
			job()
		}
	})
}

package slot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/meschoening/silkpool/internal/exposure"
	"github.com/meschoening/silkpool/internal/launchd"
	"github.com/meschoening/silkpool/internal/roleaccount"
	"github.com/meschoening/silkpool/internal/secret"
)

// Slots runs a Mac's slots: it makes and checks the service account they
// share, and starts, stops and watches their runners' launchd jobs (ADRs
// 0005 to 0007). Each slot's runner runs in a launchd daemon job of its
// own, as the service account, which launchd supervises, so it outlives
// the agent. Its methods are safe for concurrent use, but not on the same
// slot.
type Slots struct {
	Layout
	machine roleaccount.Machine
	ctl     launchd.Launchctl
	// ensure makes or checks the service account (roleaccount.Ensure, on
	// machine).
	ensure func(ctx context.Context, spec roleaccount.Spec, l roleaccount.Ledger, keep func(roleaccount.Ledger) error) (roleaccount.Account, error)
	// ids returns the service account's UID and GID, as Start runs it.
	ids func(ctx context.Context) (uid, gid int, err error)
	// poll is how often Wait asks launchd about a runner, the reset about
	// a job it booted out, and DeleteAccount about the account's processes.
	poll time.Duration
	// goneWait is how long DeleteAccount waits for the account's processes
	// to end once its user domain is booted out, or 0 for goneWait's. Only
	// tests set it.
	goneWait time.Duration
	// sessionType is the jobs' LimitLoadToSessionType, empty for the
	// system domain's default. Only the tests that load jobs in their
	// own account's domain over SSH set it, to Background, which that
	// domain needs.
	sessionType string
	// outputMode is the output folder's mode (output), or 0 for 0711, so
	// the service account may enter it but not list it. Only the tests
	// that stand this account in for the service account set it, to
	// 0311, which keeps the folder's owner from listing it too.
	outputMode os.FileMode

	// What the reset, the clean and DeleteAccount run, besides launchctl
	// and dscl,
	// which tests replace. Open bounds each call, as it bounds every
	// launchctl, dscl and ps it runs, by bound.
	//
	// procs lists every live process (listProcs).
	procs func(ctx context.Context) ([]proc, error)
	// killAs runs the kill helper as the account uid, with its group gid
	// (runKillHelper).
	killAs func(ctx context.Context, uid, gid int) error
	// lsDelete runs lsregister -delete as the account uid, with its group
	// gid (runLSDelete).
	lsDelete func(ctx context.Context, uid, gid int) error
	// command runs crontab and atrm (runCommand).
	command func(ctx context.Context, name string, args ...string) ([]byte, error)
	// bound is how long one command may run.
	bound time.Duration
	// varFolders is /var/folders, sfanalytics the folder of
	// /private/var/protected that holds an account's analytics, and
	// atSpool the at spool, which holds each at job as a file of its
	// submitter's (atJobs).
	varFolders, sfanalytics, atSpool string
	// idRange is the range of IDs Silkpool picks the service account's
	// from, roleaccount.First to Last: the reset and DeleteAccount act on
	// no account with a UID or GID outside it.
	idRange [2]int
	// What the exposure check and the survey read, which tests replace:
	// view reads the folders whose entries the read check probes
	// (readView), and walkRoots the folders the survey walks
	// (macWalkRoots).
	view      func() (exposure.View, error)
	walkRoots func() ([]string, error)
	// checkBound and surveyBound are how long the exposure check's job may
	// run, and the survey's.
	checkBound, surveyBound time.Duration
	// checking keeps the check's one launchd job to one check or survey at
	// a time, from loading it until its report is read, and surveying
	// keeps Ready's surveys and Resurvey to one at a time, from reading
	// the latest survey until a new one is saved.
	checking, surveying sync.Mutex
	// What WorkerUp reads, which tests replace: children lists every live
	// process's children by its parent's PID (listChildren), and program
	// the program a process runs (programOf).
	children func() (map[int][]int, error)
	program  func(pid int) (string, error)
	// log is where DeleteAccount says what it leaves.
	log *slog.Logger

	mu sync.Mutex
	// resets is closed when a slot's reset next finishes, or nil if
	// nobody waits for one.
	resets chan struct{}
	// account is held while the service account, its home or its
	// records change: as it's made or checked, as a reset marks it for
	// its clean, and throughout the clean. Whoever holds use too takes
	// use first.
	account sync.Mutex
	// use is held for reading while a runner or the exposure check's job
	// starts or runs as the service account, and for writing by the
	// account-wide clean, which kills every process of the account.
	use sync.RWMutex
	// clock is the time, or nil for time.Now, and cleaning stands in for
	// the account-wide clean's steps, or is nil for them. Only tests set
	// them.
	clock    func() time.Time
	cleaning func(context.Context) error
}

// commandBound is how long each command the slots run may take: dscl,
// launchctl, ps, crontab, at and the kill helper. launchctl bootout waits
// up to 20 s for a job's process to end after SIGTERM.
const commandBound = time.Minute

// Open returns the slots l lays out, on this Mac.
func Open(_ context.Context, l Layout) (*Slots, error) {
	s := &Slots{
		Layout:      l,
		poll:        500 * time.Millisecond,
		bound:       commandBound,
		varFolders:  "/var/folders",
		sfanalytics: "/private/var/protected/sfanalytics",
		atSpool:     "/usr/lib/cron/jobs",
		idRange:     [2]int{roleaccount.First, roleaccount.Last},
		view:        readView,
		walkRoots:   macWalkRoots,
		children:    listChildren,
		program:     programOf,
		checkBound:  checkBound,
		surveyBound: surveyBound,
		log:         slog.Default(),
	}
	s.ctl = launchd.Launchctl{Run: func(ctx context.Context, args ...string) (string, int, error) {
		ctx, cancel := context.WithTimeout(ctx, s.bound)
		defer cancel()
		return launchd.Local.Run(ctx, args...)
	}}
	s.machine = roleaccount.Mac
	s.machine.Directory = roleaccount.Directory{Run: func(ctx context.Context, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, s.bound)
		defer cancel()
		return roleaccount.Local.Run(ctx, args...)
	}}
	s.machine.Processes = func(ctx context.Context) ([]roleaccount.Process, error) {
		ctx, cancel := context.WithTimeout(ctx, s.bound)
		defer cancel()
		return roleaccount.Mac.Processes(ctx)
	}
	s.procs = func(ctx context.Context) ([]proc, error) {
		ctx, cancel := context.WithTimeout(ctx, s.bound)
		defer cancel()
		return listProcs(ctx)
	}
	s.killAs = func(ctx context.Context, uid, gid int) error {
		ctx, cancel := context.WithTimeout(ctx, s.bound)
		defer cancel()
		return runKillHelper(ctx, s.Launcher, uid, gid)
	}
	s.lsDelete = func(ctx context.Context, uid, gid int) error {
		ctx, cancel := context.WithTimeout(ctx, s.bound)
		defer cancel()
		return runLSDelete(ctx, uid, gid)
	}
	s.command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, s.bound)
		defer cancel()
		return runCommand(ctx, name, args...)
	}
	s.ensure = func(ctx context.Context, spec roleaccount.Spec, l roleaccount.Ledger, keep func(roleaccount.Ledger) error) (roleaccount.Account, error) {
		return roleaccount.Ensure(ctx, s.machine, spec, l, keep)
	}
	s.ids = s.accountIDs
	return s, nil
}

// Close does nothing: runners' jobs run on.
func (s *Slots) Close() error { return nil }

// Start starts runner's listener in slot n, from the runner root the
// caller built, with its JIT config.
//
//  1. It records runner in the slot's folder, so that a restarted agent
//     knows whose job the slot's is.
//  2. It writes config to a file only the service account can read, in
//     the runner root, which must be the account's own, mode 0700, as must
//     the slot's temporary folder.
//  3. It loads the slot's launchd job, io.github.meschoening.silkpool.slot.N,
//     with launchctl bootstrap, from a plist in a folder of root's alone.
//     The job runs the launcher as the service account (UserName), in a
//     security session of its own (SessionCreate), with the slot's own
//     temporary folder as TMPDIR, and starts as it's loaded. It never
//     starts at boot: launchd loads nothing from that folder then. The
//     launcher moves the config into the listener's environment, so it's
//     never on a command line or in the job's settings, which other
//     accounts can read. launchd sets HOME to the account's home, which
//     the launcher keeps.
//  4. The listener's output goes to a new output file of the job's own,
//     which the account may write, in a folder it can't list (newOutput),
//     and which the agent records in a folder of root's alone. Once the
//     job has ended, Wait, or else the slot's reset, copies it into the
//     agent's logs (ListenerLog). launchd opens the file, and enters the
//     runner root, as the account, so Start checks the account can reach
//     both.
//
// A slot runs one runner at a time: Start fails while the slot's job is
// still loaded, which Wait unloads once it has read how it ended. A last
// runner's output that hasn't been copied yet, Start copies first, and
// fails if it can't. Start fails if the job doesn't load, and then
// deletes the config file, the output file and the plist. The
// account-wide clean waits for a Start, and a Start for the clean.
func (s *Slots) Start(ctx context.Context, n int, runner string, config secret.Value) error {
	if err := checkRunner(runner); err != nil {
		return err
	}
	s.use.RLock()
	defer s.use.RUnlock()
	uid, gid, err := s.ids(ctx)
	if err != nil {
		return fmt.Errorf("can't find the service account: %w", err)
	}
	root := s.RunnerRoot(n)
	if err := ownDir(root, uid, 0o700); err != nil {
		return fmt.Errorf("slot %d's runner root isn't ready: %w", n, err)
	}
	if err := ownDir(s.TempDir(n), uid, 0o700); err != nil {
		return fmt.Errorf("slot %d's temporary folder isn't ready: %w", n, err)
	}
	st, err := s.ctl.Print(ctx, s.Domain, s.JobLabel(n))
	if err != nil {
		return fmt.Errorf("can't ask launchd about slot %d's job: %w", n, err)
	}
	if st.Loaded {
		return fmt.Errorf("slot %d's job is still loaded, from %s", n, st.Plist)
	}
	if _, err := s.collectOutput(n); err != nil {
		return fmt.Errorf("slot %d's last runner's output isn't copied yet: %w", n, err)
	}
	if err := enterable(root, uid, gid); err != nil {
		return fmt.Errorf("slot %d's runner root isn't ready: %w", n, err)
	}
	if err := writeRecord(s.Folder(n)+"/"+runnerFile, runner); err != nil {
		return fmt.Errorf("can't record runner %s in slot %d: %w", runner, n, err)
	}
	path := root + "/" + configFile
	if err := writeConfig(path, config, uid, gid); err != nil {
		return fmt.Errorf("can't write the JIT config of runner %s: %w", runner, err)
	}
	if err := s.load(ctx, n, runner, uid, gid); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// load makes slot n's job's output file for runner, which runs as uid
// with its group gid, records it, writes the job's plist, and loads it.
func (s *Slots) load(ctx context.Context, n int, runner string, uid, gid int) error {
	if err := s.jobsFolder(); err != nil {
		return err
	}
	out, err := s.newOutput(uid, gid)
	if err != nil {
		return fmt.Errorf("can't make runner %s's output file: %w", runner, err)
	}
	plist := s.plist(n)
	err = writeRecord(s.outputRecord(n), filepath.Base(out))
	if err == nil {
		err = writeFile(plist, s.job(n, out).Plist(), 0o644)
	}
	if err == nil {
		err = s.ctl.Bootstrap(ctx, s.Domain, plist)
	}
	if err != nil {
		removeFiles(plist, out, s.outputRecord(n))
		return fmt.Errorf("launchd didn't load runner %s's job: %w", runner, err)
	}
	return nil
}

// jobsFolder makes the jobs' folder, or checks it's root's alone.
func (s *Slots) jobsFolder() error {
	if err := os.MkdirAll(s.jobs(), 0o700); err != nil {
		return err
	}
	if err := ownDir(s.jobs(), os.Geteuid(), 0o700); err != nil {
		return fmt.Errorf("the jobs' folder isn't root's alone: %w", err)
	}
	return nil
}

// job is slot n's launchd job, with its output to the file out: the
// decided settings (ADR 0006), and a fixed environment, with the slot's
// own TMPDIR.
func (s *Slots) job(n int, out string) launchd.Job {
	root := s.RunnerRoot(n)
	return launchd.Job{
		Label:            s.JobLabel(n),
		Program:          s.Launcher,
		Args:             []string{LaunchMode, root},
		UserName:         s.Name,
		SessionCreate:    true,
		SessionType:      s.sessionType,
		RunAtLoad:        true,
		WorkingDirectory: root,
		Env: map[string]string{
			"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE": "1",
			channelTimeoutVar: channelTimeout,
			"TMPDIR":          s.TempDir(n) + "/",
		},
		Stdout: out,
		Stderr: out,
	}
}

// writeFile writes b to a new file at path, replacing one an earlier
// attempt left, and never following a link there.
func writeFile(path string, b []byte, perm os.FileMode) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(path)
	}
	return err
}

// Stop stops runner's listener in slot n gracefully: SIGINT to the
// listener alone, with launchctl kill. If it hasn't ended StopTimeout
// later, the slot's reset kills what's left. It returns once launchd has
// sent the signal; Wait reports the end.
func (s *Slots) Stop(ctx context.Context, n int, runner string) error {
	return s.signal(ctx, n, runner, "SIGINT")
}

// Kill kills runner's listener in slot n with SIGKILL. When a job's
// process ends, launchd ends what's left in its process group; the
// slot's reset kills the rest.
func (s *Slots) Kill(ctx context.Context, n int, runner string) error {
	return s.signal(ctx, n, runner, "SIGKILL")
}

// StopTimeout is how long a graceful stop gives the listener after
// SIGINT before the slot's reset kills what's left.
const StopTimeout = 30 * time.Second

func (s *Slots) signal(ctx context.Context, n int, runner, sig string) error {
	if err := checkRunner(runner); err != nil {
		return err
	}
	st, err := s.status(ctx, n, runner)
	if errors.Is(err, ErrGone) || err == nil && !st.Running() {
		return nil
	}
	if err != nil {
		return err
	}
	return s.ctl.Kill(ctx, sig, s.Domain, s.JobLabel(n))
}

// status returns what launchd says of runner's job in slot n, or ErrGone
// if the slot holds no job of Silkpool's for it: none is loaded, or the
// slot's last runner was another. A job of the slot's label that launchd
// loaded from another plist isn't Silkpool's, and is an error.
func (s *Slots) status(ctx context.Context, n int, runner string) (launchd.Status, error) {
	if s.recordedRunner(n) != runner {
		return launchd.Status{}, ErrGone
	}
	st, err := s.ctl.Print(ctx, s.Domain, s.JobLabel(n))
	if err != nil {
		return st, err
	}
	if !st.Loaded {
		return st, ErrGone
	}
	// launchd names the plist by its resolved path: /private/var/db/…
	// for /var/db/….
	ours, err := s.resolvedPlist(n)
	if err != nil {
		return st, fmt.Errorf("can't resolve slot %d's plist, to tell its job is Silkpool's: %w", n, err)
	}
	if st.Plist != ours {
		return st, fmt.Errorf("slot %d's job is loaded from %s, not Silkpool's %s", n, st.Plist, ours)
	}
	return st, nil
}

// resolvedPlist is the path of slot n's job's plist with the jobs'
// folder's links resolved, as launchctl print names a job's plist.
func (s *Slots) resolvedPlist(n int) (string, error) { return s.resolved(s.plist(n)) }

// resolved is the path of plist, a plist in the jobs' folder, with the
// folder's links resolved.
func (s *Slots) resolved(plist string) (string, error) {
	dir, err := filepath.EvalSymlinks(s.jobs())
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(plist)), nil
}

// Wait waits for runner's listener in slot n to end, and returns how it
// ended. launchd keeps a job's last exit while it's loaded, so a runner
// that ended while the agent was down is read the same way. Once it has
// read it, Wait unloads the job, so the slot can start its next runner.
// It's ErrGone if the slot holds no job for runner. Wait at most once at
// a time for a runner.
//
// Once the job is unloaded, Wait copies the listener's output into the
// agent's logs (collectOutput). If that fails, it logs it and leaves the
// output for the slot's reset, which fails closed until it's copied.
func (s *Slots) Wait(ctx context.Context, n int, runner string) (Exit, error) {
	if err := checkRunner(runner); err != nil {
		return Exit{}, err
	}
	tick := time.NewTicker(s.poll)
	defer tick.Stop()
	for {
		st, err := s.status(ctx, n, runner)
		if err != nil {
			return Exit{}, err
		}
		if st.Exited && st.State == "not running" {
			if err := s.ctl.Bootout(ctx, s.Domain, s.JobLabel(n)); err != nil {
				return Exit{}, fmt.Errorf("runner %s ended with %d, but its job wouldn't unload: %w", runner, st.Code, err)
			}
			os.Remove(s.plist(n))
			wrote, err := s.collectOutput(n)
			if err != nil {
				s.logger().Warn("Couldn't copy a runner's listener output into the agent's logs. The slot's reset tries again",
					"runner", runner, "slot", n, "error", secret.Scrub(err.Error()))
			}
			if hint := exConfigHint(st.Code, wrote); err == nil && hint != "" {
				s.logger().Warn("A runner's job didn't start: "+hint, "runner", runner, "slot", n)
			}
			return Exit{Code: st.Code}, nil
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return Exit{}, ctx.Err()
		}
	}
}

// Alive returns the runners whose listeners are still running in their
// slots' jobs, sorted: what a restarted agent tells the node as
// Started's Alive.
func (s *Slots) Alive(ctx context.Context) ([]string, error) {
	recorded, err := s.Recorded()
	if err != nil {
		return nil, err
	}
	var alive []string
	for n, runner := range recorded {
		st, err := s.status(ctx, n, runner)
		switch {
		case errors.Is(err, ErrGone):
		case err != nil:
			return nil, err
		case st.Running():
			alive = append(alive, runner)
		}
	}
	slices.Sort(alive)
	return alive, nil
}

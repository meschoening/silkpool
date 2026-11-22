package slot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/launchd"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/roleaccount"
	"github.com/meschoening/silkpool/internal/secret"
)

// Reset resets slot n once its runner has ended, and returns what the
// node applies, as ResetSlot asks: LogsCopied, then SlotReset, or
// ResetFailed if a step failed. A failed step fails closed: the steps
// after it don't run, and the next attempt starts again from the first.
//
// Every slot runs as the Mac's one service account (#214), so the reset
// splits in two (ADR 0005). This is the slot's own part, which never
// touches another slot's job or folders, or anything of the account's
// that another slot's job may be using:
//
//  1. Stop the slot's launchd job and kill what's in it: unload it, if
//     launchd loaded it from Silkpool's plist, and wait until launchd
//     says it's gone (unloadJob). launchd kills what's left of a job's
//     process group as it ends, since the job doesn't set
//     AbandonProcessGroup.
//  2. Copy the listener's output, if Wait didn't, from the job's output
//     file into the runner's folder in the agent's logs, and delete the
//     file (collectOutput). Then copy the last runner's _diag logs
//     beside it, and read the run its Worker log names, as on Linux
//     (copyLogs).
//  3. Delete the runner root whole, with the runner's credential files,
//     _diag and _work.
//  4. Delete the slot's temporary folder, its runners' TMPDIR, which
//     Prepare makes again.
//  5. Mark the account for its account-wide clean, and forget the slot's
//     runner, so that it counts as idle.
//
// What every slot shares waits for the account-wide clean, which runs
// only once every slot is idle (Clean): the account's launchd user
// domain, its scheduled work, its processes outside every slot's job,
// such as a daemon that called setsid, its home, the shared places and
// its /var/folders folders. The reset runs the clean itself if it leaves
// every slot idle.
//
// The reset reads the account at each attempt, and acts on its UID only
// as its record proves it Silkpool's (whose): it copies logs only the
// account owns, and clears the user flags only of what the account owns.
// Without an account the record proves, it copies no logs, and what's
// flagged in the slot's folders fails the step.
func (s *Slots) Reset(ctx context.Context, n int) []node.Event {
	run, err := s.reset(ctx, n, time.Now())
	events := []node.Event{node.LogsCopied{Slot: n, Run: run}}
	if err != nil {
		return append(events, node.ResetFailed{Slot: n, Detail: detail(err)})
	}
	// A clean that fails here fails the next slot's preparation instead,
	// which runs it again: this slot's own part is done.
	s.cleanIfIdle(ctx, false)
	return append(events, node.SlotReset{Slot: n})
}

func (s *Slots) reset(ctx context.Context, n int, now time.Time) (node.Run, error) {
	var run node.Run
	a, err := s.whose(ctx)
	if err != nil {
		return run, fmt.Errorf("slot %d's reset couldn't read the service account: %w", n, err)
	}
	owner := remover{uid: a.provenUID()}
	steps := []struct {
		what string
		do   func() error
	}{
		{"stop its job", func() error { return s.unloadJob(ctx, n) }},
		{"copy its listener's output", func() error { _, err := s.collectOutput(n); return err }},
		{"copy its runner's logs", func() (err error) { run, err = s.copyLogs(n, a.provenUID(), now); return err }},
		{"delete its runner root", func() error { return owner.removePath(s.RunnerRoot(n), false) }},
		{"delete its temporary folder", func() error { return owner.removePath(s.TempDir(n), false) }},
		{"mark the service account for its clean", func() error { return s.markForClean(now) }},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			return run, fmt.Errorf("slot %d's reset couldn't %s: %w", n, step.what, err)
		}
	}
	if err := os.Remove(filepath.Join(s.Folder(n), runnerFile)); err != nil && !os.IsNotExist(err) {
		return run, fmt.Errorf("slot %d's reset couldn't forget its runner: %w", n, err)
	}
	s.resetDone()
	return run, nil
}

// DeleteSlot deletes what slot n keeps on the Mac, its folder, as
// removing a node asks once the slot has been reset, and returns
// SlotDeleted or DeleteFailed. It unloads the slot's job first, in case
// one outlived the reset. It never deletes the service account, which
// only uninstall does (DeleteAccount). It clears the user flags only of
// what the account owns, as its record proves it Silkpool's.
func (s *Slots) DeleteSlot(ctx context.Context, n int) node.Event {
	a, err := s.whose(ctx)
	if err == nil {
		err = s.unloadJob(ctx, n)
	}
	if err == nil {
		err = remover{uid: a.provenUID()}.removePath(s.Folder(n), false)
	}
	if err != nil {
		return node.DeleteFailed{Slot: n, Detail: detail(fmt.Errorf("couldn't delete slot %d: %w", n, err))}
	}
	return node.SlotDeleted{Slot: n}
}

// A macAccount is the service account as an attempt to reset a slot or
// delete the account finds it, from its ledger and its user record.
type macAccount struct {
	ledger roleaccount.Ledger
	// proven reports that the user record of the account's name holds
	// the GeneratedUID the ledger owns for it, and an ID in the role
	// accounts' range as its UID and its GID: only then are uid, gid and
	// guid set, from that record.
	proven   bool
	uid, gid int
	guid     string
	// held are the UIDs a process of the account may hold: the ledger's
	// ID, and the UID the account's record holds, if it holds one.
	held []int
}

// whose reads the service account as its ledger and its user record have
// it. A record of Silkpool's that install didn't finish, cut short before
// it gave the record its UID or its GID, isn't proven, so nothing acts on
// a UID of it, but the UID it holds counts as one its processes may
// hold. A record whose IDs aren't one number each, or are outside the
// role accounts' range, which Silkpool never gives, is an error: nothing
// acts on an account like that.
func (s *Slots) whose(ctx context.Context) (macAccount, error) {
	l, err := readLedger(s.ledger())
	if err != nil {
		return macAccount{}, fmt.Errorf("can't read the service account's ledger: %w", err)
	}
	a := macAccount{ledger: l, held: []int{l.ID}}
	r, found, err := s.machine.Read(ctx, roleaccount.Users, s.Name)
	if err != nil {
		return a, err
	}
	if !found || !l.User.Owned || !r.Is("GeneratedUID", l.User.GUID) {
		return a, nil
	}
	ids := map[string]int{}
	for _, attr := range []string{"UniqueID", "PrimaryGroupID"} {
		if _, ok := r[attr]; !ok {
			continue
		}
		id, ok := oneID(r, attr)
		if !ok || !s.roleID(id) {
			return a, fmt.Errorf("the service account's %s is %q, but Silkpool gives its accounts one ID from %d to %d",
				attr, r[attr], s.idRange[0], s.idRange[1])
		}
		ids[attr] = id
	}
	uid, hasUID := ids["UniqueID"]
	gid, hasGID := ids["PrimaryGroupID"]
	if hasUID {
		a.held = append(a.held, uid)
	}
	if hasUID && hasGID {
		a.proven, a.uid, a.gid, a.guid = true, uid, gid, l.User.GUID
	}
	return a, nil
}

// provenUID is a's UID, as its record proves it, or -1 for none.
func (a macAccount) provenUID() int {
	if a.proven {
		return a.uid
	}
	return -1
}

// roleID reports whether id is in the range of IDs Silkpool picks the
// service account's from.
func (s *Slots) roleID(id int) bool { return id >= s.idRange[0] && id <= s.idRange[1] }

// oneID reads an attribute that holds one ID, and false unless it holds
// exactly one decimal number.
func oneID(r roleaccount.Record, attr string) (int, bool) {
	v, ok := r.One(attr)
	n, err := strconv.Atoi(v)
	return n, ok && err == nil && strconv.Itoa(n) == v
}

// bootoutDomain boots out the launchd user domain of the account uid,
// ending every agent launchd runs for the account. It runs launchctl
// bootout once and never asks launchd about the domain: on the Mac (#229),
// launchd still held a domain 30 s after its bootout while the clean kept
// asking with launchctl print, and restarted distnoted's agent in it, but a
// bootout alone let the domain go within a second. A domain launchd
// doesn't hold, or is still tearing down from an earlier bootout
// (launchd.ErrTearingDown), is going already, so either is done. Whether
// anything of the account is left the kill rounds read from ps (killAll).
func (s *Slots) bootoutDomain(ctx context.Context, uid int) error {
	err := s.ctl.BootoutDomain(ctx, fmt.Sprintf("user/%d", uid))
	if errors.Is(err, launchd.ErrTearingDown) {
		return nil
	}
	return err
}

// unloadWait is how long unloadJob waits for launchd to let a job it
// booted out go: launchd gives a job's process 20 s to end after SIGTERM
// by default.
const unloadWait = 30 * time.Second

// unloadJob unloads slot n's launchd job, if launchd holds it, and
// deletes its plist. It's the one place, but Wait, that unloads a slot's
// job. It boots the job out only if launchd loaded it from Silkpool's own
// plist, as status tells, and waits until launchd says it's gone, so
// nothing after it meets the job still going. A job of the slot's label
// that launchd loaded from another plist isn't Silkpool's: it's left,
// and fails the step.
func (s *Slots) unloadJob(ctx context.Context, n int) error {
	return s.unload(ctx, s.JobLabel(n), s.plist(n))
}

// unload unloads the job label, if launchd holds it from plist, its
// plist in the jobs' folder, as unloadJob does a slot's, and deletes the
// plist.
func (s *Slots) unload(ctx context.Context, label, plist string) error {
	st, err := s.ctl.Print(ctx, s.Domain, label)
	if err != nil {
		return err
	}
	if st.Loaded {
		ours, err := s.resolved(plist)
		if err != nil {
			return fmt.Errorf("can't resolve its job's plist, to tell its job is Silkpool's: %w", err)
		}
		if st.Plist != ours {
			return fmt.Errorf("its job is loaded from %s, not Silkpool's %s, so it's left", st.Plist, ours)
		}
		if err := s.ctl.Bootout(ctx, s.Domain, label); err != nil {
			return err
		}
		deadline := time.Now().Add(unloadWait)
		for st.Loaded {
			if time.Now().After(deadline) {
				return fmt.Errorf("its job is still loaded %v after its bootout", unloadWait)
			}
			select {
			case <-time.After(s.poll):
			case <-ctx.Done():
				return ctx.Err()
			}
			if st, err = s.ctl.Print(ctx, s.Domain, label); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// killAll kills every process of a, as the account-wide clean does once
// every slot is idle: it boots out a's user domain and runs the kill
// helper as a, until no process of a is left, and then removes a's user
// domain and scheduled work again, since a process could have made some
// before it was killed. Then it looks once more, and goes another round
// if a process of a is there: launchd may start one of the account's
// agents, such as distnoted, which it keeps alive in a user domain, as
// the domain goes, as a clean on the Mac left one running (#229). Each
// round counts toward killRounds, past which a process still there fails
// it closed. A process that holds a's UID as its effective UID alone,
// which the helper can't reach, fails it.
func (s *Slots) killAll(ctx context.Context, a macAccount) error {
	for round := 1; ; round++ {
		if err := s.bootoutDomain(ctx, a.uid); err != nil {
			return err
		}
		if err := s.killAs(ctx, a.uid, a.gid); err != nil {
			return err
		}
		left, err := s.processesOf(ctx, a.uid)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			if err := errors.Join(s.bootoutDomain(ctx, a.uid), s.unschedule(ctx, a.uid, s.FullDiskAccess)); err != nil {
				return err
			}
			if left, err = s.processesOf(ctx, a.uid); err != nil || len(left) == 0 {
				return err
			}
		}
		if round == killRounds {
			return fmt.Errorf("%w after %d rounds of kills", stillRun(left), round)
		}
		select {
		case <-time.After(time.Duration(round) * 50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// processesOf returns the PIDs of the live processes whose real or saved
// UID is uid, which are the ones kill(-1) run as uid reaches. A process
// that holds uid as its effective UID alone, which it can't reach, is an
// error: it's left running.
func (s *Slots) processesOf(ctx context.Context, uid int) ([]int, error) {
	procs, err := s.procs(ctx)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range procs {
		switch {
		case p.ruid == uid || p.svuid == uid:
			pids = append(pids, p.pid)
		case p.uid == uid:
			return nil, fmt.Errorf("process %d holds its account's UID %d as its effective UID alone, which its kill helper can't reach, so it's left running", p.pid, uid)
		}
	}
	return pids, nil
}

// goneWait is how long DeleteAccount waits for the account's processes to
// end once it has booted its user domain out: launchd ends its agents
// there within a second (#229).
const goneWait = 10 * time.Second

// waitNoneHold waits, up to goneWait, until no live process holds one of
// ids, as noneHold reads ps, and fails as noneHold does if one still
// does then, logging each such process as ps prints it.
func (s *Slots) waitNoneHold(ctx context.Context, ids ...int) error {
	wait := s.goneWait
	if wait == 0 {
		wait = goneWait
	}
	for deadline := time.Now().Add(wait); ; {
		err := s.noneHold(ctx, ids...)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			s.logHolders(ctx, ids...)
			return err
		}
		select {
		case <-time.After(s.poll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// logHolders logs each live process that holds one of ids, as ps prints
// it: its PID, its parent, its UIDs and its command.
func (s *Slots) logHolders(ctx context.Context, ids ...int) {
	procs, err := s.procs(ctx)
	if err != nil {
		return
	}
	for _, pid := range holding(procs, ids...) {
		out, _ := s.command(ctx, "/bin/ps", "-ww", "-o", "pid=,ppid=,uid=,ruid=,svuid=,command=", "-p", strconv.Itoa(pid))
		s.logger().Warn("A process still holds the service account's UID, so the account isn't deleted",
			"process", secret.Scrub(string(bytes.TrimSpace(out))))
	}
}

// noneHold fails while any live process holds one of ids as its
// effective, real or saved UID. An ID of 0 is none.
func (s *Slots) noneHold(ctx context.Context, ids ...int) error {
	ids = slices.DeleteFunc(slices.Clone(ids), func(id int) bool { return id == 0 })
	if len(ids) == 0 {
		return nil
	}
	procs, err := s.procs(ctx)
	if err != nil {
		return err
	}
	if err := stillRun(holding(procs, ids...)); err != nil {
		return fmt.Errorf("%w with its account's UID", err)
	}
	return nil
}

// holding returns the PIDs of the processes in procs that hold one of
// ids as their effective, real or saved UID. An ID of 0 is none.
func holding(procs []proc, ids ...int) []int {
	ids = slices.DeleteFunc(slices.Clone(ids), func(id int) bool { return id == 0 })
	var pids []int
	for _, p := range procs {
		if slices.Contains(ids, p.uid) || slices.Contains(ids, p.ruid) || slices.Contains(ids, p.svuid) {
			pids = append(pids, p.pid)
		}
	}
	return pids
}

// A proc is a live process, and the UIDs it holds.
type proc struct {
	pid int
	// uid, ruid and svuid are its effective, real and saved UIDs.
	uid, ruid, svuid int
}

// listProcs lists every live process, as ps prints it.
func listProcs(ctx context.Context) ([]proc, error) {
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "uid=", "-o", "ruid=", "-o", "svuid=", "-o", "state=", "-o", "pid=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps -A: %w", err)
	}
	return parseProcs(string(out), os.Getpid())
}

// parseProcs reads ps's lines of a process's effective, real and saved
// UID, its state and its PID. A zombie (state Z) is dead already: its
// parent, or launchd, reaps it, so it's left out. It fails unless self is
// among them, so a listing cut short can't pass for fewer processes.
func parseProcs(out string, self int) ([]proc, error) {
	var procs []proc
	seen := false
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 5 {
			return nil, fmt.Errorf("ps -A printed %q", line)
		}
		var ids [4]int
		for i, v := range []string{f[0], f[1], f[2], f[4]} {
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("ps -A printed %q", line)
			}
			ids[i] = n
		}
		seen = seen || ids[3] == self
		if !strings.HasPrefix(f[3], "Z") {
			procs = append(procs, proc{pid: ids[3], uid: ids[0], ruid: ids[1], svuid: ids[2]})
		}
	}
	if !seen {
		return nil, fmt.Errorf("the processes ps listed don't include this one, %d, so they can't have been listed whole", self)
	}
	return procs, nil
}

// runKillHelper runs the kill helper, `<launcher> kill-all <uid>`, as the
// account uid with its group gid alone, with no environment, in /. A
// helper that the account's processes killed first counts as a round:
// the next one tries again. One cut off by ctx doesn't.
func runKillHelper(ctx context.Context, launcher string, uid, gid int) error {
	cmd := exec.CommandContext(ctx, launcher, KillAllMode, strconv.Itoa(uid))
	cmd.Env, cmd.Dir = []string{}, "/"
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}},
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("its kill helper didn't finish: %w", ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("its kill helper failed: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// checkAccountUID refuses a UID the service account can't have: Silkpool
// picks its ID once, from roleaccount.First to Last alone, so the kill
// helper never runs kill(-1) as anyone else, such as one of the Mac's
// people. The helper runs as the account, and can't read the account's
// ledger, which only root may open: the agent checks the UID it passes
// against that (whose).
func checkAccountUID(uid int) error {
	if uid < roleaccount.First || uid > roleaccount.Last {
		return fmt.Errorf("%s runs only as the service account, whose UID is from %d to %d, not %d", KillAllMode, roleaccount.First, roleaccount.Last, uid)
	}
	return nil
}

// unschedule removes the crontab and the at jobs of the service account,
// by its name and uid, which the attempt proved, and checks they're gone.
//
// macOS refuses the cron and at spools to a program without Full Disk
// Access (#229). Every slot job runs the agent's program, so while it has
// none, a refusal is nothing to remove, which unschedule logs, as decided
// on #229. But once the agent runs with it, as strict says, the clean's
// do (Layout.FullDiskAccess, #306), so do the jobs, which may have
// scheduled work, and a refusal, as after the operator took it away,
// fails it: the clean stays due until it can check. Any other failure
// fails it.
func (s *Slots) unschedule(ctx context.Context, uid int, strict bool) error {
	return errors.Join(s.removeCrontab(ctx, s.Name, strict), s.removeAtJobs(ctx, uid, strict))
}

// refusedSpool answers macOS's refusal of the what spool, cron or at,
// which said why: an error if strict, and otherwise nothing, logged
// (noSpool).
func (s *Slots) refusedSpool(what string, said []byte, strict bool) error {
	if strict {
		return fmt.Errorf("macOS refused Silkpool the %s spool, which it opens only with Full Disk Access, so whether a job scheduled work there can't be checked: %s",
			what, secret.Scrub(string(bytes.TrimSpace(said))))
	}
	s.noSpool(what, said)
	return nil
}

// spoolRefused reports whether a cron or at command's output says macOS
// refused it the spool.
func spoolRefused(out []byte) bool { return bytes.Contains(out, []byte("Operation not permitted")) }

// noSpool logs that macOS refused the spool of what, cron or at, which
// said why, so nothing of the service account's there is removed.
func (s *Slots) noSpool(what string, said []byte) {
	s.logger().Warn("macOS refused Silkpool the "+what+" spool, as it refuses every slot job without Full Disk Access, "+
		"so the service account can have scheduled nothing there, and nothing is removed",
		"said", secret.Scrub(string(bytes.TrimSpace(said))))
}

// removeCrontab removes the crontab of the account name, if it has one.
func (s *Slots) removeCrontab(ctx context.Context, name string, strict bool) error {
	out, err := s.command(ctx, "/usr/bin/crontab", "-u", name, "-r")
	switch {
	case err != nil && spoolRefused(out):
		return s.refusedSpool("cron", out, strict)
	case err != nil && !noCrontab(out):
		return fmt.Errorf("crontab -r: %w: %s", err, bytes.TrimSpace(out))
	}
	if out, err := s.command(ctx, "/usr/bin/crontab", "-u", name, "-l"); err == nil || !noCrontab(out) {
		return fmt.Errorf("its crontab is still there: %s", bytes.TrimSpace(out))
	}
	return nil
}

// removeAtJobs removes the at jobs of the account uid, found in the at
// spool by their files' owner (atJobs), with atrm, and checks they're
// gone.
func (s *Slots) removeAtJobs(ctx context.Context, uid int, strict bool) error {
	jobs, err := atJobs(s.atSpool, uid)
	if refused(err) {
		return s.refusedSpool("at", []byte(err.Error()), strict)
	}
	if err != nil || len(jobs) == 0 {
		return err
	}
	if out, err := s.command(ctx, "/usr/bin/atrm", jobs...); err != nil {
		if spoolRefused(out) && strict {
			return fmt.Errorf("macOS refused Silkpool atrm of the service account's at jobs %s, which it removes only with Full Disk Access: %s",
				strings.Join(jobs, ", "), secret.Scrub(string(bytes.TrimSpace(out))))
		}
		if spoolRefused(out) {
			// The jobs are there, though no slot job could have made them:
			// say so, and leave them, as decided on #229.
			s.logger().Warn("macOS refused Silkpool atrm of the service account's at jobs, which it found in the at spool, "+
				"so they're left. Only a program with Full Disk Access could have made them",
				"uid", uid, "jobs", strings.Join(jobs, ", "), "said", secret.Scrub(string(bytes.TrimSpace(out))))
			return nil
		}
		return fmt.Errorf("atrm: %w: %s", err, bytes.TrimSpace(out))
	}
	if jobs, err = atJobs(s.atSpool, uid); err == nil && len(jobs) > 0 {
		err = fmt.Errorf("its at jobs %s are still there", strings.Join(jobs, ", "))
	}
	return err
}

// atJob is the name of a job's file in macOS's at spool: its queue, then
// its number in 5 hex digits, then its time in 8, as at's own source
// writes it.
var atJob = regexp.MustCompile(`^[A-Za-z=]([0-9a-f]{5})[0-9a-f]{8}$`)

// atJobs returns the numbers, in decimal, which atrm takes, of uid's jobs
// in macOS's at spool, dir, /usr/lib/cron/jobs. A job is uid's if its file
// is: at writes each job to a file the submitter owns, and macOS's atq
// names no owner (#229). It follows no link, and leaves at's own files,
// .SEQ and .lockfile. An entry uid owns that isn't a plain file named as
// a job fails it, as does a job of uid's whose number another job holds
// too, which atrm would remove with it: a spool it doesn't understand
// can't pass for none.
func atJobs(dir string, uid int) ([]string, error) {
	d, err := openFolder(unix.AT_FDCWD, dir, dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, &os.PathError{Op: "read", Path: dir, Err: err}
	}
	var jobs []string
	holders := map[string]int{}
	for _, n := range names {
		if strings.HasPrefix(n, ".") {
			continue
		}
		var st unix.Stat_t
		err := unix.Fstatat(int(d.Fd()), n, &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "stat", Path: filepath.Join(dir, n), Err: err}
		}
		m := atJob.FindStringSubmatch(n)
		var number string
		if m != nil {
			v, _ := strconv.ParseUint(m[1], 16, 32)
			number = strconv.FormatUint(v, 10)
			holders[number]++
		}
		if int(st.Uid) != uid {
			continue
		}
		if m == nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, fmt.Errorf("%s is the service account's, but isn't an at job's file", filepath.Join(dir, n))
		}
		jobs = append(jobs, number)
	}
	for _, j := range jobs {
		if holders[j] > 1 {
			return nil, fmt.Errorf("another at job holds the number %s of one of its own, which atrm would remove with it", j)
		}
	}
	slices.Sort(jobs)
	return jobs, nil
}

// runCommand runs a program in the C locale, whose messages the reset
// reads, and returns what it wrote.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	return cmd.CombinedOutput()
}

// recreateHome recreates the service account's home empty, a's, mode
// 0700, as the account-wide clean does.
func (s *Slots) recreateHome(a macAccount) error {
	if err := (remover{uid: a.uid}).removePath(s.Home(), false); err != nil {
		return err
	}
	return ownedDir(s.Home(), a.uid, a.gid, 0o700)
}

// emptyVarFolders empties the account uid's /var/folders folders
// (accountFolders), keeping the folders SIP flags sunlnk, which it won't
// let even root delete. With skipped set, a folder macOS refuses to open
// or read while it looks for them is added there, as nothing to empty.
//
// With skipped set, an entry in one of them that macOS refuses, as SIP
// refuses even root a stat of a data vault there (#294), is left too,
// with the folders holding it (remover.refused), and added there. The
// clean, which doesn't set it, fails closed on such an entry, since a
// job may have written what's in it and the next job could read it: but
// for the vault of the Launch Services database, 0/com.apple.LaunchServices.dv
// in each folder, with lsReset, which the clean sets once it has reset
// that database another way (resetLaunchServices). That vault, refused,
// is left, with the folders holding it (remover.vault).
func (s *Slots) emptyVarFolders(uid int, skipped *[]string, lsReset bool) error {
	folders, err := accountFolders(s.varFolders, uid, skipped)
	if err != nil {
		return err
	}
	for _, f := range folders {
		r := remover{uid: uid, keepNoUnlink: true, refused: skipped}
		if lsReset {
			r.vault = &resetVault{path: filepath.Join(f, "0", launchServicesVault)}
		}
		if err := r.removePath(f, true); err != nil {
			return err
		}
	}
	return nil
}

// launchServicesVault is the data vault in the 0 folder of an account's
// /var/folders folder, its DARWIN_USER_DIR, that holds the account's
// Launch Services database. SIP refuses even root a stat of it (#294).
const launchServicesVault = "com.apple.LaunchServices.dv"

// lsregister is Launch Services' own tool, which deletes the database of
// the account it runs as.
const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// resetLaunchServices resets a's Launch Services database, as the clean
// does once nothing of a's runs, since a job that registers an app, as
// opening one or building one with Xcode does, leaves it there, with its
// URL schemes and document types, for the next job. Only the account's
// own lsd may open the vault the database is in, which SIP refuses even
// root, so it runs lsregister -delete as a (lsDelete), which has that lsd
// delete the database file. launchd starts that lsd in a's user domain
// for it, with what lsd needs there, such as cfprefsd and distnoted: then
// it kills every process of a again (killAll), so lsd can't write back
// what it had loaded. The next job's lsd starts a database afresh, which
// holds only the apps in the Mac's own places. On the Mac (#297), a job's
// registration that survived the clean without this was gone after it,
// though the app was still on disk.
//
// lsregister's -kill option, which reset the database in place, is gone
// from macOS: it says it was removed as "dangerous and no longer useful".
func (s *Slots) resetLaunchServices(ctx context.Context, a macAccount) error {
	if err := s.lsDelete(ctx, a.uid, a.gid); err != nil {
		return err
	}
	return s.killAll(ctx, a)
}

// lsDeleted is what lsregister -delete prints once the database file is
// deleted.
const lsDeleted = "Launch Services database file has been deleted"

// lsNoDatabase is what lsregister -delete prints, exiting 0 all the same,
// when the account has no database file to delete, the error under its
// failure ENOENT: as for an account no job has used Launch Services as
// yet, as the privileged-macos lane's account met it on the Mac (#297).
// Nothing of the account's ran as it started, so the lsd it started
// loaded nothing: there's nothing to reset. The match stays within the
// one diagnostic, which closes no brace before its underlying error.
var lsNoDatabase = regexp.MustCompile(`Failed to delete Launch Services database: [^}]*NSUnderlyingError=\S+ \{Error Domain=NSPOSIXErrorDomain Code=2 `)

// runLSDelete runs lsregister -delete as the account uid, with its group
// gid alone, as the kill helper runs (runKillHelper), in the C locale.
// It fails unless lsregister exits 0 and says it deleted the database, or
// that there was none to delete.
func runLSDelete(ctx context.Context, uid, gid int) error {
	cmd := exec.CommandContext(ctx, lsregister, "-delete")
	cmd.Env, cmd.Dir = []string{"LC_ALL=C"}, "/"
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}},
	}
	out, err := cmd.CombinedOutput()
	return lsDeleteResult(out, err)
}

// lsDeleteResult reads how lsregister -delete went from what it printed
// and its error.
func lsDeleteResult(out []byte, err error) error {
	said := secret.Scrub(string(bytes.TrimSpace(out)))
	if err != nil {
		return fmt.Errorf("lsregister -delete: %w: %s", err, said)
	}
	if !bytes.Contains(out, []byte(lsDeleted)) && !lsNoDatabase.Match(out) {
		return fmt.Errorf("lsregister -delete didn't say the database was deleted: %q", said)
	}
	return nil
}

// DeleteAccount deletes the Mac's service account, as uninstall asks
// once the machine isn't enrolled and its slots are stopped (ADR 0007).
// Every slot shares the account, so removing a node never deletes it
// (DeleteSlot). It takes ctx, which bounds what it runs.
//
// It refuses, and deletes nothing, while any slot's job or the exposure
// check's is loaded, or a process holds the account's UID, or the
// ledger's ID, as its effective, real or saved UID. Then, for an account
// its record proves Silkpool's, it deletes what the account leaves on the
// Mac outside Silkpool's folders, which only its name, UID and
// GeneratedUID show are its own, while its record can still show the
// account is Silkpool's: its crontab and at jobs, what's in its
// /var/folders folders, and its folder in sfanalytics, named by its
// user's GeneratedUID. Then it deletes the account's records as the
// ledger proves them Silkpool's (roleaccount.Remove), user before group,
// and logs any record of the name it leaves as someone else's. Last, it
// deletes what the account leaves in Silkpool's own folders, which are
// Silkpool's by where they are: its home, each slot's runner root and
// temporary folder, and every file in the jobs' output folder. So when
// macOS refuses the records' deletion, as it does to a process whose app
// hasn't Full Disk Access (roleaccount.ErrPermission), those are left as
// they were. An account install didn't finish, whose record holds no UID
// or no GID yet, has its records deleted all the same, with nothing done
// by a UID. A failure before the records go leaves what's left for the
// next attempt to finish; one after leaves the rest of Silkpool's
// folders for the uninstall, which deletes them whole.
//
// Where it looks for the account's folders outside its home, in
// /var/folders and sfanalytics, a folder macOS refuses to open or read
// even for root (refused), as SIP and TCC may, holds nothing it can
// delete, so it goes on, as decided on #229. So does an entry in one of
// the account's /var/folders folders that macOS refuses, even a stat of,
// as SIP refuses a data vault there (#294). It returns each such folder
// and entry, for uninstall to list, and logs it. Any other error there
// still fails closed, and so does a refusal of a folder of the account's
// own, once it's found.
//
// The account's /var/folders folders stay, emptied: SIP keeps them.
func (s *Slots) DeleteAccount(ctx context.Context) (skipped []string, err error) {
	a, err := s.whose(ctx)
	if err != nil {
		return nil, err
	}
	slots, err := s.slotNumbers()
	if err != nil {
		return nil, err
	}
	labels := []string{s.checkLabel()}
	for _, n := range slots {
		labels = append(labels, s.JobLabel(n))
	}
	for _, label := range labels {
		st, err := s.ctl.Print(ctx, s.Domain, label)
		if err != nil {
			return nil, err
		}
		if st.Loaded {
			return nil, fmt.Errorf("its job %s is still loaded, from %s", label, st.Plist)
		}
	}
	if a.proven {
		// launchd keeps distnoted's agent alive in the account's user
		// domain while it holds one, which keeps the account's UID held
		// (#229): boot the domain out, as the clean does, so what launchd
		// runs for the account ends.
		if err := s.bootoutDomain(ctx, a.uid); err != nil {
			return nil, err
		}
	}
	if err := s.waitNoneHold(ctx, a.held...); err != nil {
		return nil, err
	}
	if a.proven {
		defer func() {
			for _, p := range skipped {
				s.log.Warn("macOS refused to open a folder, or an entry in one, even as root, so nothing of the service account's there was deleted", "folder", p)
			}
		}()
		if err := runSteps([]step{
			{"remove its scheduled work", func() error { return s.unschedule(ctx, a.uid, false) }},
			{"empty its /var/folders folders", func() error { return s.emptyVarFolders(a.uid, &skipped, false) }},
			{"delete its sfanalytics folder", func() error { return s.removeAnalytics(a, &skipped) }},
		}); err != nil {
			return skipped, err
		}
	}
	_, left, err := roleaccount.Remove(ctx, s.machine.Directory, s.Name, a.ledger, s.keepLedger)
	for _, l := range left {
		s.log.Warn("A record of the service account's name isn't Silkpool's, so it's left",
			"record", string(l.Kind)+"/"+l.Name, "evidence", secret.Scrub(l.Evidence))
	}
	if err != nil || !a.proven {
		return skipped, err
	}
	owner := remover{uid: a.uid}
	return skipped, runSteps([]step{
		{"delete its home", func() error { return owner.removePath(s.Home(), false) }},
		{"delete the slots' folders it owns", func() error {
			for _, n := range slots {
				if err := errors.Join(owner.removePath(s.RunnerRoot(n), false), owner.removePath(s.TempDir(n), false)); err != nil {
					return err
				}
			}
			return nil
		}},
		{"delete the jobs' output files", func() error {
			if err := s.clearOutput(a.uid, true); err != nil {
				return err
			}
			for _, n := range slots {
				if err := removeFiles(s.outputRecord(n)); err != nil {
					return err
				}
			}
			return nil
		}},
	})
}

// A step is one of DeleteAccount's steps: what it does, as its error
// says it, and doing it.
type step struct {
	what string
	do   func() error
}

// runSteps does each of steps in order, and stops at the first that
// fails, saying which.
func runSteps(steps []step) error {
	for _, st := range steps {
		if err := st.do(); err != nil {
			return fmt.Errorf("couldn't %s: %w", st.what, err)
		}
	}
	return nil
}

// removeAnalytics deletes a's folder in sfanalytics, named by its user's
// GeneratedUID, which macOS makes for the account, owned by it but
// closed to it, as "Verify keychain signing from a launchd-launched slot
// on the Mac" found. Only a folder, not a link, that a's UID owns is
// a's: anything else there fails closed. sfanalytics itself refused
// (refused), as macOS 26 refuses it even to root, is added to skipped,
// as nothing to delete.
func (s *Slots) removeAnalytics(a macAccount, skipped *[]string) error {
	dir, err := openFolder(unix.AT_FDCWD, s.sfanalytics, s.sfanalytics)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if refused(err) {
		*skipped = append(*skipped, s.sfanalytics)
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	path := filepath.Join(s.sfanalytics, a.guid)
	var st unix.Stat_t
	err = unix.Fstatat(int(dir.Fd()), a.guid, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != a.uid {
		return fmt.Errorf("%s isn't a folder of the account's, so it's left", path)
	}
	return remover{uid: a.uid}.removeAt(int(dir.Fd()), a.guid, path, false)
}

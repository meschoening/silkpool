package slot

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/meschoening/silkpool/internal/node"
)

// Reset resets slot n once its runner has ended, and returns what the
// node applies, as ResetSlot asks: LogsCopied, then SlotReset, or
// ResetFailed if a step failed. A failed step fails closed: the steps
// after it don't run, and the next attempt starts again from the first.
//
// Every slot runs as the machine's one service account (#214), so the
// reset splits in two (ADR 0005). This is the slot's own part, which
// never touches another slot's units or folders:
//
//  1. Stop and kill each of the slot's runner units, and wait until
//     none of them has a process left. A job's processes can't leave
//     its unit's cgroup.
//  2. Copy the last runner's _diag logs into its folder in the agent's
//     logs, beside its listener's output, and read the run its Worker
//     log names (copyLogs).
//  3. Delete the runner root whole, with the runner's credential files,
//     _diag and _work. PrivateTmp= gave the unit its own /tmp and
//     /var/tmp, which systemd deleted with it.
//  4. Forget the slot's runner, so that it counts as idle.
//
// What every slot shares, the account's home, its scheduled work and its
// processes outside every unit, waits for the account-wide clean, which
// runs only once every slot is idle (Clean). The reset marks the account
// as needing it, and runs it then if no other slot holds a runner.
//
// Linux has no shared-places sweep: under ProtectSystem=strict a slot
// can't write outside its own folders and the home, so the set is empty.
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
	a, err := s.owner()
	if err != nil {
		return run, fmt.Errorf("slot %d's reset couldn't read the service account: %w", n, err)
	}
	steps := []struct {
		what string
		do   func() error
	}{
		{"stop its units", func() error { return s.stopUnits(ctx, n) }},
		{"copy its runner's logs", func() (err error) { run, err = s.copyLogs(n, a.uid, now); return err }},
		{"delete its runner root", func() error { return os.RemoveAll(s.RunnerRoot(n)) }},
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

// stopUnits stops and kills each of slot n's runner units, and waits
// until every one of them has ended, or is gone.
func (s *Slots) stopUnits(ctx context.Context, n int) error {
	units, err := s.units(ctx, n)
	if err != nil {
		return err
	}
	for _, unit := range units {
		if err := s.stopUnit(ctx, unit); err != nil {
			return err
		}
	}
	return nil
}

// stopTimeout is how long stopUnit waits for a unit it killed to end.
const stopTimeout = 30 * time.Second

// stopUnit stops and kills unit, and waits until it has ended, or is
// gone: systemd marks it inactive or failed only once no process of its
// cgroup is left. It kills again each time it looks, for at most
// stopTimeout.
func (s *Slots) stopUnit(ctx context.Context, unit string) error {
	deadline := time.Now().Add(stopTimeout)
	for round := 1; ; round++ {
		var job dbus.ObjectPath
		if err := s.manager(ctx, "StopUnit", unit, "replace").Store(&job); err != nil && !gone(err) {
			return fmt.Errorf("can't stop %s: %w", unit, err)
		}
		if err := s.manager(ctx, "KillUnit", unit, "all", int32(syscall.SIGKILL)).Err; err != nil && !gone(err) {
			return fmt.Errorf("can't kill %s: %w", unit, err)
		}
		var path dbus.ObjectPath
		err := s.manager(ctx, "GetUnit", unit).Store(&path)
		if gone(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var state string
		err = getProperty(ctx, s.bus.Object(systemd, path), systemd+".Unit", "ActiveState", &state)
		switch {
		case gone(err), err == nil && (state == "inactive" || state == "failed"):
			return nil
		case err != nil:
			return err
		case time.Now().After(deadline):
			return fmt.Errorf("%s is still %s %s after it was killed", unit, state, stopTimeout)
		}
		select {
		case <-time.After(min(time.Duration(round)*50*time.Millisecond, 500*time.Millisecond)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// DeleteSlot deletes what slot n keeps on the machine, its folder, as
// removing a node asks once the slot has been reset, and returns
// SlotDeleted or DeleteFailed. It stops the slot's units first, in case
// one outlived the reset. It never deletes the service account, which
// only removing the package does (DeleteAccount).
func (s *Slots) DeleteSlot(ctx context.Context, n int) node.Event {
	err := s.stopUnits(ctx, n)
	if err == nil {
		err = os.RemoveAll(s.Folder(n))
	}
	if err != nil {
		return node.DeleteFailed{Slot: n, Detail: detail(fmt.Errorf("couldn't delete slot %d: %w", n, err))}
	}
	return node.SlotDeleted{Slot: n}
}

// DeleteAccount deletes the machine's service account and its group, as
// the package's removal asks once the machine isn't enrolled and the
// agent has stopped (ADR 0007). It never deletes an account Silkpool
// didn't make: one with the service account's name that it didn't
// record, and that lacks its marker, makes it fail. It fails too, and
// deletes nothing, while a process holds the account's UID, which
// userdel doesn't refuse, or the UID the record holds if the account is
// gone. With no account, there's nothing to delete. The home and the
// record go with Root, which the package deletes after it.
func (l Layout) DeleteAccount() error {
	a, err := l.owner()
	if err != nil {
		return err
	}
	if a.uid > 0 {
		left, err := processesOf(a.uid)
		if err != nil {
			return err
		}
		if err := stillRun(left); err != nil {
			return fmt.Errorf("can't delete Silkpool's service account %s: %w", l.Account(), err)
		}
	}
	if a.name == "" {
		if f, err := readLocalAccount(l.Account()); err == nil && f.exists {
			return fmt.Errorf("can't delete the account %s: Silkpool didn't make it", l.Account())
		}
		return nil
	}
	if err := system("userdel", a.name); err != nil {
		return err
	}
	f, err := readLocalAccount(a.name)
	if err != nil {
		return err
	}
	if f.exists {
		return fmt.Errorf("the account %s is still there after userdel", a.name)
	}
	if f.group >= 0 {
		// userdel leaves its group when login.defs says so.
		return system("groupdel", a.name)
	}
	return nil
}

// A serviceUser is the account the reset, the clean and the deletion act
// on.
type serviceUser struct {
	// name is the account's name, or "" for none: either there's no
	// service account, or it was deleted, and uid is the UID its
	// processes may still run as.
	name string
	// uid is the account's UID, or 0 if there's nothing to act on.
	uid int
	// gid is its primary group's ID, or -1 if it has none.
	gid int
	// expire is its shadow entry's expiry date field.
	expire string
}

// owner reads the account the reset, the clean and the deletion act on
// (whose).
func (l Layout) owner() (serviceUser, error) {
	f, err := readLocalAccount(l.Account())
	if err != nil {
		return serviceUser{}, err
	}
	record := l.recorded()
	taken := false
	if record != 0 {
		_, err := user.LookupId(strconv.Itoa(record))
		taken = err == nil
	}
	return l.whose(f, record, taken), nil
}

// whose returns the account the reset, the clean and the deletion act
// on, from f, the account files' view of the service account's name,
// record, the UID its record holds or 0, and taken, whether an account
// has that UID.
//
// It's the account with the service account's name, if Silkpool
// recorded it or it carries Silkpool's marker, so that PrepareAccount
// would adopt it. Otherwise it's no account: either there's none, or the
// one with the name isn't Silkpool's, and is left alone. Even then, if
// the record names a UID no account has, the service account was
// deleted, and its processes may still run as that UID, which a new
// account could be given.
func (l Layout) whose(f found, record int, taken bool) serviceUser {
	switch {
	case f.exists && (f.uid == record || f.gecos == serviceMarker):
		return serviceUser{name: l.Account(), uid: f.uid, gid: f.gid, expire: f.expire}
	case record != 0 && !taken:
		return serviceUser{uid: record, gid: -1}
	}
	return serviceUser{}
}

// expire stops new starts for a: it restores its expiry, if it drifted.
func expire(a serviceUser) error {
	if a.name == "" || a.expire == expired {
		return nil
	}
	return system("usermod", "--expiredate", expired, a.name)
}

// unschedule removes a's crontab, its at jobs and its linger, and checks
// they're gone.
func (s *Slots) unschedule(ctx context.Context, a serviceUser) error {
	if a.name == "" {
		// Cron and at run nothing for an account that doesn't exist.
		return nil
	}
	return errors.Join(removeCrontab(a.name), removeAtJobs(a.name), s.removeLinger(ctx, a))
}

// removeCrontab removes the crontab of the account name, if cron is
// installed and it has one.
func removeCrontab(name string) error {
	if _, err := exec.LookPath("crontab"); err != nil {
		return nil
	}
	if out, err := command("crontab", "-u", name, "-r"); err != nil && !noCrontab(out) {
		return fmt.Errorf("crontab -r: %w: %s", err, bytes.TrimSpace(out))
	}
	if out, err := command("crontab", "-u", name, "-l"); err == nil || !noCrontab(out) {
		return fmt.Errorf("its crontab is still there: %s", bytes.TrimSpace(out))
	}
	return nil
}

// removeAtJobs removes the at jobs of the account name, if at is
// installed.
func removeAtJobs(name string) error {
	if _, err := exec.LookPath("atq"); err != nil {
		return nil
	}
	out, err := command("atq")
	if err != nil {
		return fmt.Errorf("atq: %w: %s", err, bytes.TrimSpace(out))
	}
	if jobs := atJobs(out, name); len(jobs) > 0 {
		if out, err := command("atrm", jobs...); err != nil {
			return fmt.Errorf("atrm: %w: %s", err, bytes.TrimSpace(out))
		}
	}
	out, err = command("atq")
	if err != nil {
		return fmt.Errorf("atq: %w: %s", err, bytes.TrimSpace(out))
	}
	if jobs := atJobs(out, name); len(jobs) > 0 {
		return fmt.Errorf("its at jobs %s are still there", strings.Join(jobs, ", "))
	}
	return nil
}

// atJobs returns the numbers of name's jobs in atq's output, which root
// gets for every account. Each line is a job's number, a tab, its time,
// its queue and its owner.
func atJobs(atq []byte, name string) []string {
	var jobs []string
	s := bufio.NewScanner(bytes.NewReader(atq))
	for s.Scan() {
		if f := strings.Fields(s.Text()); len(f) > 2 && f[len(f)-1] == name {
			jobs = append(jobs, f[0])
		}
	}
	return jobs
}

// lingerDir is where logind keeps a file for each account that lingers.
const lingerDir = "/var/lib/systemd/linger"

// removeLinger turns a's linger off, if it's on. The account is expired,
// so its user manager can't start anyway: this is a backstop.
func (s *Slots) removeLinger(ctx context.Context, a serviceUser) error {
	path := filepath.Join(lingerDir, a.name)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	// logind deletes the file and lets the account's user manager go. If
	// logind can't, deleting the file is enough: it's what logind reads.
	s.bus.Object("org.freedesktop.login1", "/org/freedesktop/login1").CallWithContext(ctx,
		"org.freedesktop.login1.Manager.SetUserLinger", 0, uint32(a.uid), false, false)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// killAll kills every process of a, once every slot is idle: it stops
// and kills a's user manager, then runs the kill helper as a until no
// process of a is left, and removes a's scheduled work again, since a
// process could have added some before it was killed.
func (s *Slots) killAll(ctx context.Context, a serviceUser) error {
	if a.uid <= 0 {
		return nil
	}
	if err := s.stopUnit(ctx, fmt.Sprintf("user@%d.service", a.uid)); err != nil {
		return err
	}
	for round := 1; ; round++ {
		if err := s.runKillAll(ctx, a); err != nil {
			return err
		}
		left, err := processesOf(a.uid)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			break
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
	return s.unschedule(ctx, a)
}

// units returns the names of slot n's runner units that systemd has
// loaded.
func (s *Slots) units(ctx context.Context, n int) ([]string, error) {
	var units []struct {
		Name, Description, Load, Active, Sub, Following string
		Path                                            dbus.ObjectPath
		JobID                                           uint32
		JobType                                         string
		JobPath                                         dbus.ObjectPath
	}
	patterns := []string{fmt.Sprintf("%s-slot%d-*.service", s.Name, n)}
	if err := s.manager(ctx, "ListUnitsByPatterns", []string{}, patterns).Store(&units); err != nil {
		return nil, fmt.Errorf("can't list its units: %w", err)
	}
	names := make([]string, len(units))
	for i, u := range units {
		names[i] = u.Name
	}
	return names, nil
}

// nogroup is the group the kill helper runs in for an account that's
// gone with its group: its group doesn't matter to kill(2).
const nogroup = 65534

// runKillAll runs the kill helper, `silkpool kill-all <uid>`, as a. A
// helper that a's processes killed first counts as a round: the next
// one tries again.
func (s *Slots) runKillAll(ctx context.Context, a serviceUser) error {
	gid := a.gid
	if gid < 0 {
		gid = nogroup
	}
	cmd := exec.CommandContext(ctx, s.Launcher, KillAllMode, strconv.Itoa(a.uid))
	cmd.Env, cmd.Dir = []string{}, "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(a.uid), Gid: uint32(gid), Groups: []uint32{}},
	}
	out, err := cmd.CombinedOutput()
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

// processesOf returns the PIDs of the live processes whose real or saved
// UID is uid, which are the ones kill(-1) run as uid reaches.
func processesOf(uid int) ([]int, error) {
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		// A process that ends meanwhile has no status to read.
		if status, err := os.ReadFile("/proc/" + d.Name() + "/status"); err == nil && runsAs(status, uid) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// runsAs reports whether the process whose /proc status is status is
// alive, and its real or saved UID is uid. A zombie is dead already: its
// parent, or PID 1, reaps it.
func runsAs(status []byte, uid int) bool {
	var state string
	var uids []string
	s := bufio.NewScanner(bytes.NewReader(status))
	for s.Scan() {
		key, value, _ := strings.Cut(s.Text(), ":")
		switch key {
		case "State":
			state = strings.TrimSpace(value)
		case "Uid":
			uids = strings.Fields(value)
		}
	}
	if len(uids) < 3 || strings.HasPrefix(state, "Z") || strings.HasPrefix(state, "X") {
		return false
	}
	id := strconv.Itoa(uid)
	return uids[0] == id || uids[2] == id
}

// recreateHome recreates the service account's home empty, a's, mode
// 0700. With no account, it's left gone, for PrepareAccount to make with
// the account.
func (l Layout) recreateHome(a serviceUser) error {
	if err := os.RemoveAll(l.Home()); err != nil {
		return err
	}
	if a.name == "" {
		return nil
	}
	return ownedDir(l.Home(), a.uid, a.gid, 0o700)
}

// command runs a system command in the C locale, whose messages the
// reset reads, and returns its output.
func command(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.CombinedOutput()
}

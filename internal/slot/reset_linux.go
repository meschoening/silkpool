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
	"unicode"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
)

// Reset resets slot n once its runner has ended, in the order "Define how
// the agent runs and launches slots on each OS"
// gives Linux, and returns what the node applies, as ResetSlot asks:
// LogsCopied, then SlotReset, or ResetFailed if a step failed. A failed
// step fails closed: the steps after it don't run, and the next attempt
// starts again from the first.
//
//  1. Stop new starts. The account is expired, so cron, at, sshd and its
//     user manager all refuse it. An expiry that drifted is restored.
//  2. Remove its scheduled work: its crontab, its at jobs and, as a
//     backstop, its linger.
//  3. Kill every process of the account. Each of the slot's units, and
//     the account's user manager, is stopped and killed, and then a
//     helper running as the account kills every process it may signal
//     (KillAll), until none is left. Its scheduled work is removed again
//     then, since a process could have added some before it was killed.
//  4. Copy the last runner's _diag logs into its folder in the agent's
//     logs, beside its listener's output, and read the run its Worker
//     log names (copyLogs).
//  5. Recreate the home empty.
//  6. Delete the runner root whole, with the runner's credential files,
//     _diag and _work.
//
// Linux has no shared-places sweep: under ProtectSystem=strict a slot
// can't write outside its own folders, so the set is empty.
func (s *Slots) Reset(ctx context.Context, n int) []node.Event {
	run, err := s.reset(ctx, n, time.Now())
	events := []node.Event{node.LogsCopied{Slot: n, Run: run}}
	if err != nil {
		return append(events, node.ResetFailed{Slot: n, Detail: detail(err)})
	}
	return append(events, node.SlotReset{Slot: n})
}

func (s *Slots) reset(ctx context.Context, n int, now time.Time) (node.Run, error) {
	var run node.Run
	a, err := s.owner(n)
	if err != nil {
		return run, fmt.Errorf("slot %d's reset couldn't read its account: %w", n, err)
	}
	steps := []struct {
		what string
		do   func() error
	}{
		{"stop new starts", func() error { return expire(a) }},
		{"remove its scheduled work", func() error { return s.unschedule(ctx, a) }},
		{"kill its processes", func() error { return s.killAll(ctx, n, a) }},
		{"copy its runner's logs", func() (err error) { run, err = s.copyLogs(n, a.uid, now); return err }},
		{"recreate its home", func() error { return s.recreateHome(n, a) }},
		{"delete its runner root", func() error { return os.RemoveAll(s.RunnerRoot(n)) }},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			return run, fmt.Errorf("slot %d's reset couldn't %s: %w", n, step.what, err)
		}
	}
	if err := os.Remove(filepath.Join(s.Folder(n), runnerFile)); err != nil && !os.IsNotExist(err) {
		return run, fmt.Errorf("slot %d's reset couldn't forget its runner: %w", n, err)
	}
	return run, nil
}

// DeleteAccount deletes slot n's account and the slot's folder, record
// included, as removing a node asks once the slot has been reset, and
// returns AccountDeleted or DeleteFailed. It never deletes an account
// Silkpool didn't make: one with the slot's name that it didn't record,
// and that lacks its marker, stays. It fails closed while a process of
// the account runs, as one would if the slot wasn't reset: userdel
// doesn't refuse it.
func (s *Slots) DeleteAccount(n int) node.Event {
	if err := s.deleteAccount(n); err != nil {
		return node.DeleteFailed{Slot: n, Detail: detail(fmt.Errorf("couldn't delete slot %d's account: %w", n, err))}
	}
	return node.AccountDeleted{Slot: n}
}

func (s *Slots) deleteAccount(n int) error {
	a, err := s.owner(n)
	if err != nil {
		return err
	}
	if a.uid > 0 {
		left, err := processesOf(a.uid)
		if err != nil {
			return err
		}
		if err := stillRun(left); err != nil {
			return err
		}
	}
	if a.name != "" {
		if err := system("userdel", a.name); err != nil {
			return err
		}
		f, err := readLocalAccount(a.name)
		if err != nil {
			return err
		}
		if f.exists {
			return errors.New("it's still there after userdel")
		}
		if f.group >= 0 {
			// userdel leaves its group when login.defs says so.
			if err := system("groupdel", a.name); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(s.Folder(n))
}

// A slotUser is the account slot n's reset and deletion act on.
type slotUser struct {
	// name is the account's name, or "" for none: either there's no slot
	// account, or it was deleted, and uid is the UID its processes may
	// still run as.
	name string
	// uid is the account's UID, or 0 if there's nothing to act on.
	uid int
	// gid is its primary group's ID, or -1 if it has none.
	gid int
	// expire is its shadow entry's expiry date field.
	expire string
}

// owner reads the account slot n's reset and deletion act on (whose).
func (s *Slots) owner(n int) (slotUser, error) {
	f, err := readLocalAccount(s.Account(n))
	if err != nil {
		return slotUser{}, err
	}
	record := s.recorded(n)
	taken := false
	if record != 0 {
		_, err := user.LookupId(strconv.Itoa(record))
		taken = err == nil
	}
	return s.whose(n, f, record, taken), nil
}

// whose returns the account slot n's reset and deletion act on, from f,
// the account files' view of the slot's name, record, the UID the slot's
// record holds or 0, and taken, whether an account has that UID.
//
// It's the account with the slot's name, if Silkpool recorded it or it
// carries Silkpool's marker, so that Prepare would adopt it. Otherwise
// it's no account: either there's none, or the one with the slot's name
// isn't Silkpool's, and is left alone. Even then, if the record names a
// UID no account has, the slot account was deleted, and its processes
// may still run as that UID, which a new account could be given.
func (l Layout) whose(n int, f found, record int, taken bool) slotUser {
	switch {
	case f.exists && (f.uid == record || f.gecos == marker(n)):
		return slotUser{name: l.Account(n), uid: f.uid, gid: f.gid, expire: f.expire}
	case record != 0 && !taken:
		return slotUser{uid: record, gid: -1}
	}
	return slotUser{}
}

// expire stops new starts for a: it restores its expiry, if it drifted.
func expire(a slotUser) error {
	if a.name == "" || a.expire == expired {
		return nil
	}
	return system("usermod", "--expiredate", expired, a.name)
}

// unschedule removes a's crontab, its at jobs and its linger, and checks
// they're gone.
func (s *Slots) unschedule(ctx context.Context, a slotUser) error {
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

// noCrontab reports whether crontab's output says the account has none.
func noCrontab(out []byte) bool { return bytes.Contains(out, []byte("no crontab for")) }

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
func (s *Slots) removeLinger(ctx context.Context, a slotUser) error {
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

// killRounds is how many times the reset runs its kill helper before it
// gives up on processes that won't die, such as one stuck in the kernel.
const killRounds = 10

// killAll kills every process of a, which ran slot n's runners. It stops
// and kills each unit of the slot and a's user manager, then runs the
// kill helper as a until no process of a is left, and removes a's
// scheduled work again.
func (s *Slots) killAll(ctx context.Context, n int, a slotUser) error {
	if a.uid <= 0 {
		return nil
	}
	units, err := s.units(ctx, n)
	if err != nil {
		return err
	}
	for _, unit := range append(units, fmt.Sprintf("user@%d.service", a.uid)) {
		var job dbus.ObjectPath
		if err := s.manager(ctx, "StopUnit", unit, "replace").Store(&job); err != nil && !gone(err) {
			return fmt.Errorf("can't stop %s: %w", unit, err)
		}
		if err := s.manager(ctx, "KillUnit", unit, "all", int32(syscall.SIGKILL)).Err; err != nil && !gone(err) {
			return fmt.Errorf("can't kill %s: %w", unit, err)
		}
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

// units returns the names of slot n's units that systemd has loaded: its
// runners' and its exposure checks'.
func (s *Slots) units(ctx context.Context, n int) ([]string, error) {
	var units []struct {
		Name, Description, Load, Active, Sub, Following string
		Path                                            dbus.ObjectPath
		JobID                                           uint32
		JobType                                         string
		JobPath                                         dbus.ObjectPath
	}
	patterns := []string{fmt.Sprintf("%s-slot%d-*.service", s.Name, n), s.checkUnit(n, "*")}
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
func (s *Slots) runKillAll(ctx context.Context, a slotUser) error {
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

// stillRun is the error for the account's processes pids that still
// run, or nil if there are none.
func stillRun(pids []int) error {
	switch len(pids) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("its process %d still runs", pids[0])
	}
	return fmt.Errorf("%d of its processes still run, such as %d", len(pids), pids[0])
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

// KillAllMode is the hidden argument that runs silkpool as the reset's
// kill helper, `silkpool kill-all <uid>`, which the agent runs as a slot
// account.
const KillAllMode = "kill-all"

// KillAll kills every process of the slot account it runs as, but
// itself, with one kill(-1, SIGKILL): the kernel signals every process
// the caller may signal while new ones can't start, so none forks its
// way past it. uid is the account it must run as. It refuses to run as
// any other, and as root, for which kill(-1) would end every process on
// the machine.
func KillAll(uid int) error {
	if err := checkKillUID(uid, os.Getuid(), os.Geteuid()); err != nil {
		return err
	}
	if err := syscall.Kill(-1, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// checkKillUID returns why the kill helper mustn't run, as real and
// effective UIDs, for the account want, or nil.
func checkKillUID(want, real, effective int) error {
	if want <= 0 || real != want || effective != want {
		return fmt.Errorf("%s runs only as the slot account it names (%d), not as UID %d", KillAllMode, want, real)
	}
	return nil
}

// recreateHome recreates slot n's home empty, a's, mode 0700. With no
// account, it's left gone, for Prepare to make with the account.
func (s *Slots) recreateHome(n int, a slotUser) error {
	if err := os.RemoveAll(s.Home(n)); err != nil {
		return err
	}
	if a.name == "" {
		return nil
	}
	return ownedDir(s.Home(n), a.uid, a.gid, 0o700)
}

// command runs a system command in the C locale, whose messages the
// reset reads, and returns its output.
func command(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.CombinedOutput()
}

// detail is err as a slot's failure detail: a sentence, with any secret
// in the text the commands it ran wrote scrubbed.
func detail(err error) string {
	d := secret.Scrub(err.Error())
	r, size := utf8.DecodeRuneInString(d)
	return string(unicode.ToUpper(r)) + d[size:]
}

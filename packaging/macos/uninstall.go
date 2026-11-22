package macos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/roleaccount"
	"github.com/meschoening/silkpool/internal/secret"
)

// A Result is how a program Uninstall runs ended: its exit code, and
// what it printed.
type Result struct {
	Code           int
	Stdout, Stderr string
}

// Removed are what an uninstall deletes, in order: the agent's folders,
// its state first, so that once that's gone a later uninstall finds
// nothing enrolled; then the launchd plists, the app and the binary.
var Removed = append(append([]string{}, Folders...), AgentPlist, TrayPlist, App, Binary)

// An Uninstall removes Silkpool from a Mac, as sudo silkpool uninstall
// does once the agent has said the machine isn't enrolled (ADR 0013).
type Uninstall struct {
	// Root is the file system's root: / on a Mac.
	Root string
	// Run runs the program name with args, as launchctl, pkgutil, who
	// and id run on a Mac. Its error is for a program that couldn't run.
	Run func(ctx context.Context, name string, args ...string) (Result, error)
	// Log is told each step as it's done.
	Log io.Writer
	// Running reports whether a process with the ID pid runs. Its error
	// is for an answer that's neither.
	Running func(pid int) (bool, error)
	// Gone is how long a job booted out has to go, or 30 s if it's
	// zero: launchctl bootout can return while launchd still tears the
	// job down, and launchd gives a job's process 20 s, its default
	// ExitTimeOut, to exit before it kills it.
	Gone time.Duration
	// Unresolved says what the service account's ledger shows of an
	// account that may still be Silkpool's: a record of the account's
	// name that still holds a GeneratedUID the ledger claims, or a
	// process holding the ledger's ID. Its error is for a ledger, records
	// or processes it couldn't read. On a Mac it's internal/slot's
	// Slots.Unresolved. Unset, the uninstall changes nothing.
	Unresolved func(ctx context.Context) ([]string, error)
	// DeleteAccount deletes the service account, as its ledger proves it
	// Silkpool's, and what it leaves on the Mac. It refuses, and deletes
	// nothing, while a process holds the account's UID or the ledger's
	// ID, or a slot's job or the exposure check's is loaded. It returns
	// the folders macOS refused to open even for root, where it found
	// nothing to delete, and the entries in the account's /var/folders
	// folders it refused, such as a data vault there (#294). On a Mac it's internal/slot's
	// Slots.DeleteAccount. Unset, the uninstall changes nothing.
	DeleteAccount func(ctx context.Context) (skipped []string, err error)
}

// Messages for what a refusal leaves the Mac as.
const (
	unchanged = "Nothing was changed."
	undeleted = "Nothing was deleted, though the agent is stopped."
	// ledgerKept follows a refusal that keeps the service account's
	// ledger.
	ledgerKept = "Silkpool's state is kept, with the service account's ledger, which is all that shows whose the account is."
	// rerun says how to run the uninstall again once it has booted the
	// agent out: it's refused while the state is there and the agent
	// isn't running to say the Mac isn't enrolled.
	rerun = "start Silkpool's service with `sudo launchctl bootstrap system " + AgentPlist + "`, then run `sudo silkpool uninstall` again"
)

// Do boots the tray app out of each signed-in session that has it, and
// the agent out of launchd, then deletes the service account, then
// Removed, and last forgets the .pkg's receipt. It stops at the first
// step that fails, and says which. A path that's a link is removed as
// the link, never followed.
//
// It changes nothing while launchd has a slot's job, which may still run
// a runner from the folders it deletes, or while the service account's
// ledger, which those folders hold and which alone shows whose the
// account is, can't be read. It looks again at both once the agent, which
// alone loads the jobs, is booted out. Then, if the ledger shows the
// account may still be Silkpool's, it deletes the account
// (DeleteAccount), and deletes Removed only once the ledger shows
// nothing more (deleteAccount). Before it deletes Silkpool's folder, it
// clears the user flags uchg and uappnd in it (clearUserFlags), which the
// service account may have set on what it owned there and which stop
// even root deleting it. It never boots out, kills or deletes a slot's
// job itself.
func (u Uninstall) Do(ctx context.Context) error {
	if u.DeleteAccount == nil {
		return fmt.Errorf("can't delete the service account. %s %s", unchanged, ledgerKept)
	}
	if err := u.noSlotJobs(ctx, unchanged); err != nil {
		return err
	}
	if _, err := u.unresolved(ctx, unchanged); err != nil {
		return err
	}
	uids, err := u.sessions(ctx)
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if err := u.bootOut(ctx, "gui/"+uid+"/"+TrayLabel); err != nil {
			return err
		}
	}
	if err := u.bootOut(ctx, AgentDomain+"/"+AgentLabel); err != nil {
		return err
	}
	if err := u.noSlotJobs(ctx, undeleted); err != nil {
		return err
	}
	if err := u.deleteAccount(ctx); err != nil {
		return err
	}
	for _, p := range Removed {
		name := filepath.Join(u.Root, filepath.FromSlash(p))
		if _, err := os.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if p == SilkpoolFolder {
			if err := clearUserFlags(name); err != nil {
				return fmt.Errorf("unlocking what's in %s: %w", p, err)
			}
		}
		if err := os.RemoveAll(name); err != nil {
			return fmt.Errorf("deleting %s: %w", p, err)
		}
		fmt.Fprintf(u.Log, "Deleted %s\n", p)
	}
	// pkgutil exits 1 for a receipt that isn't there and for its other
	// failures alike, so only its words tell them apart.
	r, err := u.Run(ctx, "pkgutil", "--pkg-info", ID)
	switch {
	case err != nil:
		return err
	case r.Code == 0:
		if err := u.must(ctx, "pkgutil", "--forget", ID); err != nil {
			return err
		}
		fmt.Fprintf(u.Log, "Forgot the receipt of %s\n", ID)
	case r.Code == 1 && strings.TrimSuffix(r.Stderr, "\n") == "No receipt for '"+ID+"' found at '/'.":
	default:
		return fmt.Errorf("pkgutil --pkg-info %s exited %d: %s, so whether its receipt is still there isn't known", ID, r.Code, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// SlotLabelPrefix starts the label of every slot's launchd job, which
// the agent loads into the system domain, as "Build the macOS slot role
// accounts and launchd slot jobs" labels them,
// io.github.meschoening.silkpool.slot.N, and internal/slot's darwin
// layout (layout_darwin.go) names them. cmd/silkpool's
// TestTheUninstallsSlotGuardMatchesTheSlotsJobs checks the two agree.
const SlotLabelPrefix = ID + ".slot."

// SlotDomain is the launchd domain noSlotJobs reads, where the agent
// loads the slots' jobs.
const SlotDomain = "system"

// noSlotJobs fails unless launchd's system domain has no slot's job:
// "not enrolled" doesn't show there's none, since a forced removal goes
// on when a slot's reset fails, and a slot's runner may still run in its
// job, from what an uninstall deletes. so says what that leaves the Mac
// as. It only reads, and fails closed.
//
// It asks for the system domain by name, with launchctl print system,
// rather than relying on whose domain launchctl list lists, and takes
// the answer only as that domain's: its first line names it, and its
// one services block, which it reads whole to its closing line, lists a
// job a line, by its PID or -, its last exit and its label. Any other
// line there, a block missing, there twice or cut short, or one that
// lists nothing, which no Mac's system domain does, is an error.
func (u Uninstall) noSlotJobs(ctx context.Context, so string) error {
	unknown := func(why string) error {
		return fmt.Errorf("%s, so whether a slot's runner job is still loaded isn't known. %s", why, so)
	}
	r, err := u.Run(ctx, "launchctl", "print", SlotDomain)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return unknown(fmt.Sprintf("launchctl print system exited %d: %s", r.Code, strings.TrimSpace(r.Stderr)))
	}
	labels, err := systemServices(r.Stdout)
	if err != nil {
		return unknown("launchctl print system " + err.Error())
	}
	var slots []string
	for _, label := range labels {
		if strings.HasPrefix(label, SlotLabelPrefix) {
			slots = append(slots, label)
		}
	}
	if len(slots) > 0 {
		return fmt.Errorf("a slot's runner job is still loaded: %s. %s Restart the Mac, which never loads slot jobs at boot, then run `sudo silkpool uninstall` again", strings.Join(slots, ", "), so)
	}
	return nil
}

// deleteAccount deletes the service account, once the agent is booted
// out, if its ledger shows the account may still be Silkpool's, and then
// fails unless the ledger shows nothing more. The ledger, in the folders
// the uninstall deletes, is all that shows whose the account is, so
// while it shows an account, or can't be read, or the deletion fails,
// the uninstall keeps it, and everything else, for the next attempt to
// finish. With nothing to show, as after an uninstall that stopped once
// the account was gone, it deletes nothing and says nothing. It lists
// each folder macOS refused to open even for root, and each entry in
// the account's /var/folders folders it refused, which DeleteAccount
// left.
//
// macOS refuses to delete a user record (roleaccount.ErrPermission) to a
// process whose app doesn't have Full Disk Access, even as root, and
// whatever the account: #192's probe found it refused a never-touched
// account as it refused one whose password was set, while a group was
// deleted. DeleteAccount tries the records before it deletes what's in
// Silkpool's folders, so that refusal leaves the account's home and the
// slots' folders as they were, and says which setting lets the next
// attempt finish.
func (u Uninstall) deleteAccount(ctx context.Context) error {
	left, err := u.unresolved(ctx, undeleted)
	if err != nil || len(left) == 0 {
		return err
	}
	skipped, err := u.DeleteAccount(ctx)
	for _, p := range skipped {
		fmt.Fprintf(u.Log, "Left %s, which macOS refused to open even as root, so nothing of the service account's there was deleted\n", p)
	}
	if errors.Is(err, roleaccount.ErrPermission) {
		return fmt.Errorf("couldn't delete the service account: macOS refused to delete its record (eDSPermissionError), "+
			"as it does unless the app sudo silkpool uninstall runs in has Full Disk Access. "+
			"The agent is stopped. The account's records, its home and the slots' folders remain; its scheduled work, what macOS keeps for it in /var/folders "+
			"and its folder in sfanalytics may have been deleted. Nothing else was. %s "+
			"Give that app, such as Terminal, Full Disk Access in System Settings > Privacy & Security > Full Disk Access, then quit and reopen it. "+
			"Over SSH, turn on \"Allow full disk access for remote users\" in System Settings > General > Sharing > Remote Login. Then "+rerun, ledgerKept)
	}
	if err != nil {
		return fmt.Errorf("couldn't delete the service account: %s. The agent is stopped, and nothing else was deleted. %s "+
			"Once what stopped it is put right, "+rerun, secret.Scrub(err.Error()), ledgerKept)
	}
	const deleted = "The agent is stopped and the service account was deleted, but nothing else was."
	left, err = u.unresolved(ctx, deleted)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("the service account's ledger still shows an account that may be Silkpool's once it was deleted:\n\t%s\n%s %s "+
			"Once those records are gone or hold other GeneratedUIDs, and no process holds those IDs, "+rerun,
			secret.Scrub(strings.Join(left, "\n\t")), deleted, ledgerKept)
	}
	fmt.Fprintln(u.Log, "Deleted the service account")
	return nil
}

// unresolved returns what Unresolved says shows the service account may
// still be Silkpool's, and fails while it can't tell, or isn't set. "Not
// enrolled" doesn't show the account is gone, and neither does a forced
// removal. so says what a failure leaves the Mac as. Its error is
// scrubbed, since it holds the machine's text.
func (u Uninstall) unresolved(ctx context.Context, so string) ([]string, error) {
	if u.Unresolved == nil {
		return nil, fmt.Errorf("can't check the service account's ledger. %s %s", so, ledgerKept)
	}
	left, err := u.Unresolved(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s, so whether the service account may still be Silkpool's isn't known. %s %s", secret.Scrub(err.Error()), so, ledgerKept)
	}
	return left, nil
}

// serviceLine is a line of launchctl print's services block: the job's
// PID, or - while it isn't running, its last exit, and its label.
var serviceLine = regexp.MustCompile(`^\t\t\s*([0-9]+|-)\s+(\S+)\s+(\S.*)$`)

// systemServices reads the labels of the jobs launchctl print system
// lists, or says why it can't.
func systemServices(out string) ([]string, error) {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if lines[0] != SlotDomain+" = {" {
		return nil, fmt.Errorf("printed %q first, not the system domain's name", lines[0])
	}
	var labels []string
	blocks := 0
	for i := 1; i < len(lines); i++ {
		if lines[i] != "\tservices = {" {
			continue
		}
		if blocks++; blocks > 1 {
			return nil, errors.New("printed two services blocks")
		}
		closed := false
		for i++; i < len(lines); i++ {
			if lines[i] == "\t}" {
				closed = true
				break
			}
			m := serviceLine.FindStringSubmatch(lines[i])
			if m == nil {
				return nil, fmt.Errorf("printed %q among its services, which isn't a job", lines[i])
			}
			labels = append(labels, strings.TrimSpace(m[3]))
		}
		if !closed {
			return nil, errors.New("printed a services block that doesn't end")
		}
	}
	if blocks == 0 {
		return nil, errors.New("printed no services block")
	}
	if len(labels) == 0 {
		return nil, errors.New("listed no services, which no Mac's system domain has")
	}
	return labels, nil
}

// sessions lists the UIDs of the accounts signed in at the Mac, by who's
// console lines, each once, with id's answer for its name: only such an
// account has the gui domain the tray app's job runs in. launchctl
// answers about the gui domain of an account that isn't signed in in
// more ways than its answer for a job it hasn't got, 125 among them, so
// the uninstall asks only about these, and takes no other answer of
// theirs as a job that isn't there.
func (u Uninstall) sessions(ctx context.Context) ([]string, error) {
	r, err := u.Run(ctx, "who")
	if err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("who exited %d, so the tray app's sessions can't be found: %s", r.Code, strings.TrimSpace(r.Stderr))
	}
	seen := map[string]bool{}
	var uids []string
	for line := range strings.Lines(r.Stdout) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 2 || strings.HasPrefix(f[0], "-") {
			return nil, fmt.Errorf("who listed %q, which isn't an account signed in", strings.TrimSpace(line))
		}
		if f[1] != "console" || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		r, err := u.Run(ctx, "id", "-u", f[0])
		if err != nil {
			return nil, err
		}
		uid := strings.TrimSpace(r.Stdout)
		if n, err := strconv.Atoi(uid); r.Code != 0 || err != nil || n < 0 || strconv.Itoa(n) != uid {
			return nil, fmt.Errorf("id -u %s exited %d with %q, so the tray app's session for %s can't be found: %s", f[0], r.Code, uid, f[0], strings.TrimSpace(r.Stderr))
		}
		uids = append(uids, uid)
	}
	return uids, nil
}

// bootOut boots the service target, as domain/label, out of launchd if
// launchd has it. Then it waits until launchd has the job no more and
// the process launchd named for it, if it ran one, has exited: launchd
// keeps a job it's tearing down until its process exits, which is why it
// refuses the job's plist again until then, and the process is checked
// all the same. A process ID used again by then reads as the job's still
// running, which fails closed. An answer launchctl gives that isn't one
// of those it gives for a job it has or a job or domain it hasn't stops
// the uninstall, as does a job or process still there once Gone is up.
func (u Uninstall) bootOut(ctx context.Context, target string) error {
	loaded, pid, err := u.service(ctx, target)
	if err != nil || !loaded {
		return err
	}
	if err := u.must(ctx, "launchctl", "bootout", target); err != nil {
		return err
	}
	gone := u.Gone
	if gone == 0 {
		gone = 30 * time.Second
	}
	deadline := time.Now().Add(gone)
	for {
		if loaded {
			if loaded, _, err = u.service(ctx, target); err != nil {
				return err
			}
		}
		running := false
		if !loaded && pid > 0 {
			if u.Running == nil {
				return fmt.Errorf("can't check whether %s's process %d has exited; nothing was deleted", target, pid)
			}
			if running, err = u.Running(pid); err != nil {
				return fmt.Errorf("checking whether %s's process %d has exited: %w; nothing was deleted", target, pid, err)
			}
		}
		if !loaded && !running {
			break
		}
		if time.Now().After(deadline) {
			if loaded {
				return fmt.Errorf("launchd still had %s %v after booting it out, so nothing was deleted", target, gone)
			}
			return fmt.Errorf("%s's process %d still ran %v after booting it out, so nothing was deleted", target, pid, gone)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	fmt.Fprintf(u.Log, "Stopped %s\n", target)
	return nil
}

// launchctl's exit codes for a job, or a domain, it hasn't got, as
// launchctl error names them: 113, "Could not find specified service",
// and 112, "Could not find specified domain", as for a gui domain whose
// account isn't signed in.
const (
	noService = 113
	noDomain  = 112
)

// service asks launchd whether it has the service target, and if it
// does, the process ID it names for it, if it's running one.
func (u Uninstall) service(ctx context.Context, target string) (loaded bool, pid int, err error) {
	r, err := u.Run(ctx, "launchctl", "print", target)
	if err != nil {
		return false, 0, err
	}
	loaded, pid, err = ServiceState(r)
	if err != nil {
		return false, 0, fmt.Errorf("launchctl print %s: %w, so whether it's stopped isn't known; nothing was deleted", target, err)
	}
	return loaded, pid, nil
}

// ServiceState reads launchctl print's answer about a service: whether
// launchd has it, and, if it does, the process ID it names on the
// service's own pid line, or 0 with none, as for a job that isn't
// running. Any other exit than 0 or those for a job or domain launchd
// hasn't got, or a pid line that's malformed or there twice, is an error.
func ServiceState(r Result) (loaded bool, pid int, err error) {
	switch r.Code {
	case 0:
	case noService, noDomain:
		return false, 0, nil
	default:
		return false, 0, fmt.Errorf("exited %d: %s", r.Code, strings.TrimSpace(r.Stderr))
	}
	for line := range strings.Lines(r.Stdout) {
		rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "\tpid = ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 || strconv.Itoa(n) != rest {
			return false, 0, fmt.Errorf("printed the pid line %q", strings.TrimSpace(line))
		}
		if pid != 0 {
			return false, 0, errors.New("printed two pid lines")
		}
		pid = n
	}
	return true, pid, nil
}

// must runs a program, and fails unless it exits 0.
func (u Uninstall) must(ctx context.Context, name string, args ...string) error {
	r, err := u.Run(ctx, name, args...)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return fmt.Errorf("%s %s exited %d: %s", name, strings.Join(args, " "), r.Code, strings.TrimSpace(r.Stderr))
	}
	return nil
}

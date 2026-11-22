package windows

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winexposure"
)

// An Uninstall is silkpool uninstall, once the agent has said the
// machine isn't enrolled: it deletes everything install put on the
// machine and the agent made, in Layout.
type Uninstall struct {
	Layout  Layout
	Machine Machine
	Log     io.Writer
	// ProfileWait is how long Windows may take to unload the service
	// account's profile once nothing of it runs, ProfilePoll how often
	// it's asked, or zero for a minute and a quarter of a second.
	ProfileWait, ProfilePoll time.Duration
}

// Refused is an uninstall that changed nothing, and why.
type Refused struct{ Why string }

func (e *Refused) Error() string { return e.Why }

// Do uninstalls Silkpool:
//
//  1. It refuses, changing nothing, while a slot's service runs, or while
//     the service account can't be deleted: while a process of it runs,
//     or if Silkpool can't show it made it (winaccount.CheckDelete).
//  2. It stops the agent's service, and ends the tray apps running from
//     the programs' folder.
//  3. It deletes the slots' services, takes away the guards the agent's
//     surveys put in the folders in Program Files that the service
//     account could change, which deny it by its SID (ADR 0019), then
//     deletes the service account, its profile, once Windows has
//     unloaded it, and its rights. If the
//     account can't be deleted now, it stops there, with the agent
//     stopped, and says to start it again.
//  4. It deletes the agent's service, the PATH entry, the Run, Start-menu
//     and Apps entries, and the agent's folder: its state, the account's
//     record among it, its logs and the slots' folders.
//  5. It deletes the programs' folder. A program Windows holds, as it
//     holds the running silkpool.exe that uninstalls, it moves out of
//     the folder, to be deleted when Windows next starts.
//
// Any other step that fails stops it there. Run again, it finishes
// what's left: a step whose work is done does nothing.
func (u Uninstall) Do() error {
	l, m := u.Layout, u.Machine
	slots, err := m.Slots(l.SlotService)
	if err != nil {
		return fmt.Errorf("can't list the slots' services: %w", err)
	}
	var running []string
	for _, s := range slots {
		if s.State != Stopped {
			running = append(running, s.Name)
		}
	}
	if len(running) > 0 {
		return &Refused{fmt.Sprintf("Silkpool can't be uninstalled while a slot's service runs: %s. "+
			"Wait for its runner to end, then run silkpool uninstall again.", strings.Join(running, ", "))}
	}
	recorded, err := winaccount.ReadRecord(l.AccountRecord())
	if err != nil {
		return fmt.Errorf("can't read the record of the service account %s: %w", l.Account.Name, err)
	}
	if err := winaccount.CheckDelete(m.Accounts(), l.Account, recorded); err != nil {
		return &Refused{accountRefusal(l.Account.Name, err)}
	}
	if err := m.StopService(l.Service); err != nil {
		return fmt.Errorf("stopping the %s service: %w", l.Service, err)
	}
	u.logf("Stopped the %s service.", l.Service)
	if err := m.EndPrograms(l.Dir); err != nil {
		return fmt.Errorf("ending the tray apps running from %s: %w", l.Dir, err)
	}
	for _, s := range slots {
		if err := m.DeleteService(s.Name); err != nil {
			return fmt.Errorf("deleting the slot's service %s: %w", s.Name, err)
		}
	}
	if err := u.unguard(recorded); err != nil {
		return err
	}
	if err := u.deleteAccount(recorded); err != nil {
		return err
	}
	u.logf("Deleted the service account %s.", l.Account.Name)
	if err := m.DeleteService(l.Service); err != nil {
		return fmt.Errorf("deleting the %s service: %w", l.Service, err)
	}
	path, err := m.Path()
	if err != nil {
		return fmt.Errorf("reading the system PATH: %w", err)
	}
	if hasEntry(path, l.Dir) {
		if err := m.SetPath(withoutEntry(path, l.Dir)); err != nil {
			return fmt.Errorf("removing %s from the system PATH: %w", l.Dir, err)
		}
	}
	if err := m.DeleteRun(l.RunValue); err != nil {
		return fmt.Errorf("deleting the tray app's Run entry: %w", err)
	}
	if err := m.Remove(l.Shortcut); err != nil {
		return fmt.Errorf("deleting the Start-menu entry: %w", err)
	}
	if err := m.DeleteApps(l.AppsKey); err != nil {
		return fmt.Errorf("deleting the Apps entry: %w", err)
	}
	if err := m.RemoveAll(l.Root); err != nil {
		return fmt.Errorf("deleting %s: %w", l.Root, err)
	}
	if err := u.removePrograms(); err != nil {
		return err
	}
	u.logf("Silkpool is uninstalled.")
	return nil
}

// unguard takes the guards the agent's surveys put away again, as its
// guard ledger records them, while the account they deny, recorded, is
// still there. The ledger goes with the agent's folder.
func (u Uninstall) unguard(recorded string) error {
	b, err := os.ReadFile(u.Layout.GuardRecord())
	if errors.Is(err, fs.ErrNotExist) || recorded == "" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the record of the folders Silkpool guards: %w", err)
	}
	places := winexposure.ParseGuarded(b)
	if len(places) == 0 {
		return nil
	}
	// The latest survey counts on the guards, so it goes first: an agent
	// started again after an uninstall cut short surveys again before
	// any slot starts.
	if err := u.Machine.Remove(u.Layout.SurveyRecord()); err != nil {
		return fmt.Errorf("deleting the latest survey: %w", err)
	}
	if err := u.Machine.Unguard(recorded, places); err != nil {
		return fmt.Errorf("taking Silkpool's denials to its service account away from the folders in Program Files: %w", err)
	}
	u.logf("Took Silkpool's denials to its service account away from %d folders in Program Files.", len(places))
	return nil
}

// deleteAccount deletes the service account, its profile and its
// rights, as the account-wide clean deletes its profile: once Windows
// has unloaded it, since a profile deleted while its hive is still
// loaded, as for a while after the account's last process ends, goes
// from Windows' list but leaves its folder. A folder of the account's
// that Windows' deletion left anyway it deletes too. Windows' list no
// longer names that folder once the profile is deleted, so the folder
// is recorded in the agent's state first, which an uninstall cut short
// keeps, until the folder is gone.
func (u Uninstall) deleteAccount(recorded string) error {
	l, m := u.Layout, u.Machine
	sid := recorded
	if a, ok, err := m.Accounts().Lookup(l.Account.Name); err != nil {
		return fmt.Errorf("can't read the service account %s: %w", l.Account.Name, err)
	} else if ok {
		sid = a.SID
	}
	if sid != "" {
		if err := u.waitUnloaded(sid); err != nil {
			return fmt.Errorf("deleting the profile of the service account %s: %w", l.Account.Name, err)
		}
		folder, err := m.ProfileFolder(sid)
		if err != nil {
			return fmt.Errorf("finding the profile of the service account %s: %w", l.Account.Name, err)
		}
		if l.profileFolder(folder) {
			if err := os.WriteFile(l.profileRecord(), []byte(folder+"\n"), 0o600); err != nil {
				return fmt.Errorf("recording the profile folder of the service account %s: %w", l.Account.Name, err)
			}
		}
	}
	if err := winaccount.Delete(m.Accounts(), l.Account, recorded); err != nil {
		return fmt.Errorf("%s The %s service is stopped: start it again with Start-Service %s, in an administrator PowerShell, then run silkpool uninstall again once the machine is ready",
			accountRefusal(l.Account.Name, err), l.Service, l.Service)
	}
	b, err := os.ReadFile(l.profileRecord())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the record of the profile folder of the service account %s: %w", l.Account.Name, err)
	}
	if folder := strings.TrimSpace(string(b)); l.profileFolder(folder) {
		if err := m.RemoveAll(folder); err != nil {
			return fmt.Errorf("deleting the profile folder %s of the service account %s: %w", folder, l.Account.Name, err)
		}
	}
	if err := os.Remove(l.profileRecord()); err != nil {
		return fmt.Errorf("deleting the record of the profile folder of the service account %s: %w", l.Account.Name, err)
	}
	return nil
}

// profileRecord is the file in the agent's state that records the
// service account's profile folder while the uninstall deletes it.
func (l Layout) profileRecord() string { return filepath.Join(l.State(), "service-account-profile") }

// profileFolder reports whether folder is one the uninstall may delete
// as the account's profile: one named for the account, as Windows names
// a profile's folder, by its name, or its name and a suffix Windows adds
// to tell it apart.
func (l Layout) profileFolder(folder string) bool {
	base := strings.ToLower(folder[strings.LastIndexAny(folder, `\/`)+1:])
	name := strings.ToLower(l.Account.Name)
	return folder != "" && (base == name || strings.HasPrefix(base, name+"."))
}

// waitUnloaded waits until Windows no longer has sid's profile loaded.
func (u Uninstall) waitUnloaded(sid string) error {
	wait, poll := u.ProfileWait, u.ProfilePoll
	if wait == 0 {
		wait = time.Minute
	}
	if poll == 0 {
		poll = 250 * time.Millisecond
	}
	for deadline := time.Now().Add(wait); ; time.Sleep(poll) {
		loaded, err := u.Machine.ProfileLoaded(sid)
		if err != nil || !loaded {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("it's still loaded %v after nothing of the account ran", wait)
		}
	}
}

// removePrograms deletes the programs' folder, moving out the programs
// Windows holds.
func (u Uninstall) removePrograms() error {
	l, m := u.Layout, u.Machine
	for _, p := range Programs {
		for _, path := range []string{filepath.Join(l.Dir, p), old(filepath.Join(l.Dir, p))} {
			err := m.Remove(path)
			if errors.Is(err, ErrInUse) {
				var aside string
				aside, err = m.MoveAside(path, l.asideName())
				if err == nil {
					u.logf("%s is in use, so Windows deletes it, as %s, when it next starts.", path, aside)
				}
			}
			if err != nil {
				return fmt.Errorf("deleting %s: %w", path, err)
			}
		}
	}
	if err := m.RemoveAll(l.Dir); err != nil {
		return fmt.Errorf("deleting %s: %w", l.Dir, err)
	}
	return nil
}

// accountRefusal says why the service account name can't be deleted:
// Silkpool's own words, with any text from the machine scrubbed.
func accountRefusal(name string, err error) string {
	var foreign *winaccount.Foreign
	var busy *winaccount.Busy
	switch {
	case errors.As(err, &busy):
		return fmt.Sprintf("Silkpool can't be uninstalled while processes of its service account %s run (%d now). "+
			"Wait for them to end, or end them, then run silkpool uninstall again.", name, busy.Processes)
	case errors.As(err, &foreign):
		return fmt.Sprintf("Silkpool can't be uninstalled: an account named %s is on this machine, and Silkpool can't show it made it, so it won't delete it.", name)
	}
	return fmt.Sprintf("Silkpool can't be uninstalled, since it can't check its service account %s: %s.", name, secret.Scrub(err.Error()))
}

func (u Uninstall) logf(format string, args ...any) {
	if u.Log != nil {
		fmt.Fprintf(u.Log, format+"\n", args...)
	}
}

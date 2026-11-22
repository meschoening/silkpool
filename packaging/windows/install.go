package windows

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winexposure"
	"github.com/meschoening/silkpool/packaging/linux"
)

// A State is how the service manager shows a service.
type State int

const (
	// Missing is a service that doesn't exist.
	Missing State = iota
	// Stopped is a service that isn't running.
	Stopped
	// Running is a service in any other state: starting, running,
	// stopping or paused.
	Running
)

func (s State) String() string {
	switch s {
	case Missing:
		return "missing"
	case Stopped:
		return "stopped"
	}
	return "running"
}

// A Service is how install registers the agent's service: an
// auto-start service running Program with Args as LocalSystem, which the
// service manager restarts when it fails.
type Service struct {
	Name, DisplayName, Description string
	Program                        string
	Args                           []string
}

// An AppsEntry is the Apps entry's values, under HKLM's Uninstall key.
type AppsEntry struct {
	DisplayName, DisplayVersion, Publisher, InstallLocation, DisplayIcon string
	UninstallString                                                      string
}

// A Slot is a slot's service, as uninstall finds it.
type Slot struct {
	Name  string
	State State
}

// ErrInUse is a file's deletion or replacement refused because Windows
// holds it, as it holds a running program.
var ErrInUse = errors.New("the file is in use")

// A Machine is what install and uninstall read and change of Windows:
// Local on Windows, and fakes in the tests.
type Machine interface {
	// Version returns the version a program's version resource says, and
	// false if there's no file at path. A program with no version
	// resource is an error.
	Version(path string) (string, bool, error)
	// Exists reports whether there's a file or folder at path.
	Exists(path string) (bool, error)
	// MakeDir makes the folder path and its parents, as Program Files'
	// ACL gives them.
	MakeDir(path string) error
	// Copy writes the file src to dst whole or not at all, replacing a
	// file there that Windows doesn't hold.
	Copy(src, dst string) error
	// WriteFile writes data to path whole or not at all.
	WriteFile(path string, data []byte) error
	// Rename renames a file, even one Windows holds as a running program,
	// within its folder.
	Rename(from, to string) error
	// Remove deletes the file or empty folder path, and nothing if it's
	// gone. It fails with ErrInUse while Windows holds it.
	Remove(path string) error
	// RemoveAll deletes the folder path and what's in it, never following
	// a link, and nothing if it's gone.
	RemoveAll(path string) error
	// MoveAside moves a file Windows holds out of its folder, into the
	// machine's temporary folder under a name starting with prefix, and
	// marks it to be deleted when Windows next starts. It returns the new
	// path.
	MoveAside(path, prefix string) (string, error)
	// MakeRoot makes the agent's folder, with its state and its log
	// folders, as the agent makes them, or checks them if they're there.
	MakeRoot(root string) error
	// Accounts are the machine's local accounts.
	Accounts() winaccount.Machine

	// ServiceState returns the service name's state.
	ServiceState(name string) (State, error)
	// SetService makes the service, or sets an existing one to it,
	// with its recovery actions.
	SetService(s Service) error
	// StartService starts the service name and waits until it runs.
	StartService(name string) error
	// StopService stops the service name, if it runs, and waits until
	// it has stopped.
	StopService(name string) error
	// DeleteService deletes the service name, if it's there, and waits
	// until it's gone.
	DeleteService(name string) error
	// Slots returns the slots' services: those whose names start with
	// prefix and that carry the slot hosts' marker as their description.
	Slots(prefix string) ([]Slot, error)

	// Path returns the system PATH's entries, and SetPath sets them and
	// tells running programs it changed.
	Path() ([]string, error)
	SetPath(entries []string) error
	// SetRun sets the value name under HKLM's Run key to command, and
	// DeleteRun deletes it.
	SetRun(name, command string) error
	DeleteRun(name string) error
	// SetApps writes the Apps entry key, replacing what was there, and
	// DeleteApps deletes it.
	SetApps(key string, e AppsEntry) error
	DeleteApps(key string) error
	// EndPrograms ends every process whose program is in the folder dir,
	// but this one, and waits until they've ended.
	EndPrograms(dir string) error
	// ProfileLoaded reports whether Windows has sid's profile loaded, and
	// ProfileFolder returns its folder, as Windows lists the profile, or
	// "" if it lists none.
	ProfileLoaded(sid string) (bool, error)
	ProfileFolder(sid string) (string, error)
	// Unguard takes the guard that denies sid away from each of places,
	// the folders in Program Files the agent's surveys guarded
	// (winexposure.Unguard).
	Unguard(sid string, places []winexposure.Place) error
}

// An Install is silkpool install: Silkpool at Version, from the
// programs in the folder From, into Layout.
type Install struct {
	Layout  Layout
	Version string
	From    string
	Machine Machine
	Log     io.Writer
}

// Downgrade is the refusal of an older release over a newer one.
type Downgrade struct{ Installed, Version string }

func (e *Downgrade) Error() string {
	return fmt.Sprintf("Refusing to downgrade Silkpool from %s to %s: Silkpool can't go back a version.", e.Installed, e.Version)
}

// Do installs Silkpool, or upgrades it, or installs the same version
// again, which repairs what's missing:
//
//  1. It refuses an older version than the one installed, changing
//     nothing.
//  2. It makes the agent's folders, as the agent does, and the service
//     account, recording its SID in the agent's state.
//  3. It stops the agent's service, if it runs, and only that: slot
//     services run from copies of their own, and carry on.
//  4. It copies the programs into the layout's folder. One Windows holds,
//     as a running tray app holds its program, it renames aside first,
//     and deletes once nothing holds it.
//  5. It registers the agent's service and starts it.
//  6. It adds the folder to the system PATH, and the Run, Start-menu and
//     Apps entries.
//
// A step that fails stops it there. Run again, it starts over, and
// finishes what's left.
func (in Install) Do() error {
	l, m := in.Layout, in.Machine
	if err := linux.CheckVersion(in.Version); err != nil {
		return err
	}
	installed, there, err := m.Version(l.Program())
	if err != nil {
		return fmt.Errorf("can't tell which version of Silkpool is installed: %w", err)
	}
	if there && linux.CompareVersions(in.Version, installed) < 0 {
		return &Downgrade{installed, in.Version}
	}
	for _, p := range Programs {
		ok, err := m.Exists(filepath.Join(in.From, p))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s isn't beside silkpool.exe in %s, so this isn't a whole release", p, in.From)
		}
	}
	if err := m.MakeRoot(l.Root); err != nil {
		return fmt.Errorf("making the agent's folder: %w", err)
	}
	if err := winaccount.Keep(m.Accounts(), l.Account, l.AccountRecord()); err != nil {
		return fmt.Errorf("making the service account: %w", err)
	}
	in.logf("The service account %s is ready.", l.Account.Name)
	state, err := m.ServiceState(l.Service)
	if err != nil {
		return err
	}
	if state != Missing {
		if err := m.StopService(l.Service); err != nil {
			return fmt.Errorf("stopping the %s service: %w", l.Service, err)
		}
	}
	if err := in.copyPrograms(); err != nil {
		return err
	}
	if err := m.SetService(Service{Name: l.Service, DisplayName: l.DisplayName, Description: ServiceDescription, Program: l.Program(), Args: l.AgentArgs}); err != nil {
		return fmt.Errorf("registering the %s service: %w", l.Service, err)
	}
	if err := m.StartService(l.Service); err != nil {
		return fmt.Errorf("starting the %s service: %w", l.Service, err)
	}
	in.logf("The %s service is running Silkpool %s.", l.Service, in.Version)
	if err := in.entries(); err != nil {
		return err
	}
	in.removeOld()
	switch {
	case !there:
		in.logf("Silkpool %s is installed.", in.Version)
	case installed == in.Version:
		in.logf("Silkpool %s is installed again.", in.Version)
	default:
		in.logf("Silkpool is upgraded from %s to %s.", installed, in.Version)
	}
	return nil
}

// ServiceDescription is the agent's service's description.
const ServiceDescription = "Runs Silkpool's agent, which runs GitHub Actions jobs on this machine for its Hub."

// old is the name a replaced program is renamed to, until nothing holds
// it.
func old(path string) string { return path + ".old" }

// copyPrograms copies the programs into the layout's folder, unless
// they're being installed from there.
func (in Install) copyPrograms() error {
	l, m := in.Layout, in.Machine
	if samePath(in.From, l.Dir) {
		return nil
	}
	if err := m.MakeDir(l.Dir); err != nil {
		return err
	}
	for _, p := range Programs {
		dst := filepath.Join(l.Dir, p)
		if err := in.setAside(old(dst)); err != nil {
			return err
		}
		there, err := m.Exists(dst)
		if err != nil {
			return err
		}
		if there {
			if err := m.Rename(dst, old(dst)); err != nil {
				return fmt.Errorf("renaming %s aside: %w", dst, err)
			}
		}
		if err := m.Copy(filepath.Join(in.From, p), dst); err != nil {
			return fmt.Errorf("copying %s: %w", p, err)
		}
	}
	return nil
}

// setAside makes way for a file at path: it deletes one there, or, if
// Windows holds it, moves it out of the folder, to be deleted when
// Windows next starts.
func (in Install) setAside(path string) error {
	err := in.Machine.Remove(path)
	if errors.Is(err, ErrInUse) {
		_, err = in.Machine.MoveAside(path, in.Layout.asideName())
	}
	return err
}

// asideName starts the name of a file moved aside: the layout's
// folder's name, so a test's starts with its prefix.
func (l Layout) asideName() string { return filepath.Base(l.Dir) + "-" }

// removeOld deletes the programs renamed aside, which nothing holds once
// the agent has restarted, unless a tray app still runs from one: that
// one goes at the next install, or when Windows next starts.
func (in Install) removeOld() {
	for _, p := range Programs {
		path := old(filepath.Join(in.Layout.Dir, p))
		if err := in.Machine.Remove(path); errors.Is(err, ErrInUse) {
			if _, err := in.Machine.MoveAside(path, in.Layout.asideName()); err != nil {
				in.logf("%s is in use, so it stays until the next install: %v", path, err)
			}
		} else if err != nil {
			in.logf("Couldn't delete %s: %v", path, err)
		}
	}
}

// entries adds the folder to the system PATH, and the Run, Start-menu
// and Apps entries.
func (in Install) entries() error {
	l, m := in.Layout, in.Machine
	path, err := m.Path()
	if err != nil {
		return fmt.Errorf("reading the system PATH: %w", err)
	}
	if !hasEntry(path, l.Dir) {
		if err := m.SetPath(append(path, l.Dir)); err != nil {
			return fmt.Errorf("adding %s to the system PATH: %w", l.Dir, err)
		}
	}
	if err := m.SetRun(l.RunValue, l.RunCommand()); err != nil {
		return fmt.Errorf("adding the tray app's Run entry: %w", err)
	}
	link, err := Shortcut{Target: l.TrayProgram(), WorkingDir: l.Dir, Description: AppName}.Bytes()
	if err != nil {
		return err
	}
	if err := m.MakeDir(filepath.Dir(l.Shortcut)); err != nil {
		return err
	}
	if err := m.WriteFile(l.Shortcut, link); err != nil {
		return fmt.Errorf("adding the Start-menu entry: %w", err)
	}
	if err := m.SetApps(l.AppsKey, AppsEntry{
		DisplayName:     AppName,
		DisplayVersion:  in.Version,
		Publisher:       AppName,
		InstallLocation: l.Dir,
		DisplayIcon:     l.TrayProgram(),
		UninstallString: l.UninstallCommand(),
	}); err != nil {
		return fmt.Errorf("adding the Apps entry: %w", err)
	}
	return nil
}

// hasEntry reports whether the PATH entries hold dir, as Windows
// compares paths: ignoring case and a last backslash.
func hasEntry(entries []string, dir string) bool {
	for _, e := range entries {
		if samePath(e, dir) {
			return true
		}
	}
	return false
}

// withoutEntry is entries without dir.
func withoutEntry(entries []string, dir string) []string {
	var kept []string
	for _, e := range entries {
		if !samePath(e, dir) {
			kept = append(kept, e)
		}
	}
	return kept
}

// samePath reports whether a and b name the same path, as Windows
// compares them: ignoring case and a last separator.
func samePath(a, b string) bool {
	trim := func(p string) string { return strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`) }
	return strings.EqualFold(trim(a), trim(b))
}

func (in Install) logf(format string, args ...any) {
	if in.Log != nil {
		fmt.Fprintf(in.Log, format+"\n", args...)
	}
}

package windows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winexposure"
	"github.com/meschoening/silkpool/internal/winslot"
)

// Local is this machine, as install and uninstall change it. MakeRoot
// is the agent's own way of making its folders, which cmd/silkpool
// holds.
type Local struct {
	MakeRootFunc func(root string) error
}

var _ Machine = Local{}

// LocalLayout is the layout on this machine: a node's, or a test's for
// prefix, if it isn't "".
func LocalLayout(prefix string) (Layout, error) {
	var folders [3]string
	for i, id := range []*windows.KNOWNFOLDERID{windows.FOLDERID_ProgramFiles, windows.FOLDERID_ProgramData, windows.FOLDERID_CommonPrograms} {
		f, err := windows.KnownFolderPath(id, 0)
		if err != nil {
			return Layout{}, fmt.Errorf("finding the machine's folders: %w", err)
		}
		folders[i] = f
	}
	if prefix == "" {
		return Installed(folders[0], folders[1], folders[2]), nil
	}
	if err := CheckPrefix(prefix); err != nil {
		return Layout{}, err
	}
	return ForTest(prefix, folders[0], folders[1], folders[2]), nil
}

// Elevated reports whether this process runs with an administrator's
// elevated token, as LocalSystem does too.
func Elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

func (Local) Version(path string) (string, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	v, ok, err := ReadVersion(f)
	if err != nil {
		return "", false, fmt.Errorf("reading %s's version: %w", path, err)
	}
	if !ok {
		return "", false, fmt.Errorf("%s says no version", path)
	}
	return v, true, nil
}

func (Local) Exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (Local) MakeDir(path string) error { return os.MkdirAll(path, 0o755) }

func (l Local) Copy(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return l.WriteFile(dst, data)
}

// WriteFile writes a new file beside path, then renames it over path,
// which fails with ErrInUse while Windows holds the file there.
func (Local) WriteFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".new-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = inUse(os.Rename(f.Name(), path))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (Local) Rename(from, to string) error { return inUse(os.Rename(from, to)) }

func (Local) Remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return inUse(err)
}

// RemoveAll fails with ErrInUse while Windows holds a file in the
// folder, or the folder itself, as a running program's current folder.
func (Local) RemoveAll(path string) error { return inUse(os.RemoveAll(path)) }

// inUse wraps err with ErrInUse where Windows refused because it holds
// the file: a running program can't be deleted or replaced, though it
// can be renamed.
func inUse(err error) error {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return fmt.Errorf("%w: %w", ErrInUse, err)
	}
	return err
}

func (Local) DeleteAtStart(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

func (Local) MoveAside(path, prefix string) (string, error) {
	var id asideID
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	aside := filepath.Join(os.TempDir(), prefix+hex.EncodeToString(id[:])+"-"+filepath.Base(path))
	from, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	to, err := windows.UTF16PtrFromString(aside)
	if err != nil {
		return "", err
	}
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		return "", fmt.Errorf("moving %s to %s: %w", path, aside, err)
	}
	if err := windows.MoveFileEx(to, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); err != nil {
		return aside, fmt.Errorf("marking %s to be deleted when Windows starts: %w", aside, err)
	}
	return aside, nil
}

// MakeRoot makes the agent's folders owned by SYSTEM, as the agent does.
// An administrator's token may make SYSTEM a new folder's owner only
// with the restore privilege enabled, which it holds but doesn't enable
// until asked; LocalSystem owns what it makes anyway.
func (l Local) MakeRoot(root string) error {
	if l.MakeRootFunc == nil {
		return errors.New("no way to make the agent's folder was given")
	}
	enablePrivilege("SeRestorePrivilege")
	return l.MakeRootFunc(root)
}

func (Local) Accounts() winaccount.Machine { return winaccount.Local{} }

// The waits for the service manager.
const (
	serviceWait = 2 * time.Minute
	servicePoll = 250 * time.Millisecond
)

// openService opens the service name, or returns nil if there's none.
func openService(m *mgr.Mgr, name string) (*mgr.Service, error) {
	s, err := m.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil
	}
	return s, err
}

// withService runs f with the service name, or with nil if there's
// none.
func withService(name string, f func(*mgr.Mgr, *mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := openService(m, name)
	if err != nil {
		return err
	}
	if s != nil {
		defer s.Close()
	}
	return f(m, s)
}

// readManager opens the service manager with rights, to read services'
// states and configs, unlike mgr.Connect, which asks for every right.
// Connecting alone needs no administrator; listing the services needs
// an administrator's or an interactive or service logon, which the
// uninstall has.
func readManager(rights uint32) (*mgr.Mgr, error) {
	h, err := windows.OpenSCManager(nil, nil, rights)
	if err != nil {
		return nil, err
	}
	return &mgr.Mgr{Handle: h}, nil
}

// readService opens the service name to read its state and config, or
// returns nil if there's none.
func readService(m *mgr.Mgr, name string) (*mgr.Service, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.OpenService(m.Handle, p, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mgr.Service{Name: name, Handle: h}, nil
}

func (Local) ServiceState(name string) (State, error) {
	m, err := readManager(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return Missing, err
	}
	defer m.Disconnect()
	s, err := readService(m, name)
	if err != nil || s == nil {
		return Missing, err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return Missing, err
	}
	return stateOf(st), nil
}

func stateOf(st svc.Status) State {
	if st.State == svc.Stopped {
		return Stopped
	}
	return Running
}

// Recovery is the agent's service's recovery actions: restart it after
// 5 s, 30 s, then a minute, counting failures afresh after a day. They
// apply to its exiting with an error too, not only to its crashing.
var Recovery = []mgr.RecoveryAction{
	{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	{Type: mgr.ServiceRestart, Delay: time.Minute},
}

// recoveryReset is when the service manager counts failures afresh, in
// seconds.
const recoveryReset = 24 * 60 * 60

func (Local) SetService(c Service) error {
	return withService(c.Name, func(m *mgr.Mgr, s *mgr.Service) error {
		config := mgr.Config{
			ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
			StartType:    mgr.StartAutomatic,
			ErrorControl: mgr.ErrorNormal,
			DisplayName:  c.DisplayName,
			Description:  c.Description,
		}
		if s == nil {
			var err error
			if s, err = m.CreateService(c.Name, c.Program, config, c.Args...); err != nil {
				return err
			}
			defer s.Close()
		} else {
			config.BinaryPathName = commandLineEscaped(c.Program, c.Args)
			config.ServiceStartName = "LocalSystem"
			config.Dependencies = []string{}
			if err := s.UpdateConfig(config); err != nil {
				return err
			}
		}
		if err := s.SetRecoveryActions(Recovery, recoveryReset); err != nil {
			return fmt.Errorf("setting its recovery actions: %w", err)
		}
		if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
			return fmt.Errorf("setting its recovery actions for its errors: %w", err)
		}
		return nil
	})
}

// commandLineEscaped is program and args as the service manager's
// CreateService writes them, which UpdateConfig takes whole.
func commandLineEscaped(program string, args []string) string {
	words := []string{windows.EscapeArg(program)}
	for _, a := range args {
		words = append(words, windows.EscapeArg(a))
	}
	return strings.Join(words, " ")
}

func (Local) StartService(name string) error {
	return withService(name, func(_ *mgr.Mgr, s *mgr.Service) error {
		if s == nil {
			return fmt.Errorf("there's no service %s", name)
		}
		if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return err
		}
		for deadline := time.Now().Add(serviceWait); ; time.Sleep(servicePoll) {
			st, err := s.Query()
			if err != nil {
				return err
			}
			switch {
			case st.State == svc.Running:
				return nil
			case st.State == svc.Stopped:
				return fmt.Errorf("it stopped as it started, with exit code %d (its own %d)", st.Win32ExitCode, st.ServiceSpecificExitCode)
			case time.Now().After(deadline):
				return fmt.Errorf("it isn't running %v after it was started", serviceWait)
			}
		}
	})
}

func (Local) StopService(name string) error {
	return withService(name, func(_ *mgr.Mgr, s *mgr.Service) error {
		if s == nil {
			return nil
		}
		return stop(s)
	})
}

// stop stops s and waits until it has stopped.
func stop(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State != svc.Stopped && st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
	}
	for deadline := time.Now().Add(serviceWait); ; time.Sleep(servicePoll) {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("it hasn't stopped %v after it was asked to", serviceWait)
		}
	}
}

func (Local) DeleteService(name string) error {
	err := withService(name, func(_ *mgr.Mgr, s *mgr.Service) error {
		if s == nil {
			return nil
		}
		if err := stop(s); err != nil {
			return err
		}
		if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The service goes once every handle to it has closed.
	for deadline := time.Now().Add(serviceWait); ; time.Sleep(servicePoll) {
		state, err := Local{}.ServiceState(name)
		if err != nil {
			return err
		}
		if state == Missing {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service %s is marked for deletion, but still there %v later: close any program that has it open, such as Services", name, serviceWait)
		}
	}
}

func (Local) Slots(prefix string) ([]Slot, error) {
	m, err := readManager(windows.SC_MANAGER_CONNECT | windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, err
	}
	defer m.Disconnect()
	names, err := m.ListServices()
	if err != nil {
		return nil, err
	}
	var slots []Slot
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		s, err := readService(m, name)
		if err != nil {
			return nil, err
		}
		if s == nil {
			continue // deleted since it was listed
		}
		c, cerr := s.Config()
		st, qerr := s.Query()
		s.Close()
		if err := errors.Join(cerr, qerr); err != nil {
			return nil, fmt.Errorf("reading the service %s: %w", name, err)
		}
		if c.Description == winslot.Marker {
			slots = append(slots, Slot{Name: name, State: stateOf(st)})
		}
	}
	return slots, nil
}

// The registry keys install writes, under HKEY_LOCAL_MACHINE.
const (
	environmentKey = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	runKey         = `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`
	appsKey        = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
)

func (Local) Path() ([]string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, environmentKey, registry.QUERY_VALUE)
	if err != nil {
		return nil, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return splitPath(v), nil
}

func (Local) SetPath(entries []string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, environmentKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetExpandStringValue("Path", strings.Join(entries, ";")); err != nil {
		return err
	}
	settingChanged("Environment")
	return nil
}

// splitPath splits a PATH value into its entries, leaving out a last
// empty one, which a value ending with ";" has.
func splitPath(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(v, ";"), ";")
}

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
)

// settingChanged tells the running programs that listen, as Explorer
// does, that what's named has changed, so programs they start see the
// new PATH. It waits up to 5 s for each, and skips one that hangs.
func settingChanged(what string) {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001a
		smtoAbortIfHung = 0x0002
	)
	p, err := windows.UTF16PtrFromString(what)
	if err != nil {
		return
	}
	var result uintptr
	procSendMessageTimeoutW.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(p)), smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}

func (Local) SetRun(name, command string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, command)
}

func (Local) DeleteRun(name string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func (l Local) SetApps(key string, e AppsEntry) error {
	if err := l.DeleteApps(key); err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, appsKey+`\`+key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	for _, v := range [][2]string{
		{"DisplayName", e.DisplayName},
		{"DisplayVersion", e.DisplayVersion},
		{"Publisher", e.Publisher},
		{"InstallLocation", e.InstallLocation},
		{"DisplayIcon", e.DisplayIcon},
		{"UninstallString", e.UninstallString},
	} {
		if err := k.SetStringValue(v[0], v[1]); err != nil {
			return err
		}
	}
	// It has no Modify or Repair: running install again does both.
	for _, name := range []string{"NoModify", "NoRepair"} {
		if err := k.SetDWordValue(name, 1); err != nil {
			return err
		}
	}
	return nil
}

func (Local) DeleteApps(key string) error {
	err := registry.DeleteKey(registry.LOCAL_MACHINE, appsKey+`\`+key)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

func (Local) EndPrograms(owns func(program string) bool) error {
	// An administrator holds the debug privilege, which opens other
	// accounts' processes, but it isn't enabled until asked for.
	enablePrivilege("SeDebugPrivilege")
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	self := windows.GetCurrentProcessId()
	var ended []windows.Handle
	defer func() {
		for _, h := range ended {
			windows.CloseHandle(h)
		}
	}()
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == self || e.ProcessID == 0 {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, e.ProcessID)
		if err != nil {
			continue // one this process can't open can't be the tray app it would end
		}
		program, err := processProgram(h)
		if err != nil || !owns(program) {
			windows.CloseHandle(h)
			continue
		}
		if err := windows.TerminateProcess(h, 1); err != nil {
			windows.CloseHandle(h)
			return fmt.Errorf("ending %s (process %d): %w", program, e.ProcessID, err)
		}
		ended = append(ended, h)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	for _, h := range ended {
		if ev, err := windows.WaitForSingleObject(h, 10000); err != nil || ev != windows.WAIT_OBJECT_0 {
			return errors.New("a process ended hasn't exited 10 s later")
		}
	}
	return nil
}

// processProgram is the full path of the process h's program.
func processProgram(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// enablePrivilege enables the privilege name in this process's token,
// if it holds it.
func enablePrivilege(name string) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return
	}
	defer token.Close()
	var luid windows.LUID
	n, _ := windows.UTF16PtrFromString(name)
	if err := windows.LookupPrivilegeValue(nil, n, &luid); err != nil {
		return
	}
	privs := windows.Tokenprivileges{PrivilegeCount: 1}
	privs.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	windows.AdjustTokenPrivileges(token, false, &privs, 0, nil, nil)
}

func (Local) ProfileLoaded(sid string) (bool, error) {
	k, err := registry.OpenKey(registry.USERS, sid, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	k.Close()
	return true, nil
}

// profileListKey lists the machine's profiles, under HKEY_LOCAL_MACHINE,
// each by its account's SID.
const profileListKey = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`

func (Local) ProfileFolder(sid string) (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileListKey+`\`+sid, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("ProfileImagePath")
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return registry.ExpandString(v)
}

func (Local) Unguard(sid string, places []winexposure.Place) error {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return fmt.Errorf("the service account's SID %q: %w", sid, err)
	}
	return winexposure.Unguard(context.Background(), s, places)
}

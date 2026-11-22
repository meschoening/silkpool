package slot

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/meschoening/silkpool/internal/winaccount"
)

// local is this machine, as the clean reads and changes it. The agent
// runs as LocalSystem, which can read every task's definition, read and
// end every process the account can start, and delete the account's
// profile and its folders in the Recycle Bins.
type local struct{ winaccount.Local }

var _ Machine = local{}

// taskFileCap is the largest task definition Tasks reads. Task
// Scheduler's own are a few kilobytes.
const taskFileCap = 1 << 20

// tasksFolder is where Windows keeps each scheduled task's definition,
// System32\Tasks, by the task's path.
func tasksFolder() (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(sys, "Tasks"), nil
}

// Tasks returns the paths of the tasks whose definitions name sid as a
// principal's user, by its SID or by a name that Windows maps to it. It
// reads each definition in System32\Tasks, following no link, and fails
// on one it can't read, naming it, rather than pass it for nobody's.
func (local) Tasks(sid string) ([]string, error) {
	want, err := windows.StringToSid(sid)
	if err != nil {
		return nil, err
	}
	dir, err := tasksFolder()
	if err != nil {
		return nil, err
	}
	var tasks []string
	err = filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Only Task Scheduler writes here: a link or a junction isn't a
		// task's definition, and isn't followed.
		if e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 || e.IsDir() {
			return nil
		}
		b, err := readCapped(path, taskFileCap)
		if err != nil {
			return err
		}
		users, err := taskUsers(b)
		if err != nil {
			return fmt.Errorf("%s %w", path, err)
		}
		for _, u := range users {
			if runsAsUser(u, want) {
				rel, err := filepath.Rel(dir, path)
				if err != nil {
					return err
				}
				tasks = append(tasks, `\`+rel)
				break
			}
		}
		return nil
	})
	return tasks, err
}

// readCapped reads the file path, failing if it's over limit bytes.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s is over %d bytes", path, limit)
	}
	return b, err
}

// runsAsUser reports whether a task's user, a SID or a name, is want. A
// name Windows can't map to an account is nobody's.
func runsAsUser(user string, want *windows.SID) bool {
	if strings.HasPrefix(user, "S-1-") {
		s, err := windows.StringToSid(user)
		return err == nil && s.Equals(want)
	}
	s, _, _, err := windows.LookupSID("", user)
	return err == nil && s.Equals(want)
}

// endTaskScript ends the task its variables name, if Task Scheduler
// says it's running, as Stop-ScheduledTask does: through the task's
// IRegisteredTask, which ends every running instance. Get-ScheduledTask's
// -TaskPath and -TaskName take wildcards, so a name such as build[1]
// would match build1: the script lists every task and compares its
// folder and name as plain text instead, ignoring case, as Task
// Scheduler does. A task that's gone has nothing to end. Its State is
// the enum's name, the same in every language.
const endTaskScript = `$ErrorActionPreference = 'Stop'
$folder, $name = $env:SILKPOOL_TASK_FOLDER, $env:SILKPOOL_TASK_NAME
foreach ($t in @(Get-ScheduledTask | Where-Object { $_.TaskPath -eq $folder -and $_.TaskName -eq $name })) {
	if ($t.State -eq 'Running') { $t | Stop-ScheduledTask }
}`

// EndTask ends the running instance of the task path, if it runs,
// through Task Scheduler, in Windows PowerShell, with the task's folder
// and name in its environment, never its command line.
func (local) EndTask(path string) error {
	folder, name := splitTask(path)
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(sys, "WindowsPowerShell", "v1.0", "powershell.exe"),
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", endTaskScript)
	cmd.Env = append(os.Environ(), "SILKPOOL_TASK_FOLDER="+folder, "SILKPOOL_TASK_NAME="+name)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ending it with Stop-ScheduledTask: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// DeleteTask deletes the task path through schtasks.exe, Task
// Scheduler's own tool, which needs no COM.
func (local) DeleteTask(path string) error {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(sys, "schtasks.exe"), "/Delete", "/TN", path, "/F")
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Delete: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// Pids returns the processes whose token's user is sid, but this one. A
// process whose token can't be read, a protected one, which the account
// can't start, or one that has ended, isn't one.
func (local) Pids(sid string) ([]uint32, error) {
	want, err := windows.StringToSid(sid)
	if err != nil {
		return nil, err
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	self := windows.GetCurrentProcessId()
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var pids []uint32
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID != self && runsAs(e.ProcessID, want) {
			pids = append(pids, e.ProcessID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return pids, nil
}

// runsAs reports whether process pid's token's user is sid.
func runsAs(pid uint32, sid *windows.SID) bool {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(p)
	var t windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &t); err != nil {
		return false
	}
	defer t.Close()
	u, err := t.GetTokenUser()
	return err == nil && u.User.Sid.Equals(sid)
}

// Terminate ends the process pid. One that has ended already, whose ID
// no longer opens, is ended.
func (local) Terminate(pid uint32) error {
	p, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(p)
	if err := windows.TerminateProcess(p, 1); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	// TerminateProcess is refused for a process that's already ending,
	// which the next round's look tells apart from one that runs on.
	return nil
}

// ProfileLoaded reports whether sid's profile's hive is loaded, under
// HKEY_USERS\<sid>.
func (local) ProfileLoaded(sid string) (bool, error) {
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

// RecycleBins returns sid's folders, $Recycle.Bin\<sid>, on the
// machine's fixed drives, where they're there. A network drive, a
// removable one or an optical one isn't the machine's own.
func (local) RecycleBins(sid string) ([]string, error) {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	var bins []string
	for i := range 26 {
		if drives&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			return nil, err
		}
		if windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		bin := filepath.Join(root, "$Recycle.Bin", sid)
		_, err = os.Lstat(bin)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		bins = append(bins, bin)
	}
	return bins, nil
}

// Package windows packages the Windows node, from "Define how each binary
// is packaged, installed, upgraded and signed per OS", recorded as ADR
// 0013: the zip a release publishes for each architecture, holding
// silkpool.exe and silkpool-tray.exe, unsigned, and what silkpool install
// and silkpool uninstall do with them, since Windows has no package
// manager Silkpool uses.
//
// silkpool install, run elevated from the zip's folder, copies the
// programs into C:\Program Files\Silkpool, makes the machine's service
// account, registers the Silkpool service, which runs the agent as
// LocalSystem, starts it at boot and restarts it when it fails, and adds
// the folder to the system PATH, the tray app's Run entry, a Start-menu
// entry and an Apps entry whose uninstall runs silkpool uninstall. Run
// again from a newer release's zip, it upgrades: it stops only the
// agent's service, swaps the files Windows holds by renaming them, and
// starts it again, so runners carry on (ADR 0006). It refuses an older
// release's.
//
// silkpool uninstall is refused while the machine is enrolled. Once it
// isn't, it deletes everything install put on the machine and the agent
// made: the service account and its profile, the slots' services, the
// agent's service, the entries, the state, the logs and the programs.
//
// The rules are portable, and run against a Machine: the real one on
// Windows, and fakes in the tests, so they're tested on every OS. Every
// name and place is a Layout's, so privileged tests install a copy of
// their own that never meets a real install.
package windows

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winexposure"
	"github.com/meschoening/silkpool/internal/winslot"
	"github.com/meschoening/silkpool/packaging/linux"
)

// The programs the zip holds, by their file names, which are their
// names in the install's folder too.
const (
	Silkpool = "silkpool.exe"
	Tray     = "silkpool-tray.exe"
)

// Programs are the zip's files, in its order.
var Programs = []string{Silkpool, Tray}

// Arches are the architectures a release has a zip for, by their GOARCH
// names.
var Arches = []string{"amd64", "arm64"}

// FileName is the zip's file name for the release at version and arch,
// such as silkpool_1.2.3_windows_amd64.zip.
func FileName(version, arch string) string {
	return "silkpool_" + version + "_windows_" + arch + ".zip"
}

// The check gate's privileged-windows lane builds the zips from the
// commit at two versions, the Linux packages' (linux.CheckOlder and
// linux.CheckNewer), for the packages' own test, which installs,
// upgrades and downgrades between them on the Windows VM. It leaves each
// version's zip for amd64 in a folder named for it, in CheckDir, beside
// the test's binary (docs/testing.md).
const (
	CheckDir   = "check-packages"
	CheckOlder = linux.CheckOlder
	CheckNewer = linux.CheckNewer
)

// A Layout is where an install puts Silkpool and the names it gives what
// it registers. Installed is a node's. Tests use their own (ForTest).
type Layout struct {
	// Dir is the programs' folder, C:\Program Files\Silkpool.
	Dir string
	// Root is the agent's folder, C:\ProgramData\Silkpool, which holds
	// its state, its logs, the slots' folders and the slot hosts'
	// copies of silkpool.exe. The agent makes it, owned by SYSTEM.
	Root string
	// Service is the agent's service, Silkpool, and DisplayName the name
	// Services shows for it.
	Service, DisplayName string
	// Pipe is the agent's control channel.
	Pipe string
	// Account is the machine's service account, which every slot runs as.
	Account winaccount.Spec
	// NodeJob is the job object that holds every slot's (ADR 0014).
	NodeJob string
	// SlotService starts the name of each slot's service, followed by
	// its number.
	SlotService string
	// AgentArgs are the agent service's arguments after its program.
	AgentArgs []string
	// RunValue is the tray app's value under HKLM's Run key.
	RunValue string
	// AppsKey is the Apps entry's key under HKLM's Uninstall key.
	AppsKey string
	// Shortcut is the Start-menu entry, a shell link in the Start menu's
	// programs folder for every account.
	Shortcut string
	// Prefix is a test's prefix, which install and uninstall are given
	// with --prefix, or "" for a node's.
	Prefix string
}

// AppName is the name the Start menu, Services and Apps show.
const AppName = "Silkpool"

// SlotService starts the names of a node's slots' services.
const SlotService = "SilkpoolSlot"

// Installed is a node's layout, given the machine's Program Files,
// ProgramData and the Start menu's programs folder for every account.
func Installed(programFiles, programData, programs string) Layout {
	return Layout{
		Dir:         filepath.Join(programFiles, AppName),
		Root:        filepath.Join(programData, AppName),
		Service:     AppName,
		DisplayName: AppName,
		Pipe:        control.Path,
		Account:     winaccount.Service,
		NodeJob:     winslot.NodeJobName,
		SlotService: SlotService,
		AgentArgs:   []string{"agent"},
		RunValue:    AppName,
		AppsKey:     AppName,
		Shortcut:    filepath.Join(programs, AppName+".lnk"),
	}
}

// ForTest is a test's layout, every name in it starting with prefix,
// which the privileged-windows lane gives each test (docs/testing.md).
// Its folders and entries are named prefix without its last hyphen, and
// the agent runs with the folder, pipe, account, node job and slots'
// services named for the test.
func ForTest(prefix, programFiles, programData, programs string) Layout {
	name := strings.TrimSuffix(prefix, "-")
	l := Layout{
		Dir:         filepath.Join(programFiles, name),
		Root:        filepath.Join(programData, name),
		Service:     prefix + "agent",
		DisplayName: prefix + "agent",
		Pipe:        `\\.\pipe\` + prefix + "agent",
		Account:     winaccount.Spec{Name: prefix + "sa", Marker: winaccount.Service.Marker},
		NodeJob:     `Global\` + prefix + "node",
		SlotService: prefix + "s",
		RunValue:    name,
		AppsKey:     name,
		Shortcut:    filepath.Join(programs, name+".lnk"),
		Prefix:      prefix,
	}
	l.AgentArgs = []string{"agent", "--root", l.Root, "--pipe", l.Pipe, "--account", l.Account.Name, "--node-job", l.NodeJob, "--slot-service", l.SlotService}
	return l
}

// CheckPrefix returns an error unless prefix is one a test's layout may
// have: lowercase words of letters and digits, each followed by a
// hyphen, short enough that the service account's name, the longest,
// fits Windows' 20 characters. A node's names start with none of them,
// being capitalized.
func CheckPrefix(prefix string) error {
	if !prefixPattern.MatchString(prefix) || len(prefix+"sa") > 20 {
		return &prefixError{prefix}
	}
	return nil
}

var prefixPattern = regexp.MustCompile(`^([a-z0-9]+-)+$`)

type prefixError struct{ prefix string }

func (e *prefixError) Error() string {
	return "--prefix " + e.prefix + " isn't a test's prefix: lowercase words of letters and digits, each followed by a hyphen, " +
		"short enough that its service account's name fits Windows' 20 characters"
}

// Program and TrayProgram are the installed programs' paths.
func (l Layout) Program() string     { return filepath.Join(l.Dir, Silkpool) }
func (l Layout) TrayProgram() string { return filepath.Join(l.Dir, Tray) }

// State and Logs are the agent's state and log folders in its root.
func (l Layout) State() string { return filepath.Join(l.Root, "state") }
func (l Layout) Logs() string  { return filepath.Join(l.Root, "logs") }

// AccountRecord is the file in the agent's state that records the
// service account's SID (winaccount.Keep).
func (l Layout) AccountRecord() string { return filepath.Join(l.State(), winaccount.RecordFile) }

// SurveyRecord is the file in the agent's state that keeps the latest
// survey of the shared places (winexposure.SurveyFile).
func (l Layout) SurveyRecord() string { return filepath.Join(l.State(), winexposure.SurveyFile) }

// GuardRecord is the file in the agent's state that records each folder
// in Program Files its surveys guarded (winexposure.GuardFile).
func (l Layout) GuardRecord() string { return filepath.Join(l.State(), winexposure.GuardFile) }

// Quote quotes a path for a command line, as Windows reads its first
// word: in double quotes, which no path holds.
func Quote(path string) string { return `"` + path + `"` }

// RunCommand is the tray app's Run entry: started at sign-in, with
// --sign-in, so it shows nothing in an account that isn't an
// administrator's. The Start-menu entry passes nothing.
func (l Layout) RunCommand() string { return Quote(l.TrayProgram()) + " --sign-in" }

// UninstallCommand is the Apps entry's uninstall: silkpool uninstall,
// with a test's prefix.
func (l Layout) UninstallCommand() string {
	args := []string{"uninstall"}
	if l.Prefix != "" {
		args = append(args, "--prefix", l.Prefix)
	}
	return commandLine(l.Program(), args)
}

// commandLine is program and args as a command line, each argument
// quoted only if it holds a space, which no argument here ends with or
// holds a quote or a backslash before.
func commandLine(program string, args []string) string {
	words := []string{Quote(program)}
	for _, a := range args {
		if strings.ContainsAny(a, " \t") {
			a = `"` + a + `"`
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

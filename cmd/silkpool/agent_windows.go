package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"

	"github.com/meschoening/silkpool/internal/agent"
	"github.com/meschoening/silkpool/internal/agentlog"
	"github.com/meschoening/silkpool/internal/agentos"
	"github.com/meschoening/silkpool/internal/agentstore"
	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/runnerinstall"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/slot"
	"github.com/meschoening/silkpool/internal/version"
	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winacl"
	"github.com/meschoening/silkpool/internal/winexecpolicy"
	"github.com/meschoening/silkpool/internal/winsac"
	"github.com/meschoening/silkpool/internal/winslot"
	"github.com/meschoening/silkpool/internal/winzone"
	"github.com/meschoening/silkpool/internal/wire"
	pkgwin "github.com/meschoening/silkpool/packaging/windows"
)

func init() {
	modes["agent"] = runAgent
	modes[winslot.Mode] = func(_ context.Context, args []string) int { return winslot.Main(args) }
	startCommand = "Start-Service Silkpool, in an administrator PowerShell"
	elevate = "from an administrator PowerShell"
	smartAppControl = winsac.Read
	executionPolicy = winexecpolicy.Read
}

// The agent's folders under its root, C:\ProgramData\Silkpool, as
// "Define how each binary is packaged, installed, upgraded and signed per
// OS" has them: its state and its log, each SYSTEM's and Administrators'
// alone.
const (
	stateFolder = "state"
	logsFolder  = "logs"
)

// The folders' descriptors. Each is owned by SYSTEM, with a DACL
// protected from what's above it. The root gives SYSTEM and
// Administrators full control, and Users only read and run what's in it,
// so no one else can make, rename or replace a folder there. The state
// and the log give SYSTEM and Administrators full control, and no one
// else anything. Each entry is inherited by everything made inside.
const (
	rootSDDL    = "O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"
	privateSDDL = "O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
)

// fileAllAccess is FILE_ALL_ACCESS, SDDL's FA, which x/sys doesn't name.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

// inherited is an entry's flags for a folder's children: OI and CI.
const inherited = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE

// privateEntries and rootEntries are privateSDDL's and rootSDDL's
// DACLs, as the agent checks them. 0x1200a9 is FILE_GENERIC_READ and
// FILE_EXECUTE.
var (
	privateEntries = []winacl.Entry{
		{SID: winacl.WellKnown(windows.WinLocalSystemSid), Mask: fileAllAccess, Flags: inherited},
		{SID: winacl.WellKnown(windows.WinBuiltinAdministratorsSid), Mask: fileAllAccess, Flags: inherited},
	}
	rootEntries = append(privateEntries[:len(privateEntries):len(privateEntries)],
		winacl.Entry{SID: winacl.WellKnown(windows.WinBuiltinUsersSid), Mask: 0x1200a9, Flags: inherited})
)

// runAgent runs the agent service, as the SCM starts it, under
// LocalSystem. It idles while the machine isn't enrolled. Its folders are
// under --root, or C:\ProgramData\Silkpool, and its pipe is --pipe, or
// \\.\pipe\silkpool. Its service account is --account, or silkpool, its
// node job --node-job, or Global\silkpool-node, and its slots' services
// are named --slot-service, or SilkpoolSlot, followed by each slot's
// number. Tests give their own, so as never to touch a real install.
func runAgent(_ context.Context, args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	root := fs.String("root", "", "the agent's folder, which holds its state and its log")
	pipe := fs.String("pipe", control.Path, "the control channel's pipe")
	account := fs.String("account", winaccount.Service.Name, "the service account's name")
	job := fs.String("node-job", winslot.NodeJobName, "the node job's name")
	slotService := fs.String("slot-service", pkgwin.SlotService, "what each slot's service's name starts with")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: silkpool agent [--root <folder>] [--pipe <name>] [--account <name>] [--node-job <name>] [--slot-service <name>]")
		return 2
	}
	if *root == "" {
		data, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "silkpool agent: finding ProgramData:", err)
			return 1
		}
		*root = filepath.Join(data, "Silkpool")
	}
	if err := runsAsSystem(); err != nil {
		fmt.Fprintln(os.Stderr, "silkpool agent:", err)
		return 1
	}
	inService, err := svc.IsWindowsService()
	if err != nil || !inService {
		fmt.Fprintln(os.Stderr, "silkpool agent: the agent runs as the Silkpool service, which the service manager starts")
		return 1
	}
	// A service of its own process gets its name from the SCM, so the
	// name here isn't read: a test's service runs the agent too.
	n := names{root: *root, pipe: *pipe, account: winaccount.Spec{Name: *account, Marker: winaccount.Service.Marker}, nodeJob: *job, slotService: *slotService}
	if err := svc.Run("Silkpool", &service{n}); err != nil {
		return 1
	}
	return 0
}

// runsAsSystem fails unless the process runs as LocalSystem.
func runsAsSystem() error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !u.User.Sid.Equals(winacl.WellKnown(windows.WinLocalSystemSid)) {
		return errors.New("the agent runs as LocalSystem, as its service starts it")
	}
	return nil
}

// names are what the agent's service names on the machine: its folder,
// its pipe, its service account, its node job and its slots' services.
type names struct {
	root, pipe  string
	account     winaccount.Spec
	nodeJob     string
	slotService string
}

// A service is the agent under the SCM.
type service struct{ names }

// Execute runs the agent until the SCM stops it, or it fails. It tells
// the SCM it's running once the pipe listens and the agent answers.
// Failing, it exits with a code of its own, so the SCM's recovery
// actions restart it.
func (s *service) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s.names, func() { close(ready) }) }()
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	for {
		select {
		case <-ready:
			changes <- svc.Status{State: svc.Running, Accepts: accepts}
			ready = nil
		case err := <-done:
			if err != nil {
				return true, 1
			}
			return false, 0
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				changes <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				if err := <-done; err != nil {
					return true, 1
				}
				return false, 0
			}
		}
	}
}

// serve runs the agent until ctx ends, from its folders under its root,
// serving its control channel at its pipe, and calls ready once both
// answer. It writes why it ended to its own log, once that's open.
func serve(ctx context.Context, n names, ready func()) error {
	if err := makeRoot(n.root); err != nil {
		return err
	}
	stateDir, logDir := filepath.Join(n.root, stateFolder), filepath.Join(n.root, logsFolder)
	file, err := agentlog.Open(logDir)
	if err != nil {
		return err
	}
	defer file.Close()
	log := slog.New(agentlog.Handler(file))
	l := slot.Layout{Account: n.account, Root: n.root, Logs: logDir, Service: n.slotService, NodeJob: n.nodeJob}
	l.Program = hostCopy(l.Root, log)
	slots := slot.Open(l)
	if err := slots.PruneCopies(); err != nil {
		log.Warn("couldn't delete the slot hosts' old copies of the agent's program", "err", err)
	}
	if err := run(ctx, n, stateDir, log, file, slots, ready); err != nil {
		log.Error("the agent stopped", "err", err)
		return err
	}
	return nil
}

// slotHostFolder is the folder under the agent's root of the slot hosts'
// copies of silkpool.exe, one per version (winslot.Copies, ADR 0006).
const slotHostFolder = "slot-host"

// hostCopy makes the slot hosts' copy of this program for its version,
// under the agent's root, which each slot's service is moved to between
// its runners, and returns its path. A copy it can't make is logged, and
// its path is still where it would be: each slot then fails to start,
// saying why.
func hostCopy(root string, log *slog.Logger) string {
	copies := winslot.Copies{Dir: filepath.Join(root, slotHostFolder)}
	self, err := os.Executable()
	if err == nil {
		_, err = copies.Make(version.Version, self)
	}
	if err != nil {
		log.Error("can't copy the agent's program for the slot hosts, so no slot can start", "err", err)
	}
	return copies.Path(version.Version)
}

// installs are the runner application's installs, in the agent's state,
// for the machine's service account as the state records it: install
// made it, and the agent keeps it (winaccount.Keep), so its SID is read
// again at each install and link.
type installs struct {
	dir      string
	platform runnerapp.Platform
	record   string
}

var _ agentos.Installs = installs{}

// with returns the installs for the service account the state records.
func (i installs) with() (runnerinstall.Installs, error) {
	sid, err := winaccount.ReadRecord(i.record)
	if err == nil && sid == "" {
		err = errors.New("no SID is recorded for it")
	}
	if err != nil {
		return runnerinstall.Installs{}, fmt.Errorf("can't read the service account: %w", err)
	}
	return runnerinstall.Installs{Dir: i.dir, Platform: i.platform, Account: sid}, nil
}

func (i installs) Install(ctx context.Context, app node.InstallRunnerApp) node.Event {
	in, err := i.with()
	if err != nil {
		return node.InstallFailed{Version: app.Version, Reason: secret.Scrub(err.Error())}
	}
	return in.Install(ctx, app)
}

func (i installs) Found() ([]string, error) {
	return runnerinstall.Installs{Dir: i.dir, Platform: i.platform}.Found()
}

func (i installs) Delete(version string) error {
	return runnerinstall.Installs{Dir: i.dir, Platform: i.platform}.Delete(version)
}

func (i installs) Link(version, root string) error {
	in, err := i.with()
	if err != nil {
		return err
	}
	return in.Link(version, root)
}

// makeRoot makes the agent's root, its state and its log folders, or
// checks them, as the agent makes them. silkpool install makes them the
// same way, before it records the service account in the state.
func makeRoot(root string) error {
	if err := secureRoot(root); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join(root, stateFolder), filepath.Join(root, logsFolder)} {
		if err := securePrivate(dir); err != nil {
			return err
		}
	}
	return nil
}

// A nodeJob is the node job the agent keeps for its life, so that its
// limits hold every slot host's job (ADR 0014), and its watch, which
// tells which slot's job ran out of the memory budget. If it can't be
// opened as the agent starts, as when someone else has made a job of its
// name, each budget tries again; meanwhile slot hosts start nothing,
// since they won't run a listener outside it. It's the slots' Jobs.
type nodeJob struct {
	name string
	// slots names the slots' jobs, which the watch looks in.
	slots func() []string
	log   *slog.Logger
	mu    sync.Mutex
	job   *winslot.NodeJob
	watch *winslot.Watch
}

var _ slot.Jobs = (*nodeJob)(nil)

// open opens the node job, unless it's open.
func (n *nodeJob) open() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.openLocked()
}

// openLocked opens the node job, and starts watching it. A watch that
// can't start is logged, and tried again with the job's next use.
func (n *nodeJob) openLocked() error {
	if n.job == nil {
		j, err := winslot.OpenNodeJob(n.name)
		if err != nil {
			return err
		}
		n.job = j
	}
	if n.watch == nil {
		w, err := n.job.Watch(n.slots)
		if err != nil {
			n.log.Warn("can't watch the node job, so a slot that runs out of the memory budget isn't told", "err", err)
			return nil
		}
		n.watch = w
	}
	return nil
}

// OutOfMemory reports whether the slot job named job ran out of the
// memory budget since it was last asked, as the watch heard, and forgets
// it. Without a watch, none did.
func (n *nodeJob) OutOfMemory(job string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.openLocked() != nil || n.watch == nil {
		return false
	}
	return n.watch.OutOfMemory(job)
}

// SetBudget holds the slots to b.
func (n *nodeJob) SetBudget(b availability.Budget) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.openLocked(); err != nil {
		return err
	}
	return n.job.SetBudget(b)
}

// close stops the watch, and closes the agent's handle to the job.
func (n *nodeJob) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.watch != nil {
		n.watch.Close()
		n.watch = nil
	}
	if n.job != nil {
		n.job.Close()
		n.job = nil
	}
}

func run(ctx context.Context, n names, stateDir string, log *slog.Logger, file *agentlog.File, slots *slot.Slots, ready func()) error {
	st, err := agentstore.Open(stateDir)
	if err != nil {
		return err
	}
	defer st.Close()
	job := &nodeJob{name: n.nodeJob, slots: slots.SlotJobs, log: log}
	if err := job.open(); err != nil {
		log.Error("can't keep the node job, so no slot can start until it can", "err", err)
	}
	defer job.close()
	slots.Jobs = job
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	machineOS := agentos.New(ctx, agentos.Config{
		Slots: slots,
		Installs: installs{dir: slots.Installs(), platform: platform(),
			record: filepath.Join(stateDir, winaccount.RecordFile)},
		Survey:          slots.Survey,
		SurveyEvery:     slot.SurveyEvery,
		SmartAppControl: winsac.Read,
		Log:             log,
	})
	// The OS stops with the agent's context, before the node job closes;
	// its runners run on.
	defer machineOS.Wait()
	defer cancel()
	a := agent.New(st, agent.Config{
		Platform:  platform(),
		Machine:   machine(),
		Resources: resources(),
		OS:        machineOS,
		Log:       log,
		FreeDisk:  func() (uint64, error) { return freeDisk(stateDir) },
		LogTail:   file.Tail,
		// The policy each PowerShell's scripts meet, for the
		// diagnostics report.
		ExecutionPolicy: winexecpolicy.Read,
	})
	ln, err := control.ListenPipe(n.pipe)
	if err != nil {
		return err
	}
	server := &control.Server{Agent: a.Control(), Version: version.Version, Log: log}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, ln) }()
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()
	if _, err := a.Control().Status(ctx); err != nil && ctx.Err() == nil {
		cancel()
		<-served
		<-ran
		return fmt.Errorf("the agent didn't answer: %w", err)
	}
	ready()
	log.Info("the agent is running", "version", version.Version)
	// Whichever ends first, the other ends too, so neither outlives the
	// store.
	var first error
	select {
	case first = <-ran:
		cancel()
		<-served
	case first = <-served:
		cancel()
		<-ran
	}
	return first
}

// secureRoot makes the agent's root, if it isn't there, owned by SYSTEM
// with rootSDDL from the start, and checks it with checkRoot: a folder,
// not a link, that no one else made and no one else can change.
func secureRoot(root string) error {
	return makeOrCheck(root, rootSDDL, func(h windows.Handle) error {
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		return checkRoot(sd)
	})
}

// checkRoot accepts the root's descriptor sd only if it's owned by SYSTEM
// or, as an installer run by an administrator may make it,
// Administrators, and its DACL is exactly rootSDDL's: so no one else can
// add, rename or delete what's in it, the state and the log among them.
func checkRoot(sd *windows.SECURITY_DESCRIPTOR) error {
	owner := winacl.WellKnown(windows.WinLocalSystemSid)
	if o, _, err := sd.Owner(); err == nil && o != nil && o.Equals(winacl.WellKnown(windows.WinBuiltinAdministratorsSid)) {
		owner = o
	}
	return winacl.Check(sd, owner, rootEntries)
}

// securePrivate makes dir, if it isn't there, with privateSDDL from the
// start, and checks that it's exactly that: a folder, not a link, owned
// by SYSTEM, whose DACL gives SYSTEM and Administrators full control and
// no one else anything, explicitly, inheriting nothing.
func securePrivate(dir string) error {
	return makeOrCheck(dir, privateSDDL, func(h windows.Handle) error {
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		return winacl.Check(sd, winacl.WellKnown(windows.WinLocalSystemSid), privateEntries)
	})
}

// makeOrCheck makes the folder at path with the descriptor sddl if it
// isn't there, so it's never without it, then opens it as it is, never
// through a link, and checks it with check. It fails if path isn't a
// folder, is a link, or check fails.
func makeOrCheck(path, sddl string, check func(windows.Handle) error) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := windows.CreateDirectory(p, &sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("making %s: %w", path, err)
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	switch {
	case info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return fmt.Errorf("%s is a link, so the agent won't use it", path)
	case info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return fmt.Errorf("%s isn't a folder", path)
	}
	if err := check(h); err != nil {
		return fmt.Errorf("%s isn't as the agent makes it, so it won't use it: %w", path, err)
	}
	return nil
}

// platform is the runner application's platform for this build.
func platform() runnerapp.Platform {
	if runtime.GOARCH == "arm64" {
		return runnerapp.WinARM64
	}
	return runnerapp.WinX64
}

// machine is what an enroll tells the Hub about this machine.
func machine() wire.Machine {
	host, _ := os.Hostname()
	return wire.Machine{
		Hostname:     host,
		OS:           "windows",
		OSVersion:    osVersion(),
		Arch:         runtime.GOARCH,
		AgentVersion: version.Version,
		Zone:         zoneName(),
	}
}

// osVersion is Windows's name and release, as Settings shows them, from
// the registry.
func osVersion() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "Windows"
	}
	defer k.Close()
	product, _, _ := k.GetStringValue("ProductName")
	release, _, err := k.GetStringValue("DisplayVersion")
	if err != nil {
		release, _, _ = k.GetStringValue("ReleaseId")
	}
	b, _, _ := k.GetStringValue("CurrentBuildNumber")
	build, _ := strconv.Atoi(b)
	return windowsVersion(product, release, build)
}

// windowsVersion names Windows from its registry's ProductName,
// DisplayVersion and build: "Windows 11 26H2", or "Windows Server 2025
// 24H2", without the edition. Windows 11 still calls itself Windows 10
// there, so a build from 22000 on is Windows 11.
func windowsVersion(product, release string, build int) string {
	words := strings.Fields(product)
	name := "Windows"
	switch {
	case len(words) >= 3 && words[0] == "Windows" && words[1] == "Server":
		name = strings.Join(words[:3], " ")
	case len(words) >= 2 && words[0] == "Windows":
		name = words[0] + " " + words[1]
		if words[1] == "10" && build >= 22000 {
			name = "Windows 11"
		}
	}
	if release != "" {
		name += " " + release
	}
	return name
}

// zoneName is the OS's time zone by its IANA name, from the registry's
// TimeZoneKeyName through CLDR's windowsZones table, or "" if it names
// none, which the agent enrolls in Central for.
func zoneName() string { return zoneFromKey(timeZoneKeyName) }

// zoneFromKey is the IANA name CLDR's windowsZones table gives the
// Windows zone key read reads, or "" if read fails or the table doesn't
// name it.
func zoneFromKey(read func() (string, error)) string {
	key, err := read()
	if err != nil {
		return ""
	}
	zone, _ := winzone.IANA(key)
	return zone
}

// timeZoneKeyName reads the registry's TimeZoneKeyName, the OS's zone by
// its Windows name.
func timeZoneKeyName() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\TimeZoneInformation`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	key, _, err := k.GetStringValue("TimeZoneKeyName")
	return key, err
}

// memoryStatusEx is MEMORYSTATUSEX, which x/sys doesn't have.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// resources is what the node knows of this machine for its budget: its
// RAM and its logical CPU count. A machine whose RAM can't be read is
// one of unknown size, which the node holds to no budget.
func resources() node.Machine {
	m := node.Machine{CPUs: runtime.NumCPU()}
	s := memoryStatusEx{}
	s.Length = uint32(unsafe.Sizeof(s))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&s))); r != 0 {
		m.RAM = int64(s.TotalPhys)
	}
	return m
}

// freeDisk is the space free to the agent in dir's volume.
func freeDisk(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

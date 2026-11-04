package slot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/meschoening/silkpool/internal/secret"
)

// Slots runs a Linux node's slots: it prepares their slot accounts, and
// starts, stops and watches their runners' units through systemd over
// the system D-Bus (ADRs 0005 to 0007). Each runner runs in a transient
// unit of its own, which systemd supervises, so it outlives the agent.
// Its methods are safe for concurrent use, but not on the same slot.
type Slots struct {
	Layout
	bus     *dbus.Conn
	signals chan *dbus.Signal

	mu sync.Mutex
	// starting holds, by unit name, the channel each Start waits on for
	// its start job's result.
	starting map[string]chan string
	// watching holds, by unit object path, the channels the Waits on its
	// unit are woken through when its properties change.
	watching map[dbus.ObjectPath][]chan struct{}
	// refs holds the units this connection holds a reference to, which
	// keeps systemd from unloading them before their exit is read.
	refs map[string]bool

	// weakened leaves the sandbox's file system settings off every unit,
	// so that a test can show the exposure check finding what they hide.
	// Only tests set it.
	weakened bool
}

const (
	systemd = "org.freedesktop.systemd1"
	manager = systemd + ".Manager"
	service = systemd + ".Service"
)

// ErrGone is Wait's error for a runner whose unit is gone: it ended, and
// systemd unloaded it with its exit code, while nobody was waiting.
var ErrGone = errors.New("the runner's unit is gone")

// Open connects to systemd over the system D-Bus, to run the slots l
// lays out.
func Open(ctx context.Context, l Layout) (*Slots, error) {
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("can't connect to the system D-Bus: %w", err)
	}
	s := &Slots{
		Layout:   l,
		bus:      bus,
		signals:  make(chan *dbus.Signal, 64),
		starting: map[string]chan string{},
		watching: map[dbus.ObjectPath][]chan struct{}{},
		refs:     map[string]bool{},
	}
	bus.Signal(s.signals)
	go s.dispatch()
	err = errors.Join(
		bus.AddMatchSignalContext(ctx, dbus.WithMatchInterface(manager), dbus.WithMatchMember("JobRemoved")),
		s.manager(ctx, "Subscribe").Err,
	)
	if err != nil {
		bus.Close()
		return nil, fmt.Errorf("can't subscribe to systemd's signals: %w", err)
	}
	return s, nil
}

// Close ends the connection. Runners' units run on.
func (s *Slots) Close() error { return s.bus.Close() }

// dispatch hands each signal to whoever waits for it.
func (s *Slots) dispatch() {
	for sig := range s.signals {
		s.mu.Lock()
		switch sig.Name {
		case manager + ".JobRemoved":
			var (
				id           uint32
				job          dbus.ObjectPath
				unit, result string
			)
			if dbus.Store(sig.Body, &id, &job, &unit, &result) == nil {
				if ch := s.starting[unit]; ch != nil {
					ch <- result
					delete(s.starting, unit)
				}
			}
		case "org.freedesktop.DBus.Properties.PropertiesChanged":
			for _, ch := range s.watching[sig.Path] {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
		s.mu.Unlock()
	}
}

// Start starts runner's listener in slot n, from the runner root the
// caller built, with its JIT config.
//
//  1. It records runner in the slot's folder, so that the slot's reset
//     knows whose logs to copy, even after the agent restarts.
//  2. It writes config to a file only the slot account can read, in the
//     runner root, which must be the account's own, mode 0700.
//  3. It starts the runner's transient unit, silkpool-slotN-<runner>.service,
//     which runs the launcher as the slot account. The launcher moves the
//     config into the listener's environment, so it's never on a command
//     line or in the unit's settings, which other accounts can read.
//  4. The listener's output goes to its file in the agent's logs
//     (ListenerLog), which only root can read.
//
// The unit carries the slot's sandbox, recomputed now, and its runner
// outlives the agent. Wait reports how it ends, Stop and Kill end it,
// and Alive finds it again after the agent restarts. Every started
// runner must be waited for: until then, systemd keeps its unit for its
// exit code. Start fails if the unit doesn't start, and then deletes the
// config file.
func (s *Slots) Start(ctx context.Context, n int, runner string, config secret.Value) error {
	if err := checkRunner(runner); err != nil {
		return err
	}
	a, err := user.Lookup(s.Account(n))
	if err != nil {
		return fmt.Errorf("can't find slot %d's account: %w", n, err)
	}
	uid, _ := strconv.Atoi(a.Uid)
	gid, _ := strconv.Atoi(a.Gid)
	root := s.RunnerRoot(n)
	if err := ownDir(root, uid, 0o700); err != nil {
		return fmt.Errorf("slot %d's runner root isn't ready: %w", n, err)
	}
	if err := writeRecord(filepath.Join(s.Folder(n), runnerFile), runner); err != nil {
		return fmt.Errorf("can't record runner %s in slot %d: %w", runner, n, err)
	}
	path := root + "/" + configFile
	if err := writeConfig(path, config, uid, gid); err != nil {
		return fmt.Errorf("can't write the JIT config of runner %s: %w", runner, err)
	}
	if err := s.start(ctx, n, runner); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func (s *Slots) start(ctx context.Context, n int, runner string) error {
	if err := os.MkdirAll(s.RunnerLogs(runner), 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(s.ListenerLog(runner), os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("can't open the listener's log: %w", err)
	}
	defer log.Close()
	v, err := s.look()
	if err != nil {
		return fmt.Errorf("can't work out what slot %d may see: %w", n, err)
	}
	props := slices.Concat(s.properties(n, runner, dbus.UnixFD(log.Fd())),
		s.sandbox(n, s.hidden(n, v), s.Home(n), s.RunnerRoot(n)))
	return s.startUnit(ctx, s.unit(n, runner), props, "runner "+runner+"'s unit")
}

// startUnit starts the transient unit with props, which must keep it
// loaded with AddRef until waitUnit reads its exit, and waits for its
// start job. what names the unit in errors.
func (s *Slots) startUnit(ctx context.Context, unit string, props []property, what string) error {
	done := make(chan string, 1)
	s.mu.Lock()
	s.starting[unit] = done
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.starting, unit)
		s.mu.Unlock()
	}()
	var job dbus.ObjectPath
	if err := s.manager(ctx, "StartTransientUnit", unit, "fail", props, []auxUnit{}).Store(&job); err != nil {
		return fmt.Errorf("systemd didn't start %s: %w", what, err)
	}
	s.mu.Lock()
	s.refs[unit] = true
	s.mu.Unlock()
	select {
	case result := <-done:
		if result != "done" {
			s.unref(context.WithoutCancel(ctx), unit)
			return fmt.Errorf("%s didn't start: its start job ended %s", what, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A property is a unit setting, as StartTransientUnit takes it.
type property struct {
	Name  string
	Value dbus.Variant
}

// An auxUnit is one of the units StartTransientUnit starts beside the
// main one, which Start never asks for.
type auxUnit struct {
	Name  string
	Props []property
}

// An execCommand is ExecStart's command, as StartTransientUnit takes it.
type execCommand struct {
	Path          string
	Args          []string
	IgnoreFailure bool
}

// properties are the settings of runner's unit in slot n, besides its
// sandbox, which writes its output to log. They're the decided ones (ADRs
// 0006 and 0014): a stop that SIGINTs the listener and kills what's left
// after 30 s, and a fixed environment. AddRef keeps the unit loaded for
// this connection until Wait reads its exit, and CollectMode unloads it
// then, however it ended.
func (s *Slots) properties(n int, runner string, log dbus.UnixFD) []property {
	root := s.RunnerRoot(n)
	return []property{
		{"Description", dbus.MakeVariant(fmt.Sprintf("Silkpool runner %s in slot %d", runner, n))},
		{"Type", dbus.MakeVariant("exec")},
		{"WorkingDirectory", dbus.MakeVariant(root)},
		{"ExecStart", dbus.MakeVariant([]execCommand{{s.Launcher, []string{s.Launcher, LaunchMode, root}, false}})},
		{"Environment", dbus.MakeVariant([]string{"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1"})},
		{"StandardOutputFileDescriptor", dbus.MakeVariant(log)},
		{"StandardErrorFileDescriptor", dbus.MakeVariant(log)},
		{"OOMPolicy", dbus.MakeVariant("continue")},
		{"KillSignal", dbus.MakeVariant(int32(syscall.SIGINT))},
		{"KillMode", dbus.MakeVariant("mixed")},
		{"TimeoutStopUSec", dbus.MakeVariant(uint64(StopTimeout / time.Microsecond))},
		{"AddRef", dbus.MakeVariant(true)},
		{"CollectMode", dbus.MakeVariant("inactive-or-failed")},
	}
}

// sandbox is the sandbox every unit of slot n carries, its runners' and
// its exposure check's, which hides hidden and lets it write writable
// (ADRs 0007 and 0014): the slot account as User=, the slots' slice, a
// read-only system, no homes, and the decided protections. A writable
// path that starts with "-" may be missing.
func (s *Slots) sandbox(n int, hidden []string, writable ...string) []property {
	inaccessible := make([]string, len(hidden))
	for i, p := range hidden {
		// "-": a path that's gone needs no hiding.
		inaccessible[i] = "-" + p
	}
	props := []property{
		{"User", dbus.MakeVariant(s.Account(n))},
		{"Slice", dbus.MakeVariant(s.slice())},
		{"ReadWritePaths", dbus.MakeVariant(writable)},
	}
	if !s.weakened {
		props = append(props,
			property{"ProtectSystem", dbus.MakeVariant("strict")},
			property{"ProtectHome", dbus.MakeVariant("yes")},
			property{"InaccessiblePaths", dbus.MakeVariant(inaccessible)},
		)
	}
	for _, name := range []string{
		"PrivateTmp", "NoNewPrivileges", "PrivateDevices", "PrivateIPC", "RemoveIPC",
		"ProtectKernelTunables", "ProtectKernelModules", "ProtectKernelLogs", "ProtectControlGroups",
		"ProtectClock", "ProtectHostname", "RestrictSUIDSGID", "LockPersonality",
	} {
		props = append(props, property{name, dbus.MakeVariant(true)})
	}
	return props
}

// StopTimeout is how long a graceful stop gives the listener after
// SIGINT before systemd kills every process of the unit.
const StopTimeout = 30 * time.Second

// Stop stops runner's listener in slot n gracefully, as `systemctl stop`
// does: SIGINT to the listener alone, and SIGKILL to every process of
// the unit if it hasn't ended StopTimeout later. It returns once systemd
// has the stop in hand; Wait reports the end.
func (s *Slots) Stop(ctx context.Context, n int, runner string) error {
	if err := checkRunner(runner); err != nil {
		return err
	}
	var job dbus.ObjectPath
	err := s.manager(ctx, "StopUnit", s.unit(n, runner), "replace").Store(&job)
	if gone(err) {
		return nil
	}
	return err
}

// Kill kills every process of runner's unit in slot n with SIGKILL.
func (s *Slots) Kill(ctx context.Context, n int, runner string) error {
	if err := checkRunner(runner); err != nil {
		return err
	}
	err := s.manager(ctx, "KillUnit", s.unit(n, runner), "all", int32(syscall.SIGKILL)).Err
	if gone(err) {
		return nil
	}
	return err
}

// An Exit is how a unit's main process ended.
type Exit struct {
	// Code is its exit code, or 128 plus the signal that ended it, as a
	// shell counts it: 137 for SIGKILL.
	Code int
	// OutOfMemory reports that the kernel's OOM killer ended a process
	// of the unit while it ran, as systemd counts from the unit's
	// memory.events. systemd keeps the count after the unit's cgroup is
	// gone, while the unit stays loaded, so a kill just before the unit
	// ended, the listener's own included, still counts.
	OutOfMemory bool
}

// Wait waits for runner's listener in slot n to end, and returns how it
// ended. It's ErrGone if the unit ended and was unloaded while nobody
// waited, such as while the agent was down. Wait at most once at a time
// for a runner.
func (s *Slots) Wait(ctx context.Context, n int, runner string) (Exit, error) {
	if err := checkRunner(runner); err != nil {
		return Exit{}, err
	}
	return s.waitUnit(ctx, s.unit(n, runner))
}

// waitUnit waits for unit's main process to end, and returns how it
// ended as Wait does, or ErrGone. It reads the unit's OOM kills before
// it drops its reference, while the unit is surely loaded.
func (s *Slots) waitUnit(ctx context.Context, unit string) (Exit, error) {
	var path dbus.ObjectPath
	if err := s.manager(ctx, "GetUnit", unit).Store(&path); err != nil {
		if gone(err) {
			return Exit{}, ErrGone
		}
		return Exit{}, err
	}
	changed := make(chan struct{}, 1)
	s.mu.Lock()
	s.watching[path] = append(s.watching[path], changed)
	held := s.refs[unit]
	s.refs[unit] = true
	s.mu.Unlock()
	match := []dbus.MatchOption{
		dbus.WithMatchObjectPath(path),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
	}
	defer func() {
		s.bus.RemoveMatchSignalContext(context.WithoutCancel(ctx), match...)
		s.mu.Lock()
		s.watching[path] = slices.DeleteFunc(s.watching[path], func(c chan struct{}) bool { return c == changed })
		if len(s.watching[path]) == 0 {
			delete(s.watching, path)
		}
		s.mu.Unlock()
		s.unref(context.WithoutCancel(ctx), unit)
	}()
	if err := s.bus.AddMatchSignalContext(ctx, match...); err != nil {
		return Exit{}, err
	}
	if !held {
		if err := s.bus.Object(systemd, path).CallWithContext(ctx, systemd+".Unit.Ref", 0).Err; err != nil {
			if gone(err) {
				return Exit{}, ErrGone
			}
			return Exit{}, err
		}
	}
	// A signal wakes it as soon as the unit changes. The tick is in case
	// one is missed.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		var status struct{ code, value int32 }
		var load string
		props := s.bus.Object(systemd, path)
		err := errors.Join(
			getProperty(ctx, props, systemd+".Unit", "LoadState", &load),
			storeProperty(ctx, props, "ExecMainCode", &status.code),
			storeProperty(ctx, props, "ExecMainStatus", &status.value),
		)
		switch {
		case gone(err):
			return Exit{}, ErrGone
		case err == nil && load != "loaded":
			// The unit ended and was unloaded after GetUnit found it, and
			// asking for its path loaded a stub in its place, which never
			// ends: it's gone.
			return Exit{}, ErrGone
		case err != nil:
			return Exit{}, err
		case status.code == 1: // CLD_EXITED
			return Exit{Code: int(status.value), OutOfMemory: s.oomKilled(ctx, props)}, nil
		case status.code == 2 || status.code == 3: // CLD_KILLED, CLD_DUMPED
			return Exit{Code: 128 + int(status.value), OutOfMemory: s.oomKilled(ctx, props)}, nil
		}
		select {
		case <-changed:
		case <-tick.C:
		case <-ctx.Done():
			return Exit{}, ctx.Err()
		}
	}
}

// oomKilled reports whether the OOM killer ended a process of the unit
// obj is, from systemd's count of them, its OOMKills property, which it
// keeps from the unit's memory.events. A systemd without the property
// says nothing.
func (s *Slots) oomKilled(ctx context.Context, obj dbus.BusObject) bool {
	var kills uint64
	return storeProperty(ctx, obj, "OOMKills", &kills) == nil && kills > 0
}

// unref drops this connection's reference to unit, if it holds one, so
// that systemd unloads it once it's ended.
func (s *Slots) unref(ctx context.Context, unit string) {
	s.mu.Lock()
	held := s.refs[unit]
	delete(s.refs, unit)
	s.mu.Unlock()
	if !held {
		return
	}
	var path dbus.ObjectPath
	if s.manager(ctx, "GetUnit", unit).Store(&path) == nil {
		s.bus.Object(systemd, path).CallWithContext(ctx, systemd+".Unit.Unref", 0)
	}
}

// storeProperty stores the service property name of obj in v.
func storeProperty(ctx context.Context, obj dbus.BusObject, name string, v any) error {
	return getProperty(ctx, obj, service, name, v)
}

// getProperty stores the property name of obj's interface iface in v.
func getProperty(ctx context.Context, obj dbus.BusObject, iface, name string, v any) error {
	var variant dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, name).Store(&variant); err != nil {
		return err
	}
	return variant.Store(v)
}

// Alive returns the runners whose listeners are still running in their
// units, in any slot, sorted: what a restarted agent tells the node as
// Started's Alive.
func (s *Slots) Alive(ctx context.Context) ([]string, error) {
	var units []struct {
		Name, Description, Load, Active, Sub, Following string
		Path                                            dbus.ObjectPath
		JobID                                           uint32
		JobType                                         string
		JobPath                                         dbus.ObjectPath
	}
	err := s.manager(ctx, "ListUnitsByPatterns", []string{}, []string{s.Name + "-slot*-*.service"}).Store(&units)
	if err != nil {
		return nil, fmt.Errorf("can't list the runners' units: %w", err)
	}
	var alive []string
	for _, u := range units {
		_, runner, ok := s.parseUnit(u.Name)
		if !ok {
			continue
		}
		var pid uint32
		err := storeProperty(ctx, s.bus.Object(systemd, u.Path), "MainPID", &pid)
		switch {
		case gone(err):
		case err != nil:
			return nil, err
		case pid != 0:
			alive = append(alive, runner)
		}
	}
	slices.Sort(alive)
	return alive, nil
}

// unit is the name of runner's unit in slot n.
func (l Layout) unit(n int, runner string) string {
	return fmt.Sprintf("%s-slot%d-%s.service", l.Name, n, runner)
}

// parseUnit returns the slot and runner a unit's name names.
func (l Layout) parseUnit(unit string) (n int, runner string, ok bool) {
	rest, ok := strings.CutPrefix(unit, l.Name+"-slot")
	if !ok {
		return 0, "", false
	}
	rest, ok = strings.CutSuffix(rest, ".service")
	if !ok {
		return 0, "", false
	}
	slot, runner, ok := strings.Cut(rest, "-")
	n, err := strconv.Atoi(slot)
	if !ok || err != nil || n < 1 || checkRunner(runner) != nil {
		return 0, "", false
	}
	return n, runner, true
}

// runnerName is what a runner's name may be: ASCII letters, digits,
// hyphens, underscores and dots, safe in a unit's name and a path.
var runnerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func checkRunner(runner string) error {
	if !runnerName.MatchString(runner) {
		return fmt.Errorf("%q isn't a runner name", runner)
	}
	return nil
}

// manager calls method on systemd's manager.
func (s *Slots) manager(ctx context.Context, method string, args ...any) *dbus.Call {
	return s.bus.Object(systemd, "/org/freedesktop/systemd1").CallWithContext(ctx, manager+"."+method, 0, args...)
}

// gone reports whether err is systemd saying the unit, or its process,
// isn't there.
func gone(err error) bool {
	var e dbus.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Name {
	case systemd + ".NoSuchUnit", systemd + ".NoSuchProcess", systemd + ".UnitInactive",
		"org.freedesktop.DBus.Error.UnknownObject", "org.freedesktop.DBus.Error.UnknownMethod":
		return true
	}
	return false
}

// ownDir checks that path is a directory, not a link, that uid owns,
// with mode perm.
func ownDir(path string, uid int, perm os.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, _ := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || st == nil || int(st.Uid) != uid || fi.Mode().Perm() != perm {
		return fmt.Errorf("%s isn't a directory of its own with mode %o", path, perm)
	}
	return nil
}

// runnerFile is the file in a slot's folder that names the runner Start
// last started there, until the slot's reset has copied its logs.
const runnerFile = "runner-name"

// recordedRunner returns the runner slot n's record names, or "" if
// there's none.
func (s *Slots) recordedRunner(n int) string {
	b, err := os.ReadFile(filepath.Join(s.Folder(n), runnerFile))
	if runner := strings.TrimSpace(string(b)); err == nil && checkRunner(runner) == nil {
		return runner
	}
	return ""
}

// writeRecord writes value to a record file at path, readable by every
// account, through a new file renamed into place, so that a crash leaves
// the old record or the new one.
func writeRecord(path, value string) error {
	tmp := path + ".new"
	err := os.WriteFile(tmp, []byte(value+"\n"), 0o644)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// writeConfig writes config to a new file at path that only uid and gid
// may read. It never follows a link there, and replaces a file an
// earlier attempt left.
func writeConfig(path string, config secret.Value, uid, gid int) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(config.Reveal())
	err = errors.Join(err, f.Chown(uid, gid), f.Close())
	if err != nil {
		os.Remove(path)
	}
	return err
}

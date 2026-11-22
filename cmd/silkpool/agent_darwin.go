package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/agent"
	"github.com/meschoening/silkpool/internal/agentlog"
	"github.com/meschoening/silkpool/internal/agentos"
	"github.com/meschoening/silkpool/internal/agentstore"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/macfda"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/runnerinstall"
	"github.com/meschoening/silkpool/internal/slot"
	"github.com/meschoening/silkpool/internal/version"
	"github.com/meschoening/silkpool/internal/wire"
)

func init() {
	modes["agent"] = runAgent
	// The package loads the agent's LaunchDaemon, as
	// io.github.meschoening.silkpool.agent, from its plist.
	startCommand = "sudo launchctl bootstrap system /Library/LaunchDaemons/io.github.meschoening.silkpool.agent.plist"
}

// folders are where the agent keeps its own files on macOS, each root's
// alone: its state, which holds the node key, its log folder, and its
// control socket. Slots lays out the slots, the service account they run
// as and the runner application's installs, whose state and logs are the
// agent's own.
type folders struct {
	State, Logs, Socket string
	Slots               slot.Layout
}

// installed are the agent's folders as the package installs it (ADR
// 0008, and "Define how each binary is packaged, installed, upgraded and
// signed per OS").
var installed = folders{
	State:  "/var/db/silkpool/state",
	Logs:   "/Library/Logs/Silkpool",
	Socket: control.Path,
	Slots:  slot.Installed,
}

// fullDiskAccess reports whether the agent has Full Disk Access
// (macfda.Read). Only the tests that run the agent as a LaunchDaemon of
// their own replace it, since their program can't be given it.
var fullDiskAccess = macfda.Read

// runAgent runs the agent service in the foreground, as its LaunchDaemon
// starts it, with RunAtLoad and KeepAlive. It idles while the machine
// isn't enrolled.
func runAgent(ctx context.Context, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: silkpool agent")
		return 2
	}
	if err := serve(ctx, installed); err != nil {
		fmt.Fprintln(os.Stderr, "silkpool agent:", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, f folders) error {
	if os.Geteuid() != 0 {
		return errors.New("the agent runs as root, as its LaunchDaemon starts it")
	}
	if err := f.prepare(0); err != nil {
		return err
	}
	file, err := agentlog.Open(f.Logs)
	if err != nil {
		return err
	}
	defer file.Close()
	log := slog.New(agentlog.Handler(file))
	st, err := agentstore.Open(f.State)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots, err := slot.Open(ctx, f.Slots)
	if err != nil {
		return err
	}
	defer slots.Close()
	machineOS := agentos.New(ctx, agentos.Config{
		Slots:       slots,
		Installs:    runnerinstall.Installs{Dir: f.Slots.Installs(), Platform: runnerapp.OSXARM64},
		Survey:      slots.Resurvey,
		SurveyEvery: slot.SurveyEvery,
		Log:         log,
		// Without Full Disk Access the clean can't finish, so each slot
		// asks first (#306).
		FullDiskAccess: fullDiskAccess,
	})
	// The OS stops before the slots close; its runners run on.
	defer machineOS.Wait()
	defer cancel()
	a := agent.New(st, agent.Config{
		Platform:  runnerapp.OSXARM64,
		Machine:   machine(),
		Resources: resources(),
		OS:        machineOS,
		Log:       log,
		FreeDisk:  func() (uint64, error) { return freeDisk(f.State) },
		LogTail:   file.Tail,
	})
	ln, err := control.Listen(f.Socket)
	if err != nil {
		return err
	}
	server := &control.Server{Agent: a.Control(), Version: version.Version, Log: log}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, ln) }()
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()
	log.Info("the agent is running", "version", version.Version)
	return await(ran, served, cancel)
}

// prepare makes f's folders, each the own of the account uid, which is
// root's 0 outside tests: Silkpool's folder 0755, for the slots' folders
// it will hold beside the state, the state 0700, the log folder 0700,
// and the socket's folder 0755, so only root can make files in it.
func (f folders) prepare(uid int) error {
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Dir(f.State), 0o755},
		{f.State, 0o700},
		{f.Logs, 0o700},
		{filepath.Dir(f.Socket), 0o755},
	} {
		if err := ownFolder(d.path, d.mode, uid); err != nil {
			return err
		}
	}
	return nil
}

// ownFolder makes dir with mode if it's missing. Otherwise it must be a
// folder, not a link or a file, that uid owns, and it gets mode back.
// The mode is set through the folder it checked, so a link swapped in
// meanwhile can't redirect it.
func ownFolder(dir string, mode os.FileMode, uid int) error {
	if err := os.Mkdir(dir, mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	d, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("%s must be a folder of root's own: %w", dir, err)
	}
	defer d.Close()
	info, err := d.Stat()
	if err != nil {
		return err
	}
	if owner := info.Sys().(*syscall.Stat_t).Uid; int(owner) != uid {
		return fmt.Errorf("%s must be a folder of root's own, but uid %d owns it", dir, owner)
	}
	if err := d.Chmod(mode); err != nil {
		return fmt.Errorf("setting %s's mode: %w", dir, err)
	}
	return nil
}

// machine is what an enroll tells the Hub about this Mac.
func machine() wire.Machine {
	host, _ := os.Hostname()
	return wire.Machine{
		Hostname:     host,
		OS:           "darwin",
		OSVersion:    osVersion(),
		Arch:         runtime.GOARCH,
		AgentVersion: version.Version,
		Zone:         zoneName(),
	}
}

// osVersion is macOS's version as sw_vers gives it, such as
// "macOS 26.5.1".
func osVersion() string {
	out, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if v := strings.TrimSpace(string(out)); err == nil && v != "" {
		return "macOS " + v
	}
	return "macOS"
}

// resources is what the node knows of this Mac: its RAM, as hw.memsize
// counts it, its logical CPU count, and that it's a Mac, which has no
// budget.
func resources() node.Machine {
	m := node.Machine{CPUs: runtime.NumCPU(), MacOS: true}
	if ram, err := unix.SysctlUint64("hw.memsize"); err == nil {
		m.RAM = int64(ram)
	}
	return m
}

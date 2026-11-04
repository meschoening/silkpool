package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/agent"
	"github.com/meschoening/silkpool/internal/agentlog"
	"github.com/meschoening/silkpool/internal/agentos"
	"github.com/meschoening/silkpool/internal/agentstore"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/runnerinstall"
	"github.com/meschoening/silkpool/internal/slot"
	"github.com/meschoening/silkpool/internal/version"
	"github.com/meschoening/silkpool/internal/wire"
)

func init() {
	modes["agent"] = runAgent
	helper = slot.Helper
	startCommand = "sudo systemctl start silkpool"
}

// stateDir is where the agent keeps its state, root's alone (ADR 0008),
// and installsDir the runner application's installs, root's alone too,
// on the same filesystem as the slots' folders, so that runner roots can
// be linked to them.
const (
	stateDir    = "/var/lib/silkpool/state"
	installsDir = "/var/lib/silkpool/runner"
)

// runAgent runs the agent service in the foreground, as its systemd
// unit does, and tells systemd it's ready once the control socket
// listens. It idles while the machine isn't enrolled. Its log folder is
// the caller's: --log-dir, or the unit's LogsDirectory=.
func runAgent(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	logDir := fs.String("log-dir", os.Getenv("LOGS_DIRECTORY"), "the folder of the agent's own log")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *logDir == "" {
		fmt.Fprintln(os.Stderr, "usage: silkpool agent --log-dir <folder>")
		return 2
	}
	if err := serve(ctx, *logDir); err != nil {
		fmt.Fprintln(os.Stderr, "silkpool agent:", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, logDir string) error {
	if os.Geteuid() != 0 {
		return errors.New("the agent runs as root, as its systemd unit starts it")
	}
	file, err := agentlog.Open(logDir)
	if err != nil {
		return err
	}
	defer file.Close()
	log := slog.New(agentlog.Handler(file))
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return err
	}
	st, err := agentstore.Open(stateDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots, err := slot.Open(ctx, slot.Installed)
	if err != nil {
		return err
	}
	defer slots.Close()
	machineOS := agentos.New(ctx, agentos.Config{
		Slots:    slots,
		Installs: runnerinstall.Installs{Dir: installsDir, Platform: platform()},
		Log:      log,
	})
	// The OS stops before the slots' connection closes; its runners run on.
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
	})
	ln, err := control.Listen(control.Path)
	if err != nil {
		return err
	}
	server := &control.Server{Agent: a.Control(), Version: version.Version, Log: log}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, ln) }()
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()
	if _, err := a.Control().Status(ctx); err == nil {
		if err := notify("READY=1"); err != nil {
			log.Warn("couldn't tell systemd the agent is ready", "err", err)
		}
	}
	log.Info("the agent is running", "version", version.Version)
	return await(ran, served, cancel)
}

// await waits for the agent or its control server to end, then ends the
// other and waits for it too, so neither outlives the store. It returns
// the first one's error.
func await(ran, served <-chan error, cancel func()) error {
	var err error
	select {
	case err = <-ran:
		cancel()
		<-served
	case err = <-served:
		cancel()
		<-ran
	}
	return err
}

// platform is the runner application's platform for this build.
func platform() runnerapp.Platform {
	if runtime.GOARCH == "arm64" {
		return runnerapp.LinuxARM64
	}
	return runnerapp.LinuxX64
}

// machine is what an enroll tells the Hub about this machine.
func machine() wire.Machine {
	host, _ := os.Hostname()
	return wire.Machine{
		Hostname:     host,
		OS:           "linux",
		OSVersion:    osVersion(),
		Arch:         runtime.GOARCH,
		AgentVersion: version.Version,
		Zone:         zoneName(),
	}
}

// resources is what the node knows of this machine for its budget: its
// RAM, as the kernel counts it for sysinfo(2) and /proc/meminfo, and
// its logical CPU count. A machine whose RAM can't be read is one of
// unknown size, which the node holds to no budget.
func resources() node.Machine {
	m := node.Machine{CPUs: runtime.NumCPU()}
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err == nil {
		m.RAM = int64(info.Totalram) * int64(info.Unit)
	}
	return m
}

// osVersion is the distribution's pretty name, from os-release.
func osVersion() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	defer f.Close()
	return prettyName(f)
}

// prettyName reads PRETTY_NAME from an os-release file.
func prettyName(r io.Reader) string {
	s := bufio.NewScanner(r)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return "Linux"
}

// zoneName is the OS's time zone by its IANA name: TZ if it names one,
// else what /etc/localtime links to, else /etc/timezone.
func zoneName() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" && !filepath.IsAbs(tz) {
		return tz
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(link, "zoneinfo/"); ok {
			return name
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(b)); name != "" {
			return name
		}
	}
	if _, err := time.LoadLocation("UTC"); err == nil {
		return "UTC"
	}
	return ""
}

// freeDisk is the space free to root in dir's file system.
func freeDisk(dir string) (uint64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(dir, &s); err != nil {
		return 0, err
	}
	return s.Bfree * uint64(s.Bsize), nil
}

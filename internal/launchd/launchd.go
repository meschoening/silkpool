// Package launchd writes the Mac's launchd jobs and reads what launchctl
// says of them, without cgo: a slot's runner runs in a launchd daemon job
// of its own (ADR 0006), which launchd supervises, so it outlives the
// agent.
//
// The package runs anywhere a Launchctl can. On the Mac, Local runs
// /bin/launchctl; tests give it one of their own.
package launchd

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// A Job is a launchd job's settings, as its plist holds them.
type Job struct {
	Label string
	// Program is the program the job runs, with Args after it.
	Program string
	Args    []string
	// UserName is the account a system job runs as, which also sets its
	// HOME, USER, LOGNAME and SHELL from the account's record. launchd
	// ignores it in a user's domain.
	UserName string
	// SessionCreate gives the job a security session of its own, which
	// the keychain needs.
	SessionCreate bool
	// SessionType is the job's LimitLoadToSessionType, or empty for its
	// domain's default. A job loaded in a user's domain from an SSH
	// session needs Background.
	SessionType string
	// RunAtLoad starts the job as it's loaded. A job loaded with
	// launchctl bootstrap from outside the folders launchd loads at boot
	// is gone after a reboot, so it never starts at boot.
	RunAtLoad        bool
	WorkingDirectory string
	// Env is the job's environment, beyond what launchd sets. It goes in
	// the plist, which other accounts can read through launchctl print:
	// nothing secret may be in it.
	Env map[string]string
	// Stdout and Stderr are the files the job's output goes to.
	Stdout, Stderr string
}

// Plist renders the job as an XML property list.
func (j Job) Plist() []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" +
		`<plist version="1.0">` + "\n<dict>\n")
	str := func(key, v string) {
		if v != "" {
			fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", esc(key), esc(v))
		}
	}
	boolean := func(key string, v bool) {
		if v {
			fmt.Fprintf(&b, "\t<key>%s</key>\n\t<true/>\n", esc(key))
		}
	}
	str("Label", j.Label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range append([]string{j.Program}, j.Args...) {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", esc(a))
	}
	b.WriteString("\t</array>\n")
	str("UserName", j.UserName)
	boolean("SessionCreate", j.SessionCreate)
	str("LimitLoadToSessionType", j.SessionType)
	boolean("RunAtLoad", j.RunAtLoad)
	str("WorkingDirectory", j.WorkingDirectory)
	if len(j.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range slices.Sorted(maps.Keys(j.Env)) {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", esc(k), esc(j.Env[k]))
		}
		b.WriteString("\t</dict>\n")
	}
	str("StandardOutPath", j.Stdout)
	str("StandardErrorPath", j.Stderr)
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// A Status is what launchctl print says of a job.
type Status struct {
	// Loaded reports that the domain holds a job of the label. Nothing
	// else is set without it.
	Loaded bool
	// State is what launchd says the job is doing: "running", "not
	// running", or another word while it's starting.
	State string
	// PID is its process's, while it runs.
	PID int
	// Plist is the property list launchd loaded it from, as it names it.
	Plist string
	// Exited reports that the job's process ended, and Code how: its exit
	// code, or 128 plus the signal that ended it, as a shell counts it.
	Exited bool
	Code   int
}

// Running reports whether the job's process is running.
func (s Status) Running() bool { return s.Loaded && s.State == "running" && s.PID > 0 }

// NotLoaded is the code launchctl print exits with for a job the domain
// doesn't hold.
const NotLoaded = 113

// ParsePrint reads what launchctl print printed of a job, and the code
// it exited with: a job the domain doesn't hold, or the job's own
// settings, each a line indented by one tab. A print that failed
// otherwise, or that names its plist or its state more than once, or
// not at all, is an error.
func ParsePrint(out string, code int) (Status, error) {
	switch code {
	case NotLoaded:
		return Status{}, nil
	case 0:
	default:
		return Status{}, fmt.Errorf("launchctl print exited %d: %s", code, strings.TrimSpace(out))
	}
	s := Status{Loaded: true}
	seen := map[string]int{}
	var exit, signal string
	for line := range strings.Lines(out) {
		line = strings.TrimSuffix(line, "\n")
		rest, ok := strings.CutPrefix(line, "\t")
		if !ok || strings.HasPrefix(rest, "\t") {
			continue
		}
		key, value, ok := strings.Cut(rest, " = ")
		if !ok {
			continue
		}
		seen[key]++
		switch key {
		case "state":
			s.State = value
		case "pid":
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return Status{}, fmt.Errorf("launchctl print says the job's pid is %q", value)
			}
			s.PID = pid
		case "path":
			s.Plist = value
		case "last exit code":
			exit = value
		case "last terminating signal":
			signal = value
		}
	}
	for _, key := range []string{"state", "path"} {
		if seen[key] != 1 {
			return Status{}, fmt.Errorf("launchctl print names the job's %s %d times, not once", key, seen[key])
		}
	}
	for _, key := range []string{"pid", "last exit code", "last terminating signal"} {
		if seen[key] > 1 {
			return Status{}, fmt.Errorf("launchctl print names the job's %s %d times", key, seen[key])
		}
	}
	switch {
	case signal != "":
		// "Interrupt: 2", "Killed: 9"
		i := strings.LastIndex(signal, ": ")
		n, err := strconv.Atoi(signal[i+2:])
		if i < 0 || err != nil || n <= 0 || n >= 128 {
			return Status{}, fmt.Errorf("launchctl print says the job's last terminating signal was %q", signal)
		}
		s.Exited, s.Code = true, 128+n
	case exit != "" && exit != "(never exited)":
		// "1", or annotated, as "78: EX_CONFIG"
		code, _, _ := strings.Cut(exit, ": ")
		n, err := strconv.Atoi(code)
		if err != nil || n < 0 || n > 255 {
			return Status{}, fmt.Errorf("launchctl print says the job's last exit code was %q", exit)
		}
		s.Exited, s.Code = true, n
	}
	return s, nil
}

// A Launchctl runs launchctl.
type Launchctl struct {
	// Run runs launchctl with args, and returns what it printed, its
	// standard output and error together, and its exit code. Its error
	// is only for a launchctl that couldn't run.
	Run func(ctx context.Context, args ...string) (out string, code int, err error)
}

// Local runs /bin/launchctl.
var Local = Launchctl{Run: launchctl}

func launchctl(ctx context.Context, args ...string) (string, int, error) {
	out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return string(out), 0, nil
}

// Print returns what launchctl print says of the job in domain, such as
// "system", labelled label.
func (l Launchctl) Print(ctx context.Context, domain, label string) (Status, error) {
	out, code, err := l.Run(ctx, "print", domain+"/"+label)
	if err != nil {
		return Status{}, err
	}
	return ParsePrint(out, code)
}

// Bootstrap loads the job whose plist is at path into domain.
func (l Launchctl) Bootstrap(ctx context.Context, domain, path string) error {
	return l.do(ctx, "bootstrap", domain, path)
}

// Bootout unloads the job labelled label from domain, ending it if it
// runs.
func (l Launchctl) Bootout(ctx context.Context, domain, label string) error {
	return l.do(ctx, "bootout", domain+"/"+label)
}

// noDomain is how launchctl starts its answer about a domain launchd
// doesn't hold, as its own message has it: "Could not find domain for %s".
const noDomain = "Could not find domain for"

// Domain reports whether launchd holds domain, such as user/450, as
// launchctl print says: one it holds is printed as "user/450 = {" and
// what's in it, and one it doesn't is noDomain. Anything else is an
// error.
func (l Launchctl) Domain(ctx context.Context, domain string) (bool, error) {
	out, code, err := l.Run(ctx, "print", domain)
	if err != nil {
		return false, err
	}
	return parseDomain(domain, out, code)
}

func parseDomain(domain, out string, code int) (bool, error) {
	switch {
	case code == 0 && strings.HasPrefix(out, domain+" = {\n"):
		return true, nil
	case code != 0 && strings.HasPrefix(out, noDomain+" "):
		return false, nil
	}
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return false, fmt.Errorf("launchctl print %s exited %d: %s", domain, code, strings.TrimSpace(out))
}

// tearingDown is what launchctl says when it's asked to boot out a domain
// launchd is already tearing down: "Boot-out failed: 124: Domain is
// tearing down".
const tearingDown = "Domain is tearing down"

// ErrTearingDown is BootoutDomain's error for a domain launchd is still
// tearing down from an earlier bootout: it's going, but not gone yet.
var ErrTearingDown = errors.New("launchd is still tearing the domain down")

// BootoutDomain tears domain down, such as user/450: launchd ends every
// job in it and their processes. It returns once launchd has begun, which
// may be before the domain is gone: Domain says when it is. A domain
// launchd doesn't hold, by the time it's asked, is down already, and one
// it's still tearing down from an earlier bootout is ErrTearingDown.
func (l Launchctl) BootoutDomain(ctx context.Context, domain string) error {
	out, code, err := l.Run(ctx, "bootout", domain)
	if err != nil || code == 0 || strings.HasPrefix(out, noDomain+" ") {
		return err
	}
	err = fmt.Errorf("launchctl bootout %s exited %d: %s", domain, code, strings.TrimSpace(out))
	if strings.Contains(out, tearingDown) {
		err = fmt.Errorf("%w: %w", ErrTearingDown, err)
	}
	return err
}

// Kill sends the signal, such as "SIGINT", to the job's process.
func (l Launchctl) Kill(ctx context.Context, signal, domain, label string) error {
	return l.do(ctx, "kill", signal, domain+"/"+label)
}

func (l Launchctl) do(ctx context.Context, args ...string) error {
	out, code, err := l.Run(ctx, args...)
	if err == nil && code != 0 {
		err = fmt.Errorf("launchctl %s exited %d: %s", strings.Join(args, " "), code, strings.TrimSpace(out))
	}
	return err
}

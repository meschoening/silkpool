package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/version"
	pkgwin "github.com/meschoening/silkpool/packaging/windows"
)

func init() {
	modes["install"] = runInstall
	modes["uninstall"] = runUninstall
}

// local is this machine, as install and uninstall change it, making the
// agent's folders as the agent does.
func local() pkgwin.Local { return pkgwin.Local{MakeRootFunc: makeRoot} }

// layoutFlags parses args for a command that takes no arguments but the
// hidden --prefix, a privileged test's, which names everything install
// makes for the test (packaging/windows.ForTest), and returns the
// layout. extra adds the command's own hidden flags.
func layoutFlags(name string, args []string, extra func(*flag.FlagSet)) (pkgwin.Layout, int, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	prefix := fs.String("prefix", "", "")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "usage: silkpool %s\n", name)
		return pkgwin.Layout{}, 2, false
	}
	l, err := pkgwin.LocalLayout(*prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "silkpool %s: %v\n", name, err)
		return pkgwin.Layout{}, 1, false
	}
	return l, 0, true
}

// runInstall is silkpool install, which install.ps1 runs from the
// release's zip, elevated (ADR 0013). Run from a newer release's, it
// upgrades; from the same release's, it repairs. It installs the
// programs beside the one running.
func runInstall(_ context.Context, args []string) int {
	l, code, ok := layoutFlags("install", args, nil)
	if !ok {
		return code
	}
	if !pkgwin.Elevated() {
		fmt.Fprintln(os.Stderr, "Installing changes the whole machine, so run it from an administrator PowerShell: silkpool install")
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "silkpool install: finding this program: %v\n", err)
		return 1
	}
	in := pkgwin.Install{Layout: l, Version: version.Version, From: filepath.Dir(exe), Machine: local(), Log: os.Stdout}
	if err := in.Do(); err != nil {
		var down *pkgwin.Downgrade
		if errors.As(err, &down) {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintf(os.Stderr, "silkpool install: %v\n", err)
		}
		return 1
	}
	return 0
}

// runUninstall is silkpool uninstall, which the Apps entry runs too. It's
// refused while the machine is enrolled, which the running agent
// answers, and with no agent running while its state is there. Once
// it isn't enrolled, it deletes everything install put on the machine
// and the agent made (packaging/windows.Uninstall).
//
// Started without elevation, as the Apps entry starts it, it starts
// itself again elevated, which asks for an administrator's consent, and
// waits for it. That one keeps its window open until Enter is pressed,
// so its last words can be read.
func runUninstall(ctx context.Context, args []string) int {
	var pause bool
	l, code, ok := layoutFlags("uninstall", args, func(fs *flag.FlagSet) { fs.BoolVar(&pause, "pause", false, "") })
	if !ok {
		return code
	}
	if !pkgwin.Elevated() {
		return runElevated(elevatedUninstall(args))
	}
	if pause {
		defer waitForEnter()
	}
	notRunning := ""
	if _, err := os.Lstat(l.State()); err == nil {
		notRunning = fmt.Sprintf("Silkpool's service isn't running on this machine, so it can't say whether it's enrolled. Start it with: Start-Service %s, in an administrator PowerShell", l.Service)
	}
	if refusal := askUninstall(ctx, l.Pipe, notRunning); refusal != "" {
		fmt.Fprintln(os.Stderr, refusal)
		return 1
	}
	if err := (pkgwin.Uninstall{Layout: l, Machine: local(), Log: os.Stdout}).Do(); err != nil {
		var refused *pkgwin.Refused
		if errors.As(err, &refused) {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintf(os.Stderr, "silkpool uninstall: %v\n", err)
		}
		return 1
	}
	return 0
}

// elevatedUninstall is the command line of the elevated uninstall an
// unelevated one starts: the command again, which the modes' dispatch
// took off args, with args, and --pause.
func elevatedUninstall(args []string) []string {
	return append(append([]string{"uninstall"}, args...), "--pause")
}

// waitForEnter keeps an elevated uninstall's own window open until Enter
// is pressed.
func waitForEnter() {
	fmt.Println("Press Enter to close this window.")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

var (
	shell32             = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")
)

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size         uint32
	mask         uint32
	hwnd         windows.Handle
	verb         *uint16
	file         *uint16
	parameters   *uint16
	directory    *uint16
	show         int32
	instApp      windows.Handle
	idList       uintptr
	class        *uint16
	keyClass     windows.Handle
	hotKey       uint32
	iconOrMonitr windows.Handle
	process      windows.Handle
}

// runElevated runs this program again with args, elevated, which asks for
// an administrator's consent, waits for it, and returns its exit code.
func runElevated(args []string) int {
	const (
		seeMaskNoCloseProcess = 0x00000040
		swShowNormal          = 1
	)
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "silkpool uninstall: finding this program: %v\n", err)
		return 1
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windows.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	info := shellExecuteInfo{mask: seeMaskNoCloseProcess, verb: verb, file: file, parameters: params, show: swShowNormal}
	info.size = uint32(unsafe.Sizeof(info))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		fmt.Fprintf(os.Stderr, "Uninstalling changes the whole machine, so it needs an administrator, and Windows didn't start it as one: %v\n", err)
		return 1
	}
	defer windows.CloseHandle(info.process)
	if _, err := windows.WaitForSingleObject(info.process, windows.INFINITE); err != nil {
		return 1
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return 1
	}
	return int(code)
}

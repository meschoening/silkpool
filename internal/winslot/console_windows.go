package winslot

import (
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

// consoleMu keeps one attach at a time: a process has one console.
var consoleMu sync.Mutex

// interrupt sends Ctrl+C to the console of the process pid, and so to
// every process on it, as GitHub's RunnerService.exe stops its listener
// (Runner.Service/Windows/RunnerService.cs): it attaches to the
// listener's console, sets itself to ignore Ctrl+C, which it would
// otherwise get too, sends the event to the console, and detaches. The
// host has no console of its own, as a service, so it can attach. It
// starts no process after this, so it never sets Ctrl+C's handling
// back.
func interrupt(pid uint32) error {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if r, _, err := procAttachConsole.Call(uintptr(pid)); r == 0 {
		return fmt.Errorf("attaching to the listener's console: %w", err)
	}
	defer procFreeConsole.Call()
	if r, _, err := procSetConsoleCtrlHandler.Call(0, 1); r == 0 {
		return fmt.Errorf("ignoring Ctrl+C itself: %w", err)
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
		return fmt.Errorf("sending Ctrl+C: %w", err)
	}
	return nil
}

// takeCtrlC clears the host's own setting to ignore Ctrl+C, which it
// may have inherited and which the listener would inherit from it in
// turn: a listener that ignores Ctrl+C can't be stopped gracefully.
// RunnerService.exe clears it for the same reason.
func takeCtrlC() error {
	if r, _, err := procSetConsoleCtrlHandler.Call(0, 0); r == 0 {
		return err
	}
	return nil
}

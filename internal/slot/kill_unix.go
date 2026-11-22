//go:build unix

package slot

import (
	"fmt"
	"os"
	"syscall"
)

// killRounds is how many times the clean runs its kill helper before it
// gives up on processes that won't die, such as one stuck in the kernel.
const killRounds = 10

// stillRun is the error for the account's processes pids that still
// run, or nil if there are none.
func stillRun(pids []int) error {
	switch len(pids) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("its process %d still runs", pids[0])
	}
	return fmt.Errorf("%d of its processes still run, such as %d", len(pids), pids[0])
}

// KillAllMode is the hidden argument that runs silkpool as the clean's
// kill helper, `silkpool kill-all <uid>`, which the agent runs as the
// service account.
const KillAllMode = "kill-all"

// KillAll kills every process of the service account it runs as, but
// itself, with one kill(-1, SIGKILL): the kernel signals every process
// the caller may signal while new ones can't start, so none forks its
// way past it. uid is the account it must run as. It refuses to run as
// any other, and as root, for which kill(-1) would end every process on
// the machine, and for a UID the service account can't have
// (checkAccountUID).
func KillAll(uid int) error {
	if err := checkAccountUID(uid); err != nil {
		return err
	}
	if err := checkKillUID(uid, os.Getuid(), os.Geteuid()); err != nil {
		return err
	}
	if err := syscall.Kill(-1, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// checkKillUID returns why the kill helper mustn't run, as real and
// effective UIDs, for the account want, or nil.
func checkKillUID(want, real, effective int) error {
	if want <= 0 || real != want || effective != want {
		return fmt.Errorf("%s runs only as the service account it names (%d), not as UID %d", KillAllMode, want, real)
	}
	return nil
}

package slot

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Helper runs silkpool as one of the helpers Silkpool starts as the
// service account, if args, the arguments after the program's name, name
// one: the runner's launcher (LaunchMode), the clean's kill helper
// (KillAllMode) or the exposure check (CheckMode). It returns the exit
// code to exit with, and false if args name none of them.
func Helper(args []string) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case LaunchMode:
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, LaunchMode+" takes the runner root")
			return 2, true
		}
		fmt.Fprintln(os.Stderr, Launch(args[1]))
		return 1, true
	case KillAllMode:
		uid, err := strconv.Atoi(strings.Join(args[1:], " "))
		if err != nil {
			fmt.Fprintln(os.Stderr, KillAllMode+" takes the service account's UID")
			return 2, true
		}
		if err := KillAll(uid); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		return 0, true
	case CheckMode:
		return checkExposure(args[1:], os.Stdout), true
	}
	return 0, false
}

// checkAccountUID accepts any UID: the Linux service account is a system
// account whose UID the OS picks, which KillAll's other checks keep from
// root.
func checkAccountUID(int) error { return nil }

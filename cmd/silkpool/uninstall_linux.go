package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/meschoening/silkpool/internal/control"
)

func init() {
	modes["uninstall-check"] = uninstallCheck
}

// uninstallCheck is the check the node package's preremove script makes
// (ADR 0013): it exits 1, saying why, while the machine is enrolled, and
// 0 once it isn't. It asks the running agent through the control
// channel, whatever the agent's version, and refuses if it can't ask.
func uninstallCheck(ctx context.Context, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: silkpool uninstall-check")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c, err := control.Dial(ctx, control.Path, 0)
	if errors.Is(err, control.ErrNotRunning) {
		fmt.Fprintf(os.Stderr, "Silkpool's service isn't running on this machine. Start it with: %s\n", startCommand)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't ask Silkpool's service whether this machine is enrolled: %v\n", err)
		return 1
	}
	defer c.Close()
	st, err := c.Status(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't ask Silkpool's service whether this machine is enrolled: %v\n", err)
		return 1
	}
	if refusal := uninstallRefusal(st); refusal != "" {
		fmt.Fprintln(os.Stderr, refusal)
		return 1
	}
	return 0
}

// uninstallRefusal is why Silkpool can't be uninstalled from a machine
// whose agent reads st, or empty if it can.
func uninstallRefusal(st control.Status) string {
	if !st.Node.Enrolled {
		return ""
	}
	return fmt.Sprintf("This machine is still enrolled in %s. Run `silkpool remove` first, or `silkpool remove --force` if the Hub is gone.", st.Hub.URL)
}

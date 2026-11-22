package main

import (
	"context"
	"fmt"
	"os"

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
	notRunning := fmt.Sprintf("Silkpool's service isn't running on this machine. Start it with: %s", startCommand)
	if refusal := askUninstall(ctx, control.Path, notRunning); refusal != "" {
		fmt.Fprintln(os.Stderr, refusal)
		return 1
	}
	return 0
}

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/meschoening/silkpool/internal/control"
)

// askUninstall asks the agent at socket, whatever its version, whether
// the machine is enrolled, and returns why uninstalling Silkpool is
// refused, or empty if it may go ahead (ADR 0013). notRunning is the
// refusal when no agent is listening there, or empty to go ahead then.
// It refuses if it can't ask.
func askUninstall(ctx context.Context, socket, notRunning string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c, err := control.Dial(ctx, socket, 0)
	if errors.Is(err, control.ErrNotRunning) {
		return notRunning
	}
	if err != nil {
		return fmt.Sprintf("Couldn't ask Silkpool's service whether this machine is enrolled: %v", err)
	}
	defer c.Close()
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Sprintf("Couldn't ask Silkpool's service whether this machine is enrolled: %v", err)
	}
	return uninstallRefusal(st)
}

// uninstallRefusal is why Silkpool can't be uninstalled from a machine
// whose agent reads st, or empty if it can.
func uninstallRefusal(st control.Status) string {
	if !st.Node.Enrolled {
		return ""
	}
	return fmt.Sprintf("This machine is still enrolled in %s. Run `silkpool remove` first, or `silkpool remove --force` if the Hub is gone.", st.Hub.URL)
}

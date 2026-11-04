package hubops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/meschoening/silkpool/internal/diagnostics"
)

// DiagnosticsWithin is how long Diagnostics waits for a node's report.
// The agent makes it at once, from what it holds and the end of its
// log.
const DiagnosticsWithin = 30 * time.Second

// Diagnostics asks the node with id for its diagnostics report and
// waits for it, at most DiagnosticsWithin, for the dashboard's node page
// to show and download. It's refused for a node the Hub doesn't hold or
// has removed, and for one that isn't connected, or whose connection
// ends before it answers. The Hub doesn't keep the report.
func (o *Ops) Diagnostics(ctx context.Context, id string) (diagnostics.Report, error) {
	h, err := o.state(ctx)
	if err != nil {
		return diagnostics.Report{}, err
	}
	n, ok := byID(h, id)
	if !ok || !n.Removed.IsZero() {
		return diagnostics.Report{}, refuse("unknown-node", "the Hub has no node with the ID %s", id)
	}
	waiting, cancel := context.WithTimeout(ctx, DiagnosticsWithin)
	defer cancel()
	r, err := o.c.Hub.Diagnostics(waiting, n.ID)
	switch {
	case errors.Is(err, diagnostics.ErrNotConnected):
		return diagnostics.Report{}, refuse("not-connected", "%s isn't connected, so it can't send its diagnostics report", n.Name)
	case err != nil && waiting.Err() != nil && ctx.Err() == nil:
		return diagnostics.Report{}, fmt.Errorf("%s didn't send its diagnostics report within %s", n.Name, DiagnosticsWithin)
	}
	return r, err
}

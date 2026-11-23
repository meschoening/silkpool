package hubops

import (
	"context"
	"errors"
	"time"

	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/wording"
)

// DiagnosticsWithin is how long Diagnostics waits for a node's report.
// The agent makes it at once, from what it holds and the end of its
// log.
const DiagnosticsWithin = 30 * time.Second

// Diagnostics asks the node with id for its diagnostics report and
// waits for it, at most DiagnosticsWithin, for the dashboard's node page
// to show. It's refused for a node the Hub doesn't hold or has removed
// (unknown-node), for one that isn't connected, or whose connection ends
// before it answers (not-connected), and for one that doesn't answer
// within DiagnosticsWithin (no-answer). The Hub doesn't keep the report.
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
		return diagnostics.Report{}, refuse("not-connected", "%s", wording.DiagnosticsNotConnected(n.Name))
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		// The wait ran out, not the caller's.
		return diagnostics.Report{}, refuse("no-answer", "%s", wording.DiagnosticsNoAnswer(n.Name, DiagnosticsWithin))
	}
	return r, err
}

package hub

import (
	"context"
	"fmt"

	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/wire"
)

// An ask is an ask for a node's diagnostics report, out on a link: the
// answer, or why there's none, goes to answer.
type ask struct {
	link   *link
	answer chan asked
}

// asked is an ask's answer.
type asked struct {
	report diagnostics.Report
	err    error
}

// Diagnostics asks the node with id for its diagnostics report, and
// waits for its answer until ctx ends. It's refused with
// diagnostics.ErrNotConnected for a node that isn't connected, and fails
// with it if the node's connection ends before it answers. The Hub
// doesn't keep the report.
func (h *Hub) Diagnostics(ctx context.Context, id string) (diagnostics.Report, error) {
	answer := make(chan asked, 1)
	var askID string
	if !h.query(ctx, func() { askID = h.ask(id, answer) }) {
		return diagnostics.Report{}, ErrStopped
	}
	if askID == "" {
		return diagnostics.Report{}, diagnostics.ErrNotConnected
	}
	select {
	case a := <-answer:
		return a.report, a.err
	case <-ctx.Done():
		h.do(context.Background(), func() { delete(h.asks, askID) })
		return diagnostics.Report{}, ctx.Err()
	case <-h.done:
		return diagnostics.Report{}, ErrStopped
	}
}

// ask sends node id an ask for its report on its connection, and
// returns the ask's ID, or "" if the node isn't connected.
func (h *Hub) ask(id string, answer chan asked) string {
	l := h.links[id]
	if l == nil {
		return ""
	}
	h.asked++
	askID := fmt.Sprintf("ask-%d", h.asked)
	h.asks[askID] = &ask{link: l, answer: answer}
	h.log.Info("asked a node for its diagnostics report", "node", id)
	h.send(id, wire.AskDiagnostics{ID: askID})
	return askID
}

// answered hands the report a node sent on l to its ask, if the ask is
// still out on l.
func (h *Hub) answered(l *link, m wire.DiagnosticsReport) {
	a := h.asks[m.ID]
	if a == nil || a.link != l {
		h.log.Warn("dropped a diagnostics report no ask is waiting for", "node", l.node)
		return
	}
	delete(h.asks, m.ID)
	a.answer <- asked{report: m.Report}
}

// unasked fails the asks out on l, whose connection has ended, so no
// answer can come.
func (h *Hub) unasked(l *link) {
	for id, a := range h.asks {
		if a.link == l {
			delete(h.asks, id)
			a.answer <- asked{err: diagnostics.ErrNotConnected}
		}
	}
}

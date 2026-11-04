package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/meschoening/silkpool/internal/agentstore"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/wire"
)

// Control returns the agent as the control channel's server asks of it.
func (a *Agent) Control() control.Agent { return controlled{a} }

// controlled is the agent behind the control channel.
type controlled struct{ a *Agent }

// status returns the agent's status. It runs on the loop's goroutine.
func (a *Agent) status() (control.Status, error) {
	s := a.store.Settings()
	var hub control.Hub
	if s.Node != "" {
		hub = control.Hub{URL: s.Hub.URL, Node: s.Node, Since: store.Stamp(a.hubSince)}
	}
	return control.StatusOf(a.c.Machine.AgentVersion, a.c.Machine, store.Stamp(time.Now()), a.store.Node(), hub)
}

func (c controlled) Status(ctx context.Context) (control.Status, error) {
	var st control.Status
	var err error
	if !c.a.query(ctx, func() { st, err = c.a.status() }) {
		return st, ErrStopped
	}
	return st, err
}

func (c controlled) Watch(ctx context.Context, send func(control.Status) error) error {
	a := c.a
	for {
		var st control.Status
		var err error
		var changed chan struct{}
		if !a.query(ctx, func() { st, changed, err = a.status2() }) {
			if ctx.Err() != nil {
				return nil
			}
			return ErrStopped
		}
		if err != nil {
			return err
		}
		if err := send(st); err != nil {
			return err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		case <-a.done:
			return ErrStopped
		}
	}
}

// status2 returns the status and the channel closed at the next change.
func (a *Agent) status2() (control.Status, chan struct{}, error) {
	st, err := a.status()
	return st, a.changed, err
}

func (c controlled) Command(ctx context.Context, cmd node.Command) (node.Reply, error) {
	return c.a.Do(ctx, cmd)
}

func (c controlled) Dismiss(ctx context.Context, d node.Dismiss) (node.Reply, error) {
	return c.a.Do(ctx, d)
}

func (c controlled) Remove(ctx context.Context, r node.Remove) (node.Reply, error) {
	return c.a.Do(ctx, r)
}

func (c controlled) Enroll(ctx context.Context, e control.Enrollment) (node.Reply, error) {
	r, err := c.a.Enroll(ctx, Enrollment{ID: e.ID, Origin: e.Origin, Code: e.Code, Name: e.Name})
	switch {
	case errors.Is(r.Refused, ErrAnswerLost):
		r.Refused = &control.Error{Code: control.CodeAnswerLost, Message: r.Refused.Error()}
	case errors.Is(r.Refused, ErrUnreachable):
		r.Refused = &control.Error{Code: control.CodeUnreachable, Message: r.Refused.Error()}
	}
	return r, err
}

func (c controlled) SetHubURL(ctx context.Context, u string) error { return c.a.SetHubURL(ctx, u) }

func (c controlled) Diagnostics(ctx context.Context) (diagnostics.Report, error) {
	return c.a.Diagnostics(ctx)
}

// ErrSetHubURLNotEnrolled refuses set-hub-url on a machine that isn't
// enrolled.
var ErrSetHubURLNotEnrolled = errors.New("this machine isn't enrolled in a Hub, so it has no Hub URL to change")

// SetHubURL re-points the node at its Hub's new URL. It first checks
// that the Hub there is the node's own: verified by the node's pin, or
// through the trusted roots for a Hub without one, and naming the Hub's
// ID in its challenge. A failed check keeps the old URL and says why.
// The URL it already has is nothing to change, and isn't checked again.
func (a *Agent) SetHubURL(ctx context.Context, newURL string) error {
	u, err := url.Parse(newURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%q isn't an https:// URL", newURL)
	}
	var s agentstore.Settings
	var enrolled bool
	if !a.query(ctx, func() { s, enrolled = a.store.Settings(), a.store.Node().Enrolled }) {
		return ErrStopped
	}
	if !enrolled || s.Node == "" {
		return ErrSetHubURLNotEnrolled
	}
	if newURL == s.Hub.URL {
		// Nothing to change, as for a resend of one that applied.
		return nil
	}
	if err := a.checkHub(ctx, newURL, s); err != nil {
		return err
	}
	_, err = a.Apply(ctx, agentstore.SetHubURL{URL: newURL})
	return err
}

// checkHub checks that the Hub at hubURL is the one s names.
func (a *Agent) checkHub(ctx context.Context, hubURL string, s agentstore.Settings) error {
	ctx, cancel := context.WithTimeout(ctx, wire.HandshakeWithin)
	defer cancel()
	client, t := a.client(s.Hub.Pin)
	defer t.CloseIdleConnections()
	ws, _, err := websocket.Dial(ctx, strings.TrimSuffix(hubURL, "/")+wire.ConnectPath, &websocket.DialOptions{
		HTTPClient:      client,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if errors.Is(err, node.ErrHubIdentityChanged) {
			return fmt.Errorf("the Hub at %s doesn't present the key this machine pinned when it enrolled", hubURL)
		}
		return unreachable(hubURL, err)
	}
	defer ws.CloseNow()
	b, err := wire.Read(ctx, ws)
	if err != nil {
		return unreachable(hubURL, err)
	}
	m, err := wire.DecodeToAgent(b)
	challenge, ok := m.(wire.Challenge)
	if err != nil || !ok {
		return fmt.Errorf("what answers at %s isn't a Silkpool Hub", hubURL)
	}
	if challenge.Hub != s.Hub.ID {
		return fmt.Errorf("the Hub at %s is another Hub, not the one this node is enrolled in", hubURL)
	}
	return nil
}

// unreachable is a check that couldn't reach the Hub, which the CLI
// tells from a refusal.
func unreachable(hubURL string, err error) error {
	return &control.Error{Code: control.CodeUnreachable, Message: fmt.Sprintf("couldn't reach a Hub at %s: %v", hubURL, err)}
}

// Diagnostics returns the diagnostics report, as the node CLI prints it.
func (a *Agent) Diagnostics(ctx context.Context) (diagnostics.Report, error) {
	var src diagnostics.Sources
	if !a.query(ctx, func() { src = a.sources() }) {
		return diagnostics.Report{}, ErrStopped
	}
	return a.diagnose(src)
}

// sources returns what the report holds of the loop's state: the node,
// the slots' latest exposure checks, and the clock's offset from the
// Hub. It runs on the loop's goroutine.
func (a *Agent) sources() diagnostics.Sources {
	src := diagnostics.Sources{
		At:      time.Now(),
		Machine: diagnostics.Machine(a.c.Machine),
		Node:    a.store.Node(),
		Clock:   a.clock,
	}
	for _, n := range slices.Sorted(maps.Keys(a.checks)) {
		src.Checks = append(src.Checks, a.checks[n])
	}
	return src
}

// diagnose makes the report of src with what it reads of the machine:
// the free disk and the end of the agent's own log. It runs off the
// loop, since reading them can take a while.
func (a *Agent) diagnose(src diagnostics.Sources) (diagnostics.Report, error) {
	src.FreeDisk = -1
	if a.c.FreeDisk != nil {
		if free, err := a.c.FreeDisk(); err == nil {
			src.FreeDisk = int64(free)
		} else {
			a.log.Warn("couldn't read the free disk space", "err", err)
		}
	}
	if a.c.LogTail != nil {
		if lines, err := a.c.LogTail(diagnostics.LogLines); err == nil {
			src.Log = lines
		} else {
			a.log.Warn("couldn't read the agent's own log", "err", err)
		}
	}
	return diagnostics.Of(src)
}

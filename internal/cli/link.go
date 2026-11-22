package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/control"
)

// The tray app's link to the agent, from "Connect the tray app to the
// agent's control channel": the CLI's own control-channel client, with
// its check that the server runs as root or SYSTEM, which reads the
// status stream `silkpool status --watch` reads and sends commands as
// the CLI sends them, from the tray.

// A Reading is what the tray read of the agent: its status, or why it
// has none.
type Reading struct {
	// Status is the agent's status, if Err is nil.
	Status control.Status
	// Version is the running agent's version, as its hello said, or
	// empty if the tray couldn't connect.
	Version string
	// Err says why there's no status: control.ErrNotRunning while the
	// agent isn't running, a control.NotAdmin when the peer check
	// refused the tray's account, a control.NotServer when something
	// else holds the agent's endpoint, or what else failed.
	Err error
}

// A Link is the tray app's link to the agent. It reads and commands the
// agent through Env's Socket, ServerUID, Dial and NewID, as the CLI
// does, and checks the agent's version against Env's Version before it
// sends a command.
type Link struct {
	env Env
	// Retry is how long Watch waits to dial again after a dial or a
	// watch fails.
	Retry time.Duration
}

// NewLink returns the tray's link to the agent that env reaches.
func NewLink(env Env) *Link {
	if env.NewID == nil {
		env.NewID = randomTrayID
	}
	return &Link{env: env, Retry: 3 * time.Second}
}

func randomTrayID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "tray-" + hex.EncodeToString(b)
}

// Watch reads the agent's status, as `silkpool status --watch` does,
// until ctx ends, and calls f with each reading: the status now and
// after every change, or why there's none. It follows the agent through
// its restarts and upgrades: when a watch that read a status ends, as
// when the agent stops, it dials again at once, and after any other
// failure it says why, then dials again after Retry.
func (l *Link) Watch(ctx context.Context, f func(Reading)) {
	again := false
	for ctx.Err() == nil {
		read := l.watch(ctx, f)
		if ctx.Err() != nil {
			return
		}
		if read && !again {
			again = true
			continue
		}
		again = false
		t := time.NewTimer(l.Retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// watch dials the agent and watches its status until the watch ends,
// and reports whether it read one. A failure before it read one is
// passed on as a reading.
func (l *Link) watch(ctx context.Context, f func(Reading)) (read bool) {
	s := &session{env: l.env, tray: true}
	c, err := s.dial(ctx)
	if err != nil {
		if ctx.Err() == nil {
			f(Reading{Err: err})
		}
		return false
	}
	defer c.Close()
	err = c.Watch(ctx, func(st control.Status) error {
		read = true
		f(Reading{Status: st, Version: c.Hello.Version})
		return nil
	})
	if err == nil && ctx.Err() == nil {
		err = control.ErrDropped
	}
	if !read && ctx.Err() == nil {
		f(Reading{Version: c.Hello.Version, Err: err})
	}
	return read
}

// VersionMismatch refuses to send the tray's command to an agent of
// another version, as the CLI refuses: the tray relaunches itself from
// its new binary once it sees the agent's version change.
type VersionMismatch struct{ Tray, Service string }

func (e VersionMismatch) Error() string {
	return fmt.Sprintf("this tray app is %s but the running service is %s", e.Tray, e.Service)
}

// Command sends the availability command e from the tray, under a
// command ID of its own, and returns the agent's reply, which holds its
// refusal if it refused. Revisions work as they do in the CLI: e carries
// the revision the panel showed, and the agent refuses it as stale if
// the node changed since. So do resends: if the connection drops before
// the reply, it dials again and sends e once more under the same ID,
// which the agent answers with its first reply if the first applied.
// Dropped twice, it returns ErrNotConfirmed. It fails with what the dial
// failed with if it couldn't reach the agent, and with a
// VersionMismatch if the agent isn't the tray's version.
func (l *Link) Command(ctx context.Context, e availability.Event) (control.Reply, error) {
	id := l.env.NewID()
	return l.send(ctx, func(c *control.Client) (control.Reply, error) { return c.Command(ctx, id, e) })
}

// Dismiss dismisses the notices with IDs notices from the tray, under a
// command ID of its own, as Command sends a command.
func (l *Link) Dismiss(ctx context.Context, notices []int64) (control.Reply, error) {
	id := l.env.NewID()
	return l.send(ctx, func(c *control.Client) (control.Reply, error) { return c.Dismiss(ctx, id, notices) })
}

// send connects, checks the agent's version, and sends a request through
// do with the CLI's one resend.
func (l *Link) send(ctx context.Context, do func(*control.Client) (control.Reply, error)) (control.Reply, error) {
	s := &session{env: l.env, tray: true}
	defer s.close()
	c, err := s.dial(ctx)
	if err != nil {
		return control.Reply{}, err
	}
	s.client = c
	if c.Hello.Version != l.env.Version {
		return control.Reply{}, VersionMismatch{Tray: l.env.Version, Service: c.Hello.Version}
	}
	var r control.Reply
	err = s.once(ctx, func(c *control.Client) (err error) {
		r, err = do(c)
		return err
	})
	return r, err
}

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/meschoening/silkpool/internal/agentstore"
	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/wire"
)

// Reconnects back off from FirstRetry to MaxRetry, doubling after each
// failure, and each wait is drawn at random from the upper half of its
// step, so nodes that lost their Hub together don't all come back at
// once. A connection the Hub accepted starts the backoff again.
const (
	FirstRetry = time.Second
	MaxRetry   = time.Minute
)

// messageLimit is the most the Hub may send in one message.
const messageLimit = 1 << 20

// ErrUnreachable is why an enroll failed when the Hub can't be reached.
var ErrUnreachable = errors.New("the Hub can't be reached")

// ErrAnswerLost is why an enroll failed when the Hub's answer was lost
// after the enroll went out, and again on its one retry. The Hub may
// have enrolled the machine, and enrolling again with the same code is
// safe: the Hub answers as the node it enrolled.
var ErrAnswerLost = errors.New("the Hub's answer to the enroll was lost")

// A dialer keeps the connection to the Hub its settings name. key tells
// one set of settings from another.
type dialer struct {
	key  string
	stop context.CancelFunc
}

// backoff is the wait before each attempt to connect again.
type backoff struct{ step time.Duration }

func (b *backoff) next() time.Duration {
	if b.step == 0 {
		b.step = FirstRetry
	}
	d := b.step
	b.step = min(2*b.step, MaxRetry)
	return d/2 + rand.N(d/2+1)
}

func (b *backoff) reset() { b.step = 0 }

// dial keeps a connection to the Hub until ctx ends: it connects, serves
// the connection until it ends, and connects again after a backoff.
// "Removed" doesn't end it: the agent applies each one and asks again
// after the backoff, which only an accepted connection resets, so it
// settles at 30 to 60 s. A later "removed" names the runners whose
// target the Hub has lost since, and the wipe stops the dialer.
func (a *Agent) dial(ctx context.Context, d *dialer, s agentstore.Settings, key ed25519.PrivateKey) {
	var wait backoff
	for ctx.Err() == nil {
		conn, clock, removed, reason, err := a.attempt(ctx, s, key)
		switch {
		case removed != nil:
			a.do(ctx, func() { a.removed(d, *removed) })
		case conn != nil:
			wait.reset()
			if !a.do(ctx, func() { a.connected(d, conn, clock) }) {
				conn.Close()
				return
			}
			// The connection outlives a stopped dialer until the loop
			// ends it, so what's queued, such as a confirmation, is sent.
			err := conn.Serve(a.ctx, func(b []byte) {
				m, err := wire.DecodeToAgent(b)
				if err != nil {
					a.log.Warn("rejected a message from the Hub", "err", err)
					return
				}
				a.do(ctx, func() { a.received(conn, m) })
			})
			a.log.Info("the connection to the Hub ended", "why", err)
			// The end must reach the loop even if the dialer was
			// stopped, so it's applied while the agent runs.
			a.do(a.ctx, func() { a.ended(conn) })
		case ctx.Err() == nil:
			a.do(ctx, func() { a.failed(d, reason, err) })
		}
		t := time.NewTimer(wait.next())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// client returns an HTTP client that reaches the Hub over verified TLS,
// with its pin or through the trusted roots, and its transport.
func (a *Agent) client(pin node.Pin) (*http.Client, *http.Transport) {
	dial := a.c.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: wire.HandshakeWithin}).DialContext
	}
	t := &http.Transport{
		DialContext:         dial,
		TLSClientConfig:     wire.TLS(pin, a.c.Roots),
		TLSHandshakeTimeout: wire.HandshakeWithin,
		// A Hub that stalls before answering the upgrade or the enroll
		// mustn't hold up the backoff.
		ResponseHeaderTimeout: wire.HandshakeWithin,
		DisableKeepAlives:     true,
	}
	return &http.Client{Transport: t}, t
}

// attempt connects to the Hub once, through the handshake. It returns
// the connection the Hub accepted, with the clock's offset from the
// Hub's, or the Hub's "removed" if it has removed the node, or why it
// failed: the Hub's refusal of the agent's version, a Hub key that
// doesn't match the pin, the Hub's refusal of the node key, or anything
// else as HubUnreachable.
func (a *Agent) attempt(ctx context.Context, s agentstore.Settings, key ed25519.PrivateKey) (conn *wire.Conn, clock diagnostics.Clock, removed *wire.Removed, reason node.Reason, err error) {
	client, t := a.client(s.Hub.Pin)
	defer t.CloseIdleConnections()
	ws, resp, err := websocket.Dial(ctx, strings.TrimSuffix(s.Hub.URL, "/")+wire.ConnectPath, &websocket.DialOptions{
		HTTPClient:      client,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if errors.Is(err, node.ErrHubIdentityChanged) {
			return nil, clock, nil, node.HubIdentityChanged, err
		}
		return nil, clock, nil, node.HubUnreachable, err
	}
	clock = clockOf(resp, time.Now())
	m, err := handshake(ctx, ws, a.c.Protocol, a.c.Platform, s, key)
	switch m := m.(type) {
	case wire.Accepted:
		return wire.NewConn(ws, messageLimit), clock, nil, 0, nil
	case wire.Removed:
		ws.CloseNow()
		return nil, clock, &m, 0, nil
	case wire.Refused:
		ws.CloseNow()
		switch {
		case errors.As(m.Reason, new(pool.VersionMismatch)):
			return nil, clock, nil, node.VersionMismatch, m.Reason
		case errors.Is(m.Reason, pool.ErrWrongKey):
			return nil, clock, nil, node.WrongNodeKey, fmt.Errorf("the Hub holds a different key for this node: %w", m.Reason)
		}
		return nil, clock, nil, node.HubUnreachable, fmt.Errorf("the Hub refused the connection: %w", m.Reason)
	}
	ws.CloseNow()
	if err == nil {
		err = fmt.Errorf("the Hub answered the Hello with %T", m)
	}
	return nil, clock, nil, node.HubUnreachable, err
}

// clockOf returns the clock's offset from the Hub's, from the Date of
// the Hub's answer to the connection, which came at received, or zero
// if it has none. The Date is to the second, so the offset takes the
// middle of its second, and is to the second too.
func clockOf(resp *http.Response, received time.Time) diagnostics.Clock {
	if resp == nil {
		return diagnostics.Clock{}
	}
	at, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return diagnostics.Clock{}
	}
	offset := received.Sub(at.Add(500 * time.Millisecond)).Round(time.Second)
	return diagnostics.Clock{Offset: offset, Measured: store.Stamp(received)}
}

// handshake answers the Hub's Challenge with a signed Hello, and returns
// the Hub's answer.
func handshake(ctx context.Context, ws *websocket.Conn, protocol int, platform runnerapp.Platform, s agentstore.Settings, key ed25519.PrivateKey) (any, error) {
	b, err := wire.Read(ctx, ws)
	if err != nil {
		return nil, err
	}
	m, err := wire.DecodeToAgent(b)
	if err != nil {
		return nil, err
	}
	challenge, ok := m.(wire.Challenge)
	if !ok {
		return nil, fmt.Errorf("the Hub sent %T before its Challenge", m)
	}
	hello, err := wire.EncodeToHub(wire.Hello{
		Protocol:  protocol,
		Node:      s.Node,
		Platform:  platform,
		Signature: wire.Sign(key, challenge.Nonce, s.Node, s.Hub.ID),
	})
	if err != nil {
		return nil, err
	}
	if err := wire.Write(ctx, ws, hello); err != nil {
		return nil, err
	}
	if b, err = wire.Read(ctx, ws); err != nil {
		return nil, err
	}
	return wire.DecodeToAgent(b)
}

// post sends an enroll to the Hub's enrollment endpoint, with origin,
// the administrator's, and returns its answer.
//
// An answer lost after the enroll went out is asked for once more, with
// the same enroll: the same code and the same pending node key, which
// the Hub answers as the node it enrolled, if it did. If the retry's
// answer is lost too, the enroll fails with ErrAnswerLost. Otherwise
// the retry's answer, or why there's none, is the enroll's. Nothing
// else is retried: a Hub that can't be reached before the enroll goes
// out fails with ErrUnreachable, one whose key doesn't match the code's
// pin with node.ErrHubIdentityChanged, and the Hub's answers, its
// refusals among them, stand.
func (a *Agent) post(ctx context.Context, en *enrolling, origin node.Origin) (wire.Answer, error) {
	body, err := wire.MarshalEnroll(wire.Enroll{
		Secret:   en.code.Secret,
		Key:      wire.PublicKey(en.key.Public().(ed25519.PublicKey)),
		Name:     en.name,
		Protocol: a.c.Protocol,
		Machine:  a.c.Machine,
		Origin:   origin,
	})
	if err != nil {
		return wire.Answer{}, err
	}
	answer, err := a.ask(ctx, en.code, body)
	var l lost
	switch {
	case !errors.As(err, &l):
		return answer, err
	case ctx.Err() != nil:
		// The agent is stopping, and nobody hears the answer.
		return wire.Answer{}, fmt.Errorf("%w at %s: %w", ErrUnreachable, en.code.URL, l.err)
	}
	a.log.Warn("the Hub's answer to the enroll was lost, so the agent sends the enroll once more", "err", l.err)
	answer, err = a.ask(ctx, en.code, body)
	if errors.As(err, &l) {
		return wire.Answer{}, fmt.Errorf("%w at %s, twice: %w", ErrAnswerLost, en.code.URL, l.err)
	}
	return answer, err
}

// lost is an answer lost after the enroll went out, and err why: the
// connection ended, or the answer didn't come in time.
type lost struct{ err error }

func (l lost) Error() string { return l.err.Error() }
func (l lost) Unwrap() error { return l.err }

// ask sends the enroll body to the Hub code names, once, and returns its
// answer. An answer lost after the enroll went out fails with lost.
func (a *Agent) ask(ctx context.Context, code wire.Code, body []byte) (wire.Answer, error) {
	client, t := a.client(code.Pin)
	defer t.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 2*wire.HandshakeWithin)
	defer cancel()
	// The transport writes the enroll only on a connection to the Hub,
	// once its key has passed the pin's check, and tells its trace when
	// it has. That comes just before the write's flush, so a connection
	// that ends in that moment counts as an answer lost too: a retry is
	// safe either way.
	var sent atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			sent.Store(true)
		}
	}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(code.URL, "/")+wire.EnrollPath, bytes.NewReader(body))
	if err != nil {
		return wire.Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, node.ErrHubIdentityChanged):
			return wire.Answer{}, node.ErrHubIdentityChanged
		case sent.Load():
			return wire.Answer{}, lost{err}
		}
		return wire.Answer{}, fmt.Errorf("%w at %s: %w", ErrUnreachable, code.URL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, messageLimit))
	if resp.StatusCode != http.StatusOK {
		// The status is the answer, even if what follows it is cut short.
		return wire.Answer{}, fmt.Errorf("the Hub at %s answered %s: %s", code.URL, resp.Status, bytes.TrimSpace(b))
	}
	if err != nil {
		// The connection ended in the middle of the answer.
		return wire.Answer{}, lost{err}
	}
	return wire.UnmarshalAnswer(b)
}

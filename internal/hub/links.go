package hub

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/coder/websocket"

	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/registration"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wire"
)

// The most an agent may send in one message, and in one enrollment. A
// delivery after a long time away can carry many outbox entries.
const (
	messageLimit = 8 << 20
	enrollLimit  = 64 << 10
)

// A link is one agent's connection.
type link struct {
	node string
	conn *wire.Conn
	// verdict carries the loop's answer to the handshake: nil to accept
	// the connection, or the message that refuses it.
	verdict chan []byte
}

// enrollment is the enrollment the loop is applying: the effects that
// answer it.
type enrollment struct{ effects []hubstore.Effect }

// Handler serves the Hub's agents: the WebSocket each one dials at
// wire.ConnectPath, and the enrollment endpoint at wire.EnrollPath. It
// answers only while Run runs.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.ConnectPath, h.serveConnect)
	mux.HandleFunc("POST "+wire.EnrollPath, h.serveEnroll)
	return mux
}

// requestContext returns a context that ends with r's, or when the Hub
// stops.
func (h *Hub) requestContext(r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	go func() {
		select {
		case <-h.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// query runs f on the loop's goroutine and waits for it, reporting false
// if the Hub isn't running.
func (h *Hub) query(ctx context.Context, f func()) bool {
	ran := make(chan struct{})
	if !h.do(ctx, func() { f(); close(ran) }) {
		return false
	}
	select {
	case <-ran:
		return true
	case <-h.done:
		return false
	}
}

// serveConnect runs one agent's connection: the handshake, then its
// messages until it ends.
func (h *Hub) serveConnect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := h.requestContext(r)
	defer cancel()
	var hubID string
	if !h.query(ctx, func() { hubID = h.store.Hub().Settings.HubID }) {
		http.Error(w, "the Hub isn't running", http.StatusServiceUnavailable)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		h.log.Info("an agent's connection failed before it began", "err", scrubbed(err))
		return
	}
	defer ws.CloseNow()
	hello, nonce, err := h.handshake(ctx, ws, hubID)
	if err != nil {
		h.log.Info("an agent's connection failed in its handshake", "err", scrubbed(err))
		return
	}
	l := &link{node: hello.Node, conn: wire.NewConn(ws, messageLimit), verdict: make(chan []byte, 1)}
	if !h.do(ctx, func() { h.hello(l, hello, nonce, hubID) }) {
		return
	}
	var verdict []byte
	select {
	case verdict = <-l.verdict:
	case <-ctx.Done():
		return
	}
	if verdict != nil {
		if err := wire.Write(ctx, ws, verdict); err == nil {
			ws.Close(websocket.StatusPolicyViolation, "refused")
		}
		return
	}
	err = l.conn.Serve(ctx, func(b []byte) {
		m, err := wire.DecodeToHub(b)
		if err != nil {
			h.log.Warn("rejected a message from a node", "node", l.node, "err", scrubbed(err))
			return
		}
		h.do(ctx, func() { h.received(l, m) })
	})
	h.log.Info("a node's connection ended", "node", l.node, "why", scrubbed(err))
	h.do(context.Background(), func() { h.ended(l) })
}

// handshake sends the Challenge and reads the Hello.
func (h *Hub) handshake(ctx context.Context, ws *websocket.Conn, hubID string) (wire.Hello, []byte, error) {
	nonce := wire.NewNonce()
	challenge, err := wire.EncodeToAgent(wire.Challenge{Protocol: h.c.Protocol, Hub: hubID, Nonce: nonce})
	if err != nil {
		return wire.Hello{}, nil, err
	}
	if err := wire.Write(ctx, ws, challenge); err != nil {
		return wire.Hello{}, nil, err
	}
	b, err := wire.Read(ctx, ws)
	if err != nil {
		return wire.Hello{}, nil, err
	}
	m, err := wire.DecodeToHub(b)
	if err != nil {
		return wire.Hello{}, nil, err
	}
	hello, ok := m.(wire.Hello)
	if !ok {
		return wire.Hello{}, nil, fmt.Errorf("the agent sent %T before its Hello", m)
	}
	return hello, nonce, nil
}

// hello decides a connection whose Hello has come: a node the Hub
// doesn't hold is refused at once, and otherwise the pool decides. A
// signature that isn't the node key's is refused with pool.ErrWrongKey,
// once the pool has written the refusal into history, and leaves any
// connection the node holds alone.
func (h *Hub) hello(l *link, hello wire.Hello, nonce []byte, hubID string) {
	n, ok := h.nodeOf(hello.Node)
	if !ok {
		h.log.Warn("refused a connection for a node the Hub doesn't hold", "node", secret.Scrub(hello.Node))
		l.verdict <- mustEncode(wire.Refused{Reason: wire.ErrUnknownNode})
		return
	}
	if !wire.Verify(n.Key, nonce, hello.Node, hubID, hello.Signature) {
		h.applyThen(pool.KeyRefused{Node: hello.Node}, func([]hubstore.Effect) {
			h.log.Warn("refused a connection that didn't prove itself with the node's key", "node", l.node)
			l.verdict <- mustEncode(wire.Refused{Reason: pool.ErrWrongKey})
		})
		return
	}
	h.applyThen(pool.Connected{Node: hello.Node, Protocol: hello.Protocol, Platform: hello.Platform}, func(effects []hubstore.Effect) {
		for _, eff := range effects {
			switch eff := eff.(type) {
			case pool.AnswerRefused:
				if eff.Node == l.node {
					h.log.Warn("refused a node's connection", "node", l.node, "reason", eff.Reason)
					l.verdict <- mustEncode(wire.Refused{Reason: eff.Reason})
					return
				}
			case pool.AnswerRemoved:
				if eff.Node == l.node {
					h.log.Info("answered a removed node's connection", "node", l.node, "lost_runners", eff.Lost)
					l.verdict <- mustEncode(wire.Removed{Lost: eff.Lost})
					return
				}
			}
		}
		if old := h.links[l.node]; old != nil {
			// The newer connection replaces it, and the pool has already
			// counted the replacement: it isn't a disconnection.
			h.log.Info("a node's newer connection replaced its older one", "node", l.node)
			old.conn.Close()
		}
		h.links[l.node] = l
		l.conn.Send(mustEncode(wire.Accepted{}))
		l.verdict <- nil
		if !slices.Contains(runnerapp.Platforms, hello.Platform) {
			// The one place what it stated appears, scrubbed: the pool keeps
			// it as unknown, and every surface shows it so.
			h.log.Warn("a node stated a platform Silkpool doesn't support, so the Hub shows it as unknown and mints it no runners",
				"node", l.node, "platform", secret.Scrub(string(hello.Platform)))
		}
	})
}

// received applies a message from a node's current connection.
// Messages from a connection that has since been replaced or has ended
// are dropped.
func (h *Hub) received(l *link, m any) {
	if h.links[l.node] != l {
		return
	}
	id := l.node
	switch m := m.(type) {
	case node.Node:
		h.apply(pool.Reported{Node: id, Report: m})
	case node.Reply:
		h.apply(pool.Replied{Node: id, Reply: m})
	case node.Deliver:
		h.apply(pool.Delivered{Node: id, Entries: m.Entries})
	case node.RequestRunner:
		n, _ := h.nodeOf(id)
		h.apply(registration.RunnerRequested{Node: id, NodeName: n.Name, Slot: m.Slot, Target: n.Target, Random: random()})
	case node.RetireRunner:
		h.apply(registration.RetireRequested{Node: id, Runner: m.Runner})
	case node.RemoveAtHub:
		h.apply(pool.Remove{Node: id, ID: m.ID, Origin: m.Origin})
	case node.ConfirmRemoved:
		h.apply(pool.Confirmed{Node: id})
		if n, _ := h.nodeOf(id); !n.Connected {
			// The pool tombstoned it, which ended its connection.
			delete(h.links, id)
			l.conn.Close()
		}
	case wire.DiagnosticsReport:
		h.answered(l, m)
	default:
		h.log.Warn("rejected a message from a node out of turn", "node", id, "message", fmt.Sprintf("%T", m))
	}
}

// ended applies the end of a node's current connection, with when it
// last heard the node, pings included, which becomes the node's last
// seen. A replaced connection's end changes nothing, but that no ask out
// on it will be answered.
func (h *Hub) ended(l *link) {
	h.unasked(l)
	if h.links[l.node] != l {
		return
	}
	delete(h.links, l.node)
	h.apply(pool.Disconnected{Node: l.node, Heard: l.conn.Heard()})
}

// send sends a node a message, if it's connected.
func (h *Hub) send(id string, m any) {
	l := h.links[id]
	if l == nil {
		h.log.Warn("dropped a message for a node that isn't connected", "node", id, "message", fmt.Sprintf("%T", m))
		return
	}
	b, err := wire.EncodeToAgent(m)
	if err != nil {
		h.log.Error("couldn't encode a message for a node", "node", id, "err", err)
		return
	}
	if !l.conn.Send(b) {
		h.log.Warn("a node's connection couldn't take a message", "node", id, "message", fmt.Sprintf("%T", m))
	}
}

// serveEnroll answers one enrollment.
func (h *Hub) serveEnroll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := h.requestContext(r)
	defer cancel()
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, enrollLimit))
	if err != nil {
		http.Error(w, "the enrollment is too large", http.StatusRequestEntityTooLarge)
		return
	}
	e, err := wire.UnmarshalEnroll(b)
	if err == nil {
		err = checkKey(e.Key)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var answer *wire.Answer
	if !h.query(ctx, func() { answer = h.enroll(e) }) || answer == nil {
		// The Hub stopped before it answered, as a crash would: the
		// machine retries with the same code.
		panic(http.ErrAbortHandler)
	}
	// What the machine says of itself, and the name it asks for, are its
	// words, as is a refusal that quotes them.
	m := e.Machine
	if answer.Refused != nil {
		h.log.Info("refused an enrollment", "name", secret.Scrub(e.Name), "hostname", secret.Scrub(m.Hostname), "reason", scrubbed(answer.Refused))
	} else {
		h.log.Info("enrolled a machine", "node", answer.Node, "name", answer.Name, "hostname", secret.Scrub(m.Hostname),
			"os", secret.Scrub(m.OS), "os_version", secret.Scrub(m.OSVersion), "arch", secret.Scrub(m.Arch), "agent", secret.Scrub(m.AgentVersion))
	}
	body, err := wire.MarshalAnswer(*answer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// enroll applies an enrollment, and returns its answer, or nil if the
// Hub stopped first.
func (h *Hub) enroll(e wire.Enroll) *wire.Answer {
	en := &enrollment{}
	h.enrolling = en
	h.apply(pool.Enroll{Hash: wire.Hash(e.Secret), Node: newID(), Name: e.Name, Key: e.Key, Protocol: e.Protocol, Origin: e.Origin})
	h.enrolling = nil
	for _, eff := range en.effects {
		switch eff := eff.(type) {
		case pool.Enrolled:
			return &wire.Answer{Node: eff.Node, Name: eff.Name, Target: eff.Target, Hub: h.store.Hub().Settings.HubID}
		case pool.EnrollRefused:
			return &wire.Answer{Refused: eff.Reason}
		}
	}
	return nil
}

// checkKey refuses a public key that isn't an Ed25519 key in standard
// base64, so no node is saved with a key it can't connect with.
func checkKey(key string) error {
	b, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return errors.New("the node key isn't an Ed25519 public key")
	}
	return nil
}

// scrubbed is err's text with every secret in it scrubbed (package
// secret), for the Hub's log: an error on an agent's connection can
// quote what the peer or the network sent, such as a header of its
// upgrade, a message's type or the reason its close gave.
func scrubbed(err error) string { return secret.Scrub(err.Error()) }

// mustEncode encodes a message of the handshake, which can't fail.
func mustEncode(m any) []byte {
	b, err := wire.EncodeToAgent(m)
	if err != nil {
		panic(err)
	}
	return b
}

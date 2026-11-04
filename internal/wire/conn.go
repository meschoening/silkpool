package wire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// queueSize is how many messages a Conn holds for its peer. A peer that
// falls that far behind is cut off, and gets a full snapshot when it
// connects again.
const queueSize = 1024

// ErrSlowPeer ends a connection whose peer stopped reading.
var ErrSlowPeer = errors.New("the other end stopped reading")

// ErrClosed ends a connection that this end closed.
var ErrClosed = errors.New("the connection was closed")

// A Conn is one end of a connection once its handshake is done. It
// sends queued messages in order, sends a Ping every PingEvery, and
// counts the connection as dead once it has heard nothing for DeadAfter.
type Conn struct {
	ws      *websocket.Conn
	out     chan []byte
	closed  chan struct{}
	once    sync.Once
	ending  atomic.Bool
	reason  atomic.Pointer[error]
	started time.Time
	heard   atomic.Int64
}

// NewConn returns the Conn of ws, whose handshake is done. Messages from
// the peer may be up to limit bytes long.
func NewConn(ws *websocket.Conn, limit int64) *Conn {
	ws.SetReadLimit(limit)
	c := &Conn{ws: ws, out: make(chan []byte, queueSize), closed: make(chan struct{}), started: time.Now()}
	c.heard.Store(c.started.UnixNano())
	return c
}

// Started returns when the connection's handshake ended.
func (c *Conn) Started() time.Time { return c.started }

// Heard returns when the connection last heard its peer, pings
// included.
func (c *Conn) Heard() time.Time { return time.Unix(0, c.heard.Load()) }

// Send queues a message for the peer, and reports whether it was
// queued. Nothing is queued once the connection has ended or is ending,
// and a full queue ends it with ErrSlowPeer.
func (c *Conn) Send(message []byte) bool {
	if c.ending.Load() {
		return false
	}
	select {
	case <-c.closed:
		return false
	case c.out <- message:
		return true
	default:
		c.end(ErrSlowPeer)
		return false
	}
}

// Close ends the connection at once, dropping whatever is still queued.
func (c *Conn) Close() { c.end(ErrClosed) }

// Finish ends the connection once every message already queued has been
// sent. Send queues nothing more.
func (c *Conn) Finish() {
	if c.ending.Swap(true) {
		return
	}
	select {
	case c.out <- nil:
	default:
		c.end(ErrSlowPeer)
	}
}

func (c *Conn) end(err error) {
	c.once.Do(func() {
		c.reason.Store(&err)
		close(c.closed)
		c.ws.CloseNow()
	})
}

// Serve runs the connection until it ends, handing received each
// message from the peer but pings, in order. It returns why the
// connection ended: ctx ended, Close or Finish was called, the peer went
// away, or DeadAfter passed in silence.
func (c *Conn) Serve(ctx context.Context, received func([]byte)) error {
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() { c.write(ctx, stop) })
	wg.Go(func() { c.ping(stop) })
	err := c.read(ctx, received)
	c.end(err)
	close(stop)
	wg.Wait()
	return *c.reason.Load()
}

func (c *Conn) read(ctx context.Context, received func([]byte)) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, DeadAfter)
		_, b, err := c.ws.Read(readCtx)
		cancel()
		select {
		case <-c.closed:
			return *c.reason.Load()
		default:
		}
		if err != nil {
			if errors.Is(readCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return fmt.Errorf("heard nothing for %s", DeadAfter)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		c.heard.Store(time.Now().UnixNano())
		var env envelope
		if json.Unmarshal(b, &env) == nil && env.Type == typePing {
			continue
		}
		received(b)
	}
}

func (c *Conn) write(ctx context.Context, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case m := <-c.out:
			if m == nil {
				// Finish: everything before it has been sent.
				c.end(ErrClosed)
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, DeadAfter)
			err := c.ws.Write(writeCtx, websocket.MessageText, m)
			cancel()
			if err != nil {
				c.end(err)
				return
			}
		}
	}
}

func (c *Conn) ping(stop <-chan struct{}) {
	ping, _ := EncodeToHub(Ping{})
	t := time.NewTicker(PingEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.Send(ping)
		}
	}
}

// Read reads one message during the handshake, waiting at most
// HandshakeWithin.
func Read(ctx context.Context, ws *websocket.Conn) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, HandshakeWithin)
	defer cancel()
	_, b, err := ws.Read(ctx)
	return b, err
}

// Write writes one message during the handshake, waiting at most
// HandshakeWithin.
func Write(ctx context.Context, ws *websocket.Conn, message []byte) error {
	ctx, cancel := context.WithTimeout(ctx, HandshakeWithin)
	defer cancel()
	return ws.Write(ctx, websocket.MessageText, message)
}

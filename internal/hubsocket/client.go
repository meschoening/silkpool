package hubsocket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"

	"github.com/meschoening/silkpool/internal/hubops"
)

// ErrNotRunning fails a request when nothing is listening at the
// socket: the Hub's service isn't running.
var ErrNotRunning = errors.New("the Hub's service isn't running on this machine")

// ErrNotRoot fails a request whose caller can't open the socket, which
// only root can.
var ErrNotRoot = errors.New("only root can use silkpool-hub")

// ErrDropped fails a request whose connection ended before its reply.
var ErrDropped = errors.New("the connection to the Hub dropped")

// NotServer fails a request when the process listening at the socket
// isn't running as the Hub's account.
type NotServer struct {
	Path string
	UID  int
}

func (e NotServer) Error() string {
	return fmt.Sprintf("something other than the Hub's service is listening at %s (uid %d)", e.Path, e.UID)
}

// A Reply is the Hub's reply to a request: Raw as it came, and the
// refusal or the failure it holds, if it's one.
type Reply struct {
	Raw     json.RawMessage
	Refused *hubops.Refusal
	Error   *Error
}

// Do sends req to the Hub at path and returns its reply. It accepts
// only a server running as serverUID, the Hub's account, and checks it
// before it sends anything.
func Do(ctx context.Context, path string, serverUID int, req Request) (Reply, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED):
		return Reply{}, ErrNotRunning
	case errors.Is(err, fs.ErrPermission):
		return Reply{}, ErrNotRoot
	case err != nil:
		return Reply{}, fmt.Errorf("couldn't reach the Hub at %s: %w", path, err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	uid, err := peerUID(c)
	if err != nil {
		return Reply{}, err
	}
	if uid != serverUID {
		return Reply{}, NotServer{Path: path, UID: uid}
	}
	b, err := json.Marshal(req)
	if err != nil {
		return Reply{}, err
	}
	// A refusal at connect comes before the server reads, so the write
	// can fail once it has hung up, with the refusal there to read.
	_, writeErr := c.Write(append(b, '\n'))
	r := bufio.NewScanner(c)
	r.Buffer(make([]byte, 0, 64<<10), 16<<20)
	if !r.Scan() {
		switch {
		case ctx.Err() != nil:
			return Reply{}, ctx.Err()
		case writeErr != nil:
			return Reply{}, fmt.Errorf("%w: %w", ErrDropped, writeErr)
		}
		return Reply{}, fmt.Errorf("%w before it replied: %v", ErrDropped, r.Err())
	}
	out := Reply{Raw: json.RawMessage(append([]byte(nil), r.Bytes()...))}
	var envelope struct {
		Refused *hubops.Refusal `json:"refused"`
		Error   *Error          `json:"error"`
	}
	if err := json.Unmarshal(out.Raw, &envelope); err != nil {
		return Reply{}, fmt.Errorf("the Hub replied with something that isn't JSON: %w", err)
	}
	out.Refused, out.Error = envelope.Refused, envelope.Error
	return out, nil
}

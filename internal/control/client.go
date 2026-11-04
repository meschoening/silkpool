package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/store"
)

// A Client is one connection to the agent's control channel.
type Client struct {
	c     net.Conn
	r     *bufio.Scanner
	Hello Hello
}

// Hello is what the agent says when it admits a client: its version, and
// the account it saw, and whether that's uid 0.
type Hello struct {
	Version  string
	Account  string
	Elevated bool
}

// ErrNotRunning fails a dial when nothing is listening at the socket.
var ErrNotRunning = errors.New("silkpool's service isn't running on this machine")

// ErrDropped fails a request whose connection ended before its answer.
var ErrDropped = errors.New("the connection to the agent dropped")

// NotServer fails a dial when the process listening at the socket isn't
// running as the uid the agent runs as.
type NotServer struct {
	Path string
	UID  int
}

func (e NotServer) Error() string {
	return fmt.Sprintf("something other than Silkpool's service is listening at %s (uid %d)", e.Path, e.UID)
}

// NotAdmin fails a dial the agent refused because the caller isn't an
// administrator.
type NotAdmin struct{ Account string }

func (e NotAdmin) Error() string {
	return fmt.Sprintf("only administrators can use Silkpool on this machine, and %s isn't one", e.Account)
}

// Dial connects to the control channel at path. It accepts only a server
// running as serverUID, which is root's 0 outside tests, and checks it
// before it sends anything.
func Dial(ctx context.Context, path string, serverUID int) (*Client, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("couldn't reach Silkpool's service at %s: %w", path, err)
	}
	uid, err := peerUID(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	if uid != serverUID {
		c.Close()
		return nil, NotServer{Path: path, UID: uid}
	}
	client := &Client{c: c, r: newReader(c)}
	m, err := client.receive(ctx)
	switch {
	case err != nil:
	case m.Type == typeHello:
		client.Hello = Hello{Version: m.Version, Account: m.Account, Elevated: m.Elevated}
		return client, nil
	case m.Type == typeRefused:
		err = NotAdmin{Account: m.Account}
	case m.Error != nil:
		err = m.Error
	default:
		err = fmt.Errorf("control: the agent opened with %q", m.Type)
	}
	c.Close()
	return nil, err
}

// Close ends the connection.
func (c *Client) Close() error { return c.c.Close() }

// receive reads one message, ending the connection if ctx ends first.
func (c *Client) receive(ctx context.Context) (message, error) {
	stop := context.AfterFunc(ctx, func() { c.c.Close() })
	defer stop()
	m, err := read(c.r)
	if err != nil {
		if ctx.Err() != nil {
			return m, ctx.Err()
		}
		return m, fmt.Errorf("%w: %w", ErrDropped, err)
	}
	return m, nil
}

// do sends a request and reads its answer, which must be of type want.
func (c *Client) do(ctx context.Context, req message, want string) (message, error) {
	if err := write(c.c, req); err != nil {
		return message{}, fmt.Errorf("%w: %w", ErrDropped, err)
	}
	m, err := c.receive(ctx)
	switch {
	case err != nil:
		return m, err
	case m.Type == typeError && m.Error != nil:
		return m, m.Error
	case m.Type != want:
		return m, fmt.Errorf("control: the agent answered a %s with %q", req.Type, m.Type)
	}
	return m, nil
}

// Status returns the agent's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	m, err := c.do(ctx, message{Type: typeStatus}, typeStatus)
	if err != nil || m.Status == nil {
		return Status{}, orMissing(err, "status")
	}
	return *m.Status, nil
}

// Watch calls f with the status now and after every change, until ctx
// ends, which it returns nil for, or f fails.
func (c *Client) Watch(ctx context.Context, f func(Status) error) error {
	if err := write(c.c, message{Type: typeWatch}); err != nil {
		return fmt.Errorf("%w: %w", ErrDropped, err)
	}
	for {
		m, err := c.receive(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			return err
		case m.Type == typeError && m.Error != nil:
			return m.Error
		case m.Status == nil:
			return fmt.Errorf("control: the agent sent %q while watching", m.Type)
		}
		if err := f(*m.Status); err != nil {
			return err
		}
	}
}

// Command sends an availability command with its command ID.
func (c *Client) Command(ctx context.Context, id string, e availability.Event) (Reply, error) {
	ev, err := store.FromEvent(e)
	if err != nil {
		return Reply{}, err
	}
	return c.reply(ctx, message{Type: typeCommand, ID: id, Event: &ev})
}

// Dismiss dismisses the notices with IDs notices, under one command ID.
func (c *Client) Dismiss(ctx context.Context, id string, notices []int64) (Reply, error) {
	return c.reply(ctx, message{Type: typeDismiss, ID: id, Notices: notices})
}

// Enroll enrolls the machine with an enrollment code, asking for name,
// or for the Hub's choice if it's empty.
func (c *Client) Enroll(ctx context.Context, id, code, name string) (Reply, error) {
	return c.reply(ctx, message{Type: typeEnroll, ID: id, Code: code, Name: name})
}

// Remove starts the node's removal, through the Hub unless force.
func (c *Client) Remove(ctx context.Context, id string, force bool) (Reply, error) {
	return c.reply(ctx, message{Type: typeRemove, ID: id, Force: force})
}

// SetHubURL re-points the node at its Hub's new URL.
func (c *Client) SetHubURL(ctx context.Context, url string) error {
	_, err := c.do(ctx, message{Type: typeSetHubURL, URL: url}, typeDone)
	return err
}

// Diagnostics returns the agent's diagnostics report.
func (c *Client) Diagnostics(ctx context.Context) (diagnostics.Report, error) {
	m, err := c.do(ctx, message{Type: typeDiagnostics}, typeDiagnostics)
	if err != nil || m.Report == nil {
		return diagnostics.Report{}, orMissing(err, "report")
	}
	return *m.Report, nil
}

func (c *Client) reply(ctx context.Context, req message) (Reply, error) {
	m, err := c.do(ctx, req, typeReply)
	if err != nil || m.Reply == nil {
		return Reply{}, orMissing(err, "reply")
	}
	return *m.Reply, nil
}

func orMissing(err error, what string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("control: the agent's answer has no %s", what)
}

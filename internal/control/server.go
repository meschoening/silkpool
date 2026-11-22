package control

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/node"
)

// Agent is what the server asks of the agent service.
type Agent interface {
	// Status returns the agent's status now.
	Status(ctx context.Context) (Status, error)
	// Watch calls send with the status now and again after every change,
	// until ctx ends or send fails.
	Watch(ctx context.Context, send func(Status) error) error
	// Command, Dismiss and Remove apply a command from the machine
	// itself, and return its reply.
	Command(ctx context.Context, c node.Command) (node.Reply, error)
	Dismiss(ctx context.Context, d node.Dismiss) (node.Reply, error)
	Remove(ctx context.Context, r node.Remove) (node.Reply, error)
	// Enroll enrolls the machine, and returns the reply once the Hub has
	// answered.
	Enroll(ctx context.Context, e Enrollment) (node.Reply, error)
	// SetHubURL re-points the node at its Hub's new URL, once it has
	// checked that the Hub is there.
	SetHubURL(ctx context.Context, url string) error
	// Diagnostics returns the diagnostics report.
	Diagnostics(ctx context.Context) (diagnostics.Report, error)
}

// Enrollment is an administrator's enroll of the machine: the
// enrollment code as they gave it, and the name to ask for.
type Enrollment struct {
	ID     string
	Origin node.Origin
	Code   string
	Name   string
}

// A Server serves the control channel.
type Server struct {
	Agent Agent
	// Version is the agent's version, which the hello carries.
	Version string
	// Lookup returns the account a peer's uid names, or nil for
	// LookupAccount. Only tests set it. Windows's own peer check reads
	// the account from the caller's token, and doesn't use it.
	Lookup func(uid int) (Account, error)
	// PeerUID returns the uid of the process at the other end of a
	// connection, or is nil for the OS's peer check. Only tests set it,
	// to run the channel's protocol over a Unix socket on Windows, whose
	// own peer check is the named pipe's.
	PeerUID func(net.Conn) (int, error)
	// Log is where the server logs, or nil for slog.Default.
	Log *slog.Logger
}

// Listen listens at path, the control socket, in a folder of mode 0755,
// which it creates if it's missing. It replaces a socket a stopped agent
// left, and makes the socket mode 0666: the peer check decides who may
// use it.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("control: making the socket's folder: %w", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, fmt.Errorf("control: opening the socket's folder to every account: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("control: removing an old socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control: opening the socket to every account: %w", err)
	}
	return ln, nil
}

// Serve serves ln until ctx ends, and waits for its connections to end.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("control: %w", err)
		}
		conns.Go(func() { s.serve(ctx, c) })
	}
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// serve serves one connection: the peer check, then its requests. A
// client that hangs up ends whatever its request started waiting for,
// though not the command itself.
func (s *Server) serve(ctx context.Context, c net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.Close()
	lookup := s.Lookup
	if lookup == nil {
		lookup = LookupAccount
	}
	// The peer check comes before anything can close c, so it never reads
	// a handle that's been closed, or reused, under it. It ends with ctx
	// on its own.
	var account Account
	var err error
	if s.PeerUID != nil {
		account, err = identifyByUID(c, s.PeerUID, lookup)
	} else {
		account, err = identify(ctx, c, lookup)
	}
	if err != nil {
		s.log().Warn("couldn't tell who connected to the control channel", "err", err)
		var lookupErr lookupFailed
		if errors.As(err, &lookupErr) {
			_ = write(c, message{Type: typeError, Error: &Error{Message: fmt.Sprintf("the agent couldn't look up account %d", lookupErr.uid)}})
		}
		return
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if !account.Admin {
		_ = write(c, message{Type: typeRefused, Account: account.Name, Error: &Error{Code: CodeNotAdmin,
			Message: fmt.Sprintf("only administrators can use Silkpool on this machine, and %s isn't one", account.Name)}})
		return
	}
	if write(c, message{Type: typeHello, Version: s.Version, Account: account.Name, Elevated: account.Elevated}) != nil {
		return
	}
	peer := peer{origin: node.Origin{Surface: node.CommandLine, Account: account.Name}, elevated: account.Elevated}
	requests := make(chan message)
	go func() {
		defer cancel()
		r := newReader(c)
		for {
			m, err := read(r)
			if err != nil {
				return
			}
			select {
			case requests <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-requests:
			if m.Type == typeWatch {
				err := s.Agent.Watch(ctx, func(st Status) error { return write(c, message{Type: typeStatus, Status: &st}) })
				if err != nil && ctx.Err() == nil {
					_ = write(c, message{Type: typeError, Error: ErrorOf(err)})
				}
				return
			}
			if write(c, s.handle(ctx, m, peer)) != nil {
				return
			}
		}
	}
}

// A peer is who a connection's commands come from.
type peer struct {
	origin   node.Origin
	elevated bool
}

// elevatedOnly refuses a request that needs elevation from a peer that
// isn't elevated.
func elevatedOnly(what string, p peer) *Error {
	if p.elevated {
		return nil
	}
	return &Error{Code: CodeNotElevated, Message: what + " " + needsElevation}
}

// identifyByUID returns the account of the process at the other end of
// c, by its uid, which check reads, and lookup names.
func identifyByUID(c net.Conn, check func(net.Conn) (int, error), lookup func(int) (Account, error)) (Account, error) {
	uid, err := check(c)
	if err != nil {
		return Account{}, err
	}
	account, err := lookup(uid)
	if err != nil {
		return Account{}, lookupFailed{uid: uid, err: err}
	}
	return account, nil
}

// lookupFailed fails a peer check whose uid the account database
// couldn't name. The caller is told.
type lookupFailed struct {
	uid int
	err error
}

func (e lookupFailed) Error() string {
	return fmt.Sprintf("control: looking up account %d: %v", e.uid, e.err)
}

func (e lookupFailed) Unwrap() error { return e.err }

// handle answers one request.
func (s *Server) handle(ctx context.Context, m message, p peer) message {
	fail := func(err error) message { return message{Type: typeError, Error: ErrorOf(err)} }
	replied := func(r node.Reply, err error) message {
		if err != nil {
			return fail(err)
		}
		reply, err := ReplyOf(r)
		if err != nil {
			return fail(err)
		}
		return message{Type: typeReply, Reply: &reply}
	}
	origin := p.origin
	switch m.Surface {
	case "":
	case surfaceTray:
		origin.Surface = node.Tray
	default:
		return fail(&Error{Code: CodeBadRequest, Message: fmt.Sprintf("%q isn't a surface a request can come from", m.Surface)})
	}
	switch m.Type {
	case typeStatus:
		st, err := s.Agent.Status(ctx)
		if err != nil {
			return fail(err)
		}
		return message{Type: typeStatus, Status: &st}
	case typeCommand:
		if m.Event == nil {
			return fail(&Error{Code: CodeBadRequest, Message: "a command without its event"})
		}
		ev, err := m.Event.ToEvent()
		if err != nil {
			return fail(&Error{Code: CodeBadRequest, Message: err.Error()})
		}
		return replied(s.Agent.Command(ctx, node.Command{ID: m.ID, Origin: origin, Event: ev}))
	case typeDismiss:
		return s.dismiss(ctx, m, origin, replied)
	case typeEnroll:
		if e := elevatedOnly("enroll", p); e != nil {
			return fail(e)
		}
		return replied(s.Agent.Enroll(ctx, Enrollment{ID: m.ID, Origin: origin, Code: m.Code, Name: m.Name}))
	case typeRemove:
		if e := elevatedOnly("remove", p); e != nil {
			return fail(e)
		}
		return replied(s.Agent.Remove(ctx, node.Remove{ID: m.ID, Origin: origin, Local: true, Force: m.Force}))
	case typeSetHubURL:
		if e := elevatedOnly("set-hub-url", p); e != nil {
			return fail(e)
		}
		if err := s.Agent.SetHubURL(ctx, m.URL); err != nil {
			return fail(err)
		}
		return message{Type: typeDone}
	case typeDiagnostics:
		r, err := s.Agent.Diagnostics(ctx)
		if err != nil {
			return fail(err)
		}
		return message{Type: typeDiagnostics, Report: &r}
	}
	return fail(&Error{Code: CodeBadRequest, Message: fmt.Sprintf("%q isn't a request the agent takes", m.Type)})
}

// dismiss dismisses each notice the request names, as one command each
// from origin, and answers with the first refusal, or the last reply.
func (s *Server) dismiss(ctx context.Context, m message, origin node.Origin, replied func(node.Reply, error) message) message {
	if len(m.Notices) == 0 {
		return message{Type: typeError, Error: &Error{Code: CodeBadRequest, Message: "a dismissal that names no notice"}}
	}
	var last node.Reply
	for _, id := range m.Notices {
		r, err := s.Agent.Dismiss(ctx, node.Dismiss{ID: fmt.Sprintf("%s/%d", m.ID, id), Origin: origin, Notice: id})
		if err != nil || r.Refused != nil {
			return replied(r, err)
		}
		last = r
	}
	return replied(last, nil)
}

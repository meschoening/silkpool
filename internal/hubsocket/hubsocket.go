// Package hubsocket is the Hub's local socket, through which the Hub's
// CLI, silkpool-hub, asks the running Hub to carry out the operations of
// package hubops. The decisions behind it are in "Name the CLI and
// define its commands"
// and "Define what the Hub and the agent persist".
//
// The socket admits only root: the server reads each caller's uid from
// the socket's peer credentials, and refuses any but 0 before reading
// its request. The socket's file is also its owner's alone, the Hub's
// account, which root reaches anyway. Each change's origin is the Hub's
// CLI and the caller's account.
//
// Each connection carries one request, a line of JSON, and one reply,
// the operation's reply as JSON, which --json prints as it is. A refusal
// is {"refused": {...}}, and a failure {"error": {...}}.
//
// The client checks the server the same way: it accepts only one running
// as the Hub's account, so a squatted socket can't pose as the Hub.
package hubsocket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"

	"github.com/meschoening/silkpool/internal/hub"
	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/node"
)

// Path is the socket on Linux, in the Hub's runtime folder, which systemd
// makes for its unit.
const Path = "/run/silkpool-hub/hub.sock"

// maxRequest is the longest request the server reads.
const maxRequest = 64 << 10

// A Request is one operation the CLI asks for, with its arguments. The
// fields an operation doesn't take are empty.
type Request struct {
	// Op names the operation, such as "node.rename".
	Op string `json:"op"`
	// Version is the CLI's version, which must be the Hub's, except for
	// "version".
	Version string `json:"version"`
	// ID is a code's or a node's ID, or a removal's command ID.
	ID       string   `json:"id,omitempty"`
	Name     string   `json:"name,omitempty"`
	To       string   `json:"to,omitempty"`
	Target   string   `json:"target,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Label    string   `json:"label,omitempty"`
	URL      string   `json:"url,omitempty"`
	Reassign bool     `json:"reassign,omitempty"`
}

// An Error is a failure on the socket: a stable code, and its message.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// The codes of the socket's own failures.
const (
	// CodeNotRoot refuses a caller who isn't root, before its request.
	CodeNotRoot = "not-root"
	// CodeVersion refuses a CLI of another version than the Hub's.
	CodeVersion = "version"
	// CodeBadRequest refuses a request the Hub can't read.
	CodeBadRequest = "bad-request"
	// CodeStopped fails a request because the Hub is stopping.
	CodeStopped = "stopped"
	// CodeFailed fails an operation the Hub couldn't carry out, such as
	// one whose GitHub read failed.
	CodeFailed = "failed"
)

// A Server serves the socket.
type Server struct {
	Ops *hubops.Ops
	// Version is the Hub's version.
	Version string
	// Admit reports whether a caller's uid may use the socket, or is nil
	// to admit only root. Only tests set it.
	Admit func(uid int) bool
	// Log is where the server logs, or nil for slog.Default.
	Log *slog.Logger
}

// Listen listens at path, in a folder that must exist, replacing a
// socket a stopped Hub left, and makes the socket its owner's alone.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("hubsocket: removing an old socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("hubsocket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("hubsocket: %w", err)
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
			return fmt.Errorf("hubsocket: %w", err)
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

// serve serves one connection: the peer check, then one request and its
// reply.
func (s *Server) serve(ctx context.Context, c net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	reply := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			b, _ = json.Marshal(failure{&Error{Code: CodeFailed, Message: err.Error()}})
		}
		_, _ = c.Write(append(b, '\n'))
	}
	uid, err := peerUID(c)
	if err != nil {
		s.log().Warn("couldn't tell who connected to the Hub's socket", "err", err)
		return
	}
	admit := s.Admit
	if admit == nil {
		admit = func(uid int) bool { return uid == 0 }
	}
	if !admit(uid) {
		s.log().Warn("refused the Hub's socket to an account that isn't root", "uid", uid)
		reply(failure{&Error{Code: CodeNotRoot, Message: "only root can use silkpool-hub: run it with sudo"}})
		return
	}
	r := bufio.NewScanner(c)
	r.Buffer(make([]byte, 0, 4<<10), maxRequest)
	if !r.Scan() {
		return
	}
	var req Request
	if err := json.Unmarshal(r.Bytes(), &req); err != nil {
		reply(failure{&Error{Code: CodeBadRequest, Message: "a request that isn't one: " + err.Error()}})
		return
	}
	if req.Version != s.Version && req.Op != "version" {
		reply(failure{&Error{Code: CodeVersion, Message: fmt.Sprintf("this CLI is %s but the running Hub is %s", req.Version, s.Version)}})
		return
	}
	from := node.Origin{Surface: node.HubCommandLine, Account: account(uid)}
	v, err := s.do(ctx, from, req)
	var refusal *hubops.Refusal
	var e *Error
	switch {
	case errors.As(err, &refusal):
		reply(refused{refusal})
	case errors.As(err, &e):
		reply(failure{e})
	case errors.Is(err, hub.ErrStopped):
		reply(failure{&Error{Code: CodeStopped, Message: "the Hub is stopping"}})
	case err != nil:
		s.log().Warn("the Hub's CLI asked for an operation that failed", "op", req.Op, "err", err)
		reply(failure{&Error{Code: CodeFailed, Message: err.Error()}})
	default:
		reply(v)
	}
}

type refused struct {
	Refused *hubops.Refusal `json:"refused"`
}

type failure struct {
	Error *Error `json:"error"`
}

// do carries out req's operation from origin, and returns its reply.
func (s *Server) do(ctx context.Context, from node.Origin, req Request) (any, error) {
	o := s.Ops
	switch req.Op {
	case "status":
		return o.Status(ctx)
	case "version":
		return o.Version(ctx)
	case "code.new":
		return o.MakeCode(ctx, from)
	case "code.list":
		codes, err := o.Codes(ctx)
		return struct {
			Codes []hubops.Code `json:"codes"`
		}{codes}, err
	case "code.revoke":
		return o.RevokeCode(ctx, from, req.ID)
	case "node.list":
		nodes, err := o.Nodes(ctx)
		return struct {
			Nodes []hubops.Node `json:"nodes"`
		}{nodes}, err
	case "node.get":
		n, err := o.Node(ctx, req.ID)
		return struct {
			Node hubops.Node `json:"node"`
		}{n}, err
	case "node.rename":
		return o.Rename(ctx, from, req.Name, req.To)
	case "node.set-target":
		return o.SetTarget(ctx, from, req.Name, req.Target)
	case "node.label.add":
		return o.AddLabels(ctx, from, req.Name, req.Labels)
	case "node.label.remove":
		return o.RemoveLabels(ctx, from, req.Name, req.Labels)
	case "node.remove":
		return o.RemoveNode(ctx, from, req.Name, req.ID)
	case "target.list":
		return o.Targets(ctx)
	case "target.available":
		return o.Available(ctx)
	case "target.add":
		return o.AddTarget(ctx, from, req.Target)
	case "target.remove":
		return o.RemoveTarget(ctx, from, req.Target)
	case "target.default":
		return o.SetDefaultTarget(ctx, from, req.Target)
	case "pool-label":
		return o.PoolLabel(ctx)
	case "pool-label.set":
		return o.SetPoolLabel(ctx, from, req.Label)
	case "setup-code":
		return o.SetupCode(ctx, from, req.Reassign)
	case "set-url":
		return o.SetURL(ctx, from, req.URL)
	}
	return nil, &Error{Code: CodeBadRequest, Message: fmt.Sprintf("%q isn't an operation the Hub has", req.Op)}
}

// account returns the name of the account uid names, or the uid if it
// has none.
func account(uid int) string {
	id := strconv.Itoa(uid)
	if u, err := user.LookupId(id); err == nil {
		return u.Username
	}
	return id
}

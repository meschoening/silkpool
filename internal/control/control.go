// Package control is the agent's local control channel: the Unix socket
// that the node CLI, and later the tray app, use to ask the agent
// service for its status and to send it commands. The decisions behind
// it are in "Define how the agent runs and launches slots on each OS",
// recorded as ADR 0008, and "Name the CLI and define its commands".
//
// The socket is open to every local account at the OS layer. The
// server identifies each caller by the socket's peer credentials, and
// admits only OS administrators: root, and members of the AdminGroups.
// Everyone else is refused at connect, with no read-only status, because
// status names private repositories and jobs. Enroll, remove and
// set-hub-url also need uid 0. Each command's origin is the node's
// command line and the account the peer check saw.
//
// Clients check the server the same way: they accept only a server
// running as root, so a squatted socket can't impersonate the agent.
//
// Each message is one line of JSON. The server speaks first, with a
// hello or a refusal. Then the client sends requests, one at a time,
// and the server answers each with one message, except a watch, which
// it answers with a status after every change until the client hangs
// up.
package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/meschoening/silkpool/internal/diagnostics"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/wire"
)

// Path is the control socket on Linux. Its folder is root's, mode 0755,
// so only root can create files in it, and the socket is mode 0666.
const Path = "/run/silkpool/agent.sock"

// AdminGroups are the Linux groups whose members are administrators,
// as polkit's distribution defaults have it.
var AdminGroups = []string{"sudo", "wheel", "admin"}

// maxLine is the longest message either end reads.
const maxLine = 4 << 20

// The message types.
const (
	typeHello       = "hello"
	typeRefused     = "refused"
	typeStatus      = "status"
	typeWatch       = "watch"
	typeCommand     = "command"
	typeDismiss     = "dismiss"
	typeEnroll      = "enroll"
	typeRemove      = "remove"
	typeSetHubURL   = "set-hub-url"
	typeDiagnostics = "diagnostics"
	typeReply       = "reply"
	typeDone        = "done"
	typeError       = "error"
)

// A message is any message either way, naming its type, with the fields
// of that type.
type message struct {
	Type string `json:"type"`
	// A hello's: the agent's version, the account the peer check saw,
	// and whether it's uid 0. A refusal names the account too.
	Version  string `json:"version,omitempty"`
	Account  string `json:"account,omitempty"`
	Elevated bool   `json:"elevated,omitempty"`
	// A request's.
	ID      string       `json:"id,omitempty"`
	Event   *store.Event `json:"event,omitempty"`
	Notices []int64      `json:"notices,omitempty"`
	Code    string       `json:"code,omitempty"`
	Name    string       `json:"name,omitempty"`
	Force   bool         `json:"force,omitempty"`
	URL     string       `json:"url,omitempty"`
	// An answer's.
	Status *Status             `json:"status,omitempty"`
	Reply  *Reply              `json:"reply,omitempty"`
	Report *diagnostics.Report `json:"report,omitempty"`
	Error  *Error              `json:"error,omitempty"`
}

// Status is the agent's status as the CLI and the tray read it, and the
// `status --json` contract, which changes only by adding fields.
type Status struct {
	// Version is the running agent's version.
	Version string       `json:"version"`
	Machine wire.Machine `json:"machine"`
	// Now is the agent's clock when it answered.
	Now string `json:"now"`
	// Node is the node, without its outbox and kept replies.
	Node store.Node `json:"node"`
	Hub  Hub        `json:"hub,omitzero"`
}

// Hub is the Hub the node is enrolled in, as the agent sees it.
type Hub struct {
	URL string `json:"url,omitempty"`
	// Node is the node's ID at the Hub.
	Node string `json:"node,omitempty"`
	// Since is when the node last connected to the Hub or lost its
	// connection, or when the agent started if it hasn't connected since.
	Since string `json:"since,omitempty"`
}

// StatusOf returns the status of n, without its outbox and kept replies.
func StatusOf(version string, m wire.Machine, now string, n node.Node, hub Hub) (Status, error) {
	n.Outbox, n.Replies = nil, nil
	stored, err := store.FromNode(n)
	if err != nil {
		return Status{}, err
	}
	return Status{Version: version, Machine: m, Now: now, Node: stored, Hub: hub}, nil
}

// Reply is a node.Reply's form on the channel.
type Reply struct {
	ID       string             `json:"id"`
	At       string             `json:"at"`
	Refused  *Error             `json:"refused,omitempty"`
	Settings store.Availability `json:"settings"`
}

// ReplyOf returns r's form on the channel.
func ReplyOf(r node.Reply) (Reply, error) {
	s, err := store.FromReply(r)
	if err != nil {
		return Reply{}, err
	}
	return Reply{ID: s.ID, At: s.At, Refused: ErrorOf(r.Refused), Settings: s.Settings}, nil
}

// Error is a refusal or a failure on the channel: a stable code for each
// one a client acts on, and its message.
type Error struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	// Suggestion is the name to enroll with instead, for name-taken.
	Suggestion string `json:"suggestion,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// The channel's own codes.
const (
	// CodeNotAdmin refuses a caller who isn't an administrator, at
	// connect.
	CodeNotAdmin = "not-admin"
	// CodeNotElevated refuses enroll, remove and set-hub-url from a
	// caller who isn't uid 0.
	CodeNotElevated = "not-elevated"
	// CodeBadRequest refuses a request the server can't read.
	CodeBadRequest = "bad-request"
	// CodeStopped fails a request because the agent is stopping.
	CodeStopped = "stopped"
	// CodeNameTaken refuses an enroll whose name the Hub holds, with a
	// Suggestion.
	CodeNameTaken = "name-taken"
	// CodeUnreachable fails an enroll whose Hub couldn't be reached.
	CodeUnreachable = "unreachable"
	// CodeAnswerLost fails an enroll whose answer was lost after it went
	// out to the Hub, and again on the agent's one retry. The Hub may
	// have enrolled the machine, and enrolling again with the same code
	// is safe.
	CodeAnswerLost = "answer-lost"
)

// enrollCodes are the codes of the enrollment's refusals.
var enrollCodes = []struct {
	code string
	err  error
}{
	{"not-a-code", wire.ErrNotACode},
	{"unknown-code", pool.ErrUnknownCode},
	{"code-used", pool.ErrCodeUsed},
	{"code-revoked", pool.ErrCodeRevoked},
	{"code-expired", pool.ErrCodeExpired},
}

// ErrorOf returns err's form on the channel, or nil for nil.
func ErrorOf(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var taken pool.NameTaken
	var invalid node.InvalidName
	var version pool.VersionMismatch
	switch {
	case errors.As(err, &taken):
		return &Error{Code: CodeNameTaken, Message: err.Error(), Suggestion: taken.Suggestion}
	case errors.As(err, &invalid):
		return &Error{Code: "invalid-name", Message: err.Error()}
	case errors.As(err, &version):
		return &Error{Code: "version", Message: err.Error()}
	}
	for _, c := range enrollCodes {
		if errors.Is(err, c.err) {
			return &Error{Code: c.code, Message: err.Error()}
		}
	}
	s := store.FromError(err)
	return &Error{Code: s.Code, Message: s.Message}
}

// write sends m as one line.
func write(w io.Writer, m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// newReader returns a reader of messages from r.
func newReader(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), maxLine)
	return s
}

// read reads one message, or returns io.EOF once there are no more.
func read(s *bufio.Scanner) (message, error) {
	if !s.Scan() {
		if err := s.Err(); err != nil {
			return message{}, err
		}
		return message{}, io.EOF
	}
	var m message
	if err := json.Unmarshal(s.Bytes(), &m); err != nil {
		return message{}, fmt.Errorf("control: a message that isn't one: %w", err)
	}
	return m, nil
}

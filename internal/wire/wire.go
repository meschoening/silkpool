// Package wire is the Hub-agent protocol: the messages the Hub and an
// agent exchange over the one WebSocket the agent dials, the enrollment
// endpoint beside it, the enrollment code, and how each end proves who
// it is. The decisions behind it are in "Define node enrollment,
// removal, identity and Hub-agent transport",
// recorded as ADR 0004.
//
// Every message is a JSON object naming its type, with its body beside
// it. Each direction has its own types, one for each thing the
// transport decision allows, and a message of any other type is refused
// with UnknownType: the Hub has no message that runs a command, reads a
// file or changes the agent's configuration, so it can't ask for one.
// The cores' values travel in package store's stable forms, the same
// ones each end saves.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/store"
)

// Protocol is the version of the Hub-agent protocol this build speaks.
// Each end states it when it connects, and the Hub accepts agents at its
// own version and the one before.
const Protocol = 1

// The paths on the Hub's base URL where agents reach it.
const (
	// ConnectPath is the WebSocket the agent dials and keeps.
	ConnectPath = "/agent/connect"
	// EnrollPath is the enrollment endpoint, a POST of an Enroll.
	EnrollPath = "/agent/enroll"
)

// The keepalive: each end sends a ping every PingEvery, and counts the
// connection as dead once it has heard nothing for DeadAfter.
const (
	PingEvery = 20 * time.Second
	DeadAfter = 60 * time.Second
)

// The handshake, which comes before anything else on a connection:
//
//  1. The Hub sends a Challenge.
//  2. The agent answers with a Hello, signed with its node key.
//  3. The Hub answers with Accepted, Refused or Removed. After Refused or
//     Removed it ends the connection.
//
// Each step must come within HandshakeWithin.
const HandshakeWithin = 30 * time.Second

// Challenge is the Hub's first message on a connection.
type Challenge struct {
	// Protocol is the Hub's protocol version.
	Protocol int
	// Hub is the Hub's ID.
	Hub string
	// Nonce is NonceSize random bytes, never sent before.
	Nonce []byte
}

// Hello is the agent's answer to the Challenge: its protocol version,
// its node ID and platform, and its signature of the challenge, made by
// Sign.
type Hello struct {
	Protocol  int
	Node      string
	Platform  runnerapp.Platform
	Signature []byte
}

// Accepted tells the agent the Hub accepted its connection. Messages of
// every other kind follow.
type Accepted struct{}

// Refused tells the agent the Hub refused its connection, naming the
// rule: a pool.VersionMismatch, pool.ErrWrongKey, or ErrUnknownNode.
type Refused struct{ Reason error }

// Removed tells the agent the Hub has removed its node, which it
// applies as node.Removed. Lost names the node's runners in lost
// targets, which the Hub can't delete, for the node to stop itself.
//
// It travels in the body, which agents before it never read, so one a
// protocol version behind ignores it and retires those runners through
// the Hub as before. A Removed naming none is sent without a body, as
// those agents' Hubs sent it, and reads as naming none.
type Removed struct{ Lost []string }

// Ping is the keepalive. Conn sends and swallows it.
type Ping struct{}

// Granted is the Hub's grant of a runner to a slot, which the agent
// applies as node.Granted, with the runner's JIT config, which the agent
// keeps in memory only and hands to the runner.
type Granted struct {
	Slot      int
	Runner    string
	JITConfig secret.Value
}

// ErrUnknownNode refuses a connection that names a node the Hub doesn't
// hold. One that names a node the Hub holds, but whose signature isn't
// that node key's, is refused with pool.ErrWrongKey.
var ErrUnknownNode = errors.New("the Hub doesn't know this node")

// UnknownType refuses a message of a type its direction doesn't have.
type UnknownType struct{ Type string }

func (e UnknownType) Error() string {
	return fmt.Sprintf("%q isn't a message type this end accepts", e.Type)
}

// The message types, by direction.
const (
	// The Hub's.
	typeChallenge    = "challenge"
	typeAccepted     = "accepted"
	typeRefused      = "refused"
	typeRemoved      = "removed"
	typeCommand      = "command"
	typeDismiss      = "dismiss"
	typeRemove       = "remove"
	typeSettings     = "settings"
	typeGranted      = "granted"
	typeGrantFailed  = "grant-failed"
	typeRetired      = "retired"
	typeStopLost     = "stop-lost-runners"
	typeAcknowledged = "acknowledged"

	// The agent's.
	typeHello          = "hello"
	typeReport         = "report"
	typeReply          = "reply"
	typeDeliver        = "deliver"
	typeRequestRunner  = "request-runner"
	typeRetireRunner   = "retire-runner"
	typeRemoveAtHub    = "remove-at-hub"
	typeConfirmRemoved = "confirm-removed"

	// Both.
	typePing = "ping"
)

type envelope struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

type challengeBody struct {
	Protocol int    `json:"protocol"`
	Hub      string `json:"hub"`
	Nonce    []byte `json:"nonce"`
}

type helloBody struct {
	Protocol  int    `json:"protocol"`
	Node      string `json:"node"`
	Platform  string `json:"platform"`
	Signature []byte `json:"signature"`
}

type commandBody struct {
	ID     string       `json:"id"`
	Origin store.Origin `json:"origin,omitzero"`
	Event  store.Event  `json:"event"`
}

type dismissBody struct {
	ID     string       `json:"id"`
	Origin store.Origin `json:"origin,omitzero"`
	Notice int64        `json:"notice"`
}

// removeBody is the Hub's Remove, and the agent's RemoveAtHub. Neither
// carries Local or Force: those belong to the node's own remove alone.
type removeBody struct {
	ID     string       `json:"id"`
	Origin store.Origin `json:"origin,omitzero"`
}

type settingsBody struct {
	Target    string          `json:"target,omitempty"`
	Scope     store.Scope     `json:"scope,omitzero"`
	RunnerApp store.RunnerApp `json:"runner_app,omitzero"`
}

type grantedBody struct {
	Slot      int    `json:"slot"`
	Runner    string `json:"runner"`
	JITConfig string `json:"jit_config"`
}

type grantFailedBody struct {
	Slot   int    `json:"slot"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

type retiredBody struct {
	Runner        string `json:"runner"`
	Status        int    `json:"status,omitempty"`
	TargetVisible bool   `json:"target_visible,omitempty"`
}

type stopLostBody struct {
	Runners []string `json:"runners"`
}

type removedBody struct {
	Lost []string `json:"lost"`
}

type acknowledgedBody struct {
	Through int64 `json:"through"`
}

type deliverBody struct {
	Entries []store.Entry `json:"entries"`
}

type requestRunnerBody struct {
	Slot int `json:"slot"`
}

type retireRunnerBody struct {
	Slot   int    `json:"slot"`
	Runner string `json:"runner"`
}

// EncodeToAgent encodes a message from the Hub to an agent. m is a
// Challenge, Accepted, Refused, Removed, Ping or Granted, or one of the
// node events the Hub may send: node.Command, node.Dismiss, node.Remove,
// node.HubSettings, node.GrantFailed, node.Retired,
// node.StopLostRunners or node.Acknowledged, or an AskDiagnostics.
// Anything else is refused.
func EncodeToAgent(m any) ([]byte, error) {
	switch m := m.(type) {
	case Challenge:
		return encode(typeChallenge, challengeBody(m))
	case Accepted:
		return encode(typeAccepted, nil)
	case Refused:
		return encode(typeRefused, fromRefusal(m.Reason))
	case Removed:
		if len(m.Lost) == 0 {
			return encode(typeRemoved, nil)
		}
		return encode(typeRemoved, removedBody(m))
	case Ping:
		return encode(typePing, nil)
	case Granted:
		return encode(typeGranted, grantedBody{Slot: m.Slot, Runner: m.Runner, JITConfig: m.JITConfig.Reveal()})
	case node.Command:
		origin, err := store.FromOrigin(m.Origin)
		if err != nil {
			return nil, err
		}
		event, err := store.FromEvent(m.Event)
		if err != nil {
			return nil, err
		}
		return encode(typeCommand, commandBody{ID: m.ID, Origin: origin, Event: event})
	case node.Dismiss:
		origin, err := store.FromOrigin(m.Origin)
		if err != nil {
			return nil, err
		}
		return encode(typeDismiss, dismissBody{ID: m.ID, Origin: origin, Notice: m.Notice})
	case node.Remove:
		if m.Local || m.Force {
			return nil, errors.New("wire: the Hub never sends the node's own remove")
		}
		origin, err := store.FromOrigin(m.Origin)
		if err != nil {
			return nil, err
		}
		return encode(typeRemove, removeBody{ID: m.ID, Origin: origin})
	case node.HubSettings:
		return encode(typeSettings, settingsBody{Target: m.Target, Scope: store.Scope(m.Scope), RunnerApp: store.RunnerApp(m.RunnerApp)})
	case node.GrantFailed:
		reason, err := store.FromReason(m.Reason)
		if err != nil {
			return nil, err
		}
		return encode(typeGrantFailed, grantFailedBody{Slot: m.Slot, Reason: reason, Detail: m.Detail})
	case node.Retired:
		return encode(typeRetired, retiredBody{Runner: m.Runner, Status: m.Status, TargetVisible: m.TargetVisible})
	case node.StopLostRunners:
		return encode(typeStopLost, stopLostBody{Runners: m.Runners})
	case node.Acknowledged:
		return encode(typeAcknowledged, acknowledgedBody{Through: m.Through})
	case AskDiagnostics:
		return encode(typeAskDiagnostics, askDiagnosticsBody(m))
	}
	return nil, fmt.Errorf("wire: the Hub never sends a %T", m)
}

// DecodeToAgent decodes a message from the Hub to an agent, returning
// one of the values EncodeToAgent takes. A type the Hub may not send is
// refused with UnknownType.
func DecodeToAgent(b []byte) (any, error) {
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("wire: a message that isn't one: %w", err)
	}
	switch env.Type {
	case typeChallenge:
		var body challengeBody
		err := decode(env, &body)
		return Challenge(body), err
	case typeAccepted:
		return Accepted{}, nil
	case typeRefused:
		var body refusal
		err := decode(env, &body)
		return Refused{Reason: body.toError()}, err
	case typeRemoved:
		if len(env.Body) == 0 {
			return Removed{}, nil
		}
		var body removedBody
		err := decode(env, &body)
		return Removed(body), err
	case typePing:
		return Ping{}, nil
	case typeGranted:
		var body grantedBody
		err := decode(env, &body)
		return Granted{Slot: body.Slot, Runner: body.Runner, JITConfig: secret.New(body.JITConfig)}, err
	case typeCommand:
		var body commandBody
		if err := decode(env, &body); err != nil {
			return nil, err
		}
		origin, err := body.Origin.ToOrigin()
		if err != nil {
			return nil, err
		}
		event, err := body.Event.ToEvent()
		if err != nil {
			return nil, err
		}
		return node.Command{ID: body.ID, Origin: origin, Event: event}, nil
	case typeDismiss:
		var body dismissBody
		if err := decode(env, &body); err != nil {
			return nil, err
		}
		origin, err := body.Origin.ToOrigin()
		return node.Dismiss{ID: body.ID, Origin: origin, Notice: body.Notice}, err
	case typeRemove:
		var body removeBody
		if err := decode(env, &body); err != nil {
			return nil, err
		}
		origin, err := body.Origin.ToOrigin()
		return node.Remove{ID: body.ID, Origin: origin}, err
	case typeSettings:
		var body settingsBody
		err := decode(env, &body)
		return node.HubSettings{Target: body.Target, Scope: node.Scope(body.Scope), RunnerApp: node.RunnerApp(body.RunnerApp)}, err
	case typeGrantFailed:
		var body grantFailedBody
		if err := decode(env, &body); err != nil {
			return nil, err
		}
		reason, err := store.ToReason(body.Reason)
		return node.GrantFailed{Slot: body.Slot, Reason: reason, Detail: body.Detail}, err
	case typeRetired:
		var body retiredBody
		err := decode(env, &body)
		return node.Retired{Runner: body.Runner, Status: body.Status, TargetVisible: body.TargetVisible}, err
	case typeStopLost:
		var body stopLostBody
		err := decode(env, &body)
		return node.StopLostRunners{Runners: body.Runners}, err
	case typeAcknowledged:
		var body acknowledgedBody
		err := decode(env, &body)
		return node.Acknowledged{Through: body.Through}, err
	case typeAskDiagnostics:
		var body askDiagnosticsBody
		err := decode(env, &body)
		return AskDiagnostics(body), err
	}
	return nil, UnknownType{Type: env.Type}
}

// EncodeToHub encodes a message from an agent to the Hub. m is a Hello
// or a Ping, the node's report as a node.Node, one of the node's
// effects bound for the Hub: node.Reply, node.Deliver,
// node.RequestRunner, node.RetireRunner, node.RemoveAtHub or
// node.ConfirmRemoved, or a DiagnosticsReport. Anything else is refused.
func EncodeToHub(m any) ([]byte, error) {
	switch m := m.(type) {
	case Hello:
		return encode(typeHello, helloBody{Protocol: m.Protocol, Node: m.Node, Platform: string(m.Platform), Signature: m.Signature})
	case Ping:
		return encode(typePing, nil)
	case node.Node:
		report, err := store.FromNode(m)
		if err != nil {
			return nil, err
		}
		return encode(typeReport, report)
	case node.Reply:
		reply, err := store.FromReply(m)
		if err != nil {
			return nil, err
		}
		return encode(typeReply, reply)
	case node.Deliver:
		body := deliverBody{Entries: []store.Entry{}}
		for _, e := range m.Entries {
			entry, err := store.FromEntry(e)
			if err != nil {
				return nil, err
			}
			body.Entries = append(body.Entries, entry)
		}
		return encode(typeDeliver, body)
	case node.RequestRunner:
		return encode(typeRequestRunner, requestRunnerBody{Slot: m.Slot})
	case node.RetireRunner:
		return encode(typeRetireRunner, retireRunnerBody{Slot: m.Slot, Runner: m.Runner})
	case node.RemoveAtHub:
		origin, err := store.FromOrigin(m.Origin)
		if err != nil {
			return nil, err
		}
		return encode(typeRemoveAtHub, removeBody{ID: m.ID, Origin: origin})
	case node.ConfirmRemoved:
		return encode(typeConfirmRemoved, nil)
	case DiagnosticsReport:
		return encode(typeDiagnosticsReport, diagnosticsReportBody(m))
	}
	return nil, fmt.Errorf("wire: an agent never sends a %T", m)
}

// DecodeToHub decodes a message from an agent to the Hub, returning one
// of the values EncodeToHub takes. A type an agent may not send is
// refused with UnknownType.
func DecodeToHub(b []byte) (any, error) {
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("wire: a message that isn't one: %w", err)
	}
	switch env.Type {
	case typeHello:
		var body helloBody
		err := decode(env, &body)
		return Hello{Protocol: body.Protocol, Node: body.Node, Platform: runnerapp.Platform(body.Platform), Signature: body.Signature}, err
	case typePing:
		return Ping{}, nil
	case typeReport:
		var body store.Node
		if err := decode(env, &body); err != nil {
			return nil, err
		}
		return body.ToNode()
	case typeReply:
		var body store.Reply
		if err := decode(env, &body); err != nil {
			return nil, err
		}
		return body.ToReply()
	case typeDeliver:
		var body deliverBody
		if err := decode(env, &body); err != nil {
			return nil, err
		}
		var d node.Deliver
		for _, s := range body.Entries {
			e, err := s.ToEntry()
			if err != nil {
				return nil, err
			}
			d.Entries = append(d.Entries, e)
		}
		return d, nil
	case typeRequestRunner:
		var body requestRunnerBody
		err := decode(env, &body)
		return node.RequestRunner{Slot: body.Slot}, err
	case typeRetireRunner:
		var body retireRunnerBody
		err := decode(env, &body)
		return node.RetireRunner{Slot: body.Slot, Runner: body.Runner}, err
	case typeRemoveAtHub:
		var body removeBody
		if err := decode(env, &body); err != nil {
			return nil, err
		}
		origin, err := body.Origin.ToOrigin()
		return node.RemoveAtHub{ID: body.ID, Origin: origin}, err
	case typeConfirmRemoved:
		return node.ConfirmRemoved{}, nil
	case typeDiagnosticsReport:
		var body diagnosticsReportBody
		err := decode(env, &body)
		return DiagnosticsReport(body), err
	}
	return nil, UnknownType{Type: env.Type}
}

func encode(typ string, body any) ([]byte, error) {
	env := envelope{Type: typ}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		env.Body = b
	}
	return json.Marshal(env)
}

func decode(env envelope, body any) error {
	if err := json.Unmarshal(env.Body, body); err != nil {
		return fmt.Errorf("wire: a %s message whose body isn't one: %w", env.Type, err)
	}
	return nil
}

// A refusal is a refusal's form on the wire: a code for each rule the
// other end can act on, its message, and the fields of its error.
type refusal struct {
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	Agent      int    `json:"agent,omitempty"`
	Hub        int    `json:"hub,omitempty"`
	Name       string `json:"name,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// The refusals with a code: an enrollment's and a connection's.
var refusals = []struct {
	code string
	err  error
}{
	{"unknown-code", pool.ErrUnknownCode},
	{"code-used", pool.ErrCodeUsed},
	{"code-revoked", pool.ErrCodeRevoked},
	{"code-expired", pool.ErrCodeExpired},
	{"unknown-node", ErrUnknownNode},
	{"wrong-key", pool.ErrWrongKey},
}

func fromRefusal(err error) refusal {
	r := refusal{Message: err.Error()}
	var version pool.VersionMismatch
	var invalid node.InvalidName
	var taken pool.NameTaken
	switch {
	case errors.As(err, &version):
		r.Code, r.Agent, r.Hub = "version", version.Agent, version.Hub
	case errors.As(err, &invalid):
		r.Code, r.Name = "invalid-name", invalid.Name
	case errors.As(err, &taken):
		r.Code, r.Name, r.Suggestion = "name-taken", taken.Name, taken.Suggestion
	default:
		for _, known := range refusals {
			if errors.Is(err, known.err) {
				r.Code = known.code
				break
			}
		}
	}
	return r
}

func (r refusal) toError() error {
	switch r.Code {
	case "version":
		return pool.VersionMismatch{Agent: r.Agent, Hub: r.Hub}
	case "invalid-name":
		return node.InvalidName{Name: r.Name}
	case "name-taken":
		return pool.NameTaken{Name: r.Name, Suggestion: r.Suggestion}
	}
	for _, known := range refusals {
		if known.code == r.Code {
			return known.err
		}
	}
	return errors.New(r.Message)
}

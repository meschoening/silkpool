package hubops

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/secret"
	"github.com/meschoening/silkpool/internal/wire"
)

// A Code is an enrollment code as the operator sees it: its short ID,
// which isn't secret, when it was made and expires, and its state.
type Code struct {
	ID      string    `json:"id"`
	Made    time.Time `json:"made"`
	Expires time.Time `json:"expires"`
	State   CodeState `json:"state"`
	// Node is the name of the node it enrolled, once it's used.
	Node string `json:"node,omitempty"`
}

// A CodeState is where a code is in its one use.
type CodeState string

// The states of a code.
const (
	Open    CodeState = "open"
	Used    CodeState = "used"
	Revoked CodeState = "revoked"
	Expired CodeState = "expired"
)

// A NewCode is a code just made, with its text: the one time the Hub
// gives it out. In JSON, the text is revealed as "text".
type NewCode struct {
	Code
	Text secret.Value
}

// MarshalJSON gives the code's fields and its text.
func (c NewCode) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code
		Text string `json:"text"`
	}{c.Code, c.Text.Reveal()})
}

// A CodeChange is a code after a change, and whether it changed.
type CodeChange struct {
	Code    Code `json:"code"`
	Changed bool `json:"changed"`
}

// MakeCode makes an enrollment code, from "Add node" or code new.
func (o *Ops) MakeCode(ctx context.Context, from node.Origin) (NewCode, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return NewCode{}, err
	}
	if h.Settings.BaseURL == "" {
		return NewCode{}, refuse("not-set-up", "the Hub has no URL yet: run silkpool-hub init")
	}
	code, err := o.c.NewCode(h.Settings.BaseURL, o.c.Pin)
	if err != nil {
		return NewCode{}, err
	}
	id := o.c.NewID()
	for slices.ContainsFunc(h.Pool.Codes, func(c pool.Code) bool { return c.ID == id }) {
		id = o.c.NewID()
	}
	if err := o.apply(ctx, pool.MakeCode{ID: id, Hash: wire.Hash(code.Secret), Origin: from}); err != nil {
		return NewCode{}, err
	}
	if h, err = o.state(ctx); err != nil {
		return NewCode{}, err
	}
	c, _ := o.code(h, id)
	return NewCode{Code: c, Text: code.Text()}, nil
}

// Codes lists the Hub's codes, oldest first: each is kept for
// pool.CodeRetention after it was made.
func (o *Ops) Codes(ctx context.Context) ([]Code, error) {
	h, err := o.state(ctx)
	if err != nil {
		return nil, err
	}
	out := []Code{}
	for _, c := range h.Pool.Codes {
		out = append(out, o.codeOf(h, c))
	}
	return out, nil
}

// RevokeCode revokes the open code id names, ignoring case. A code
// that's revoked or expired already changes nothing, and a used one is
// refused.
func (o *Ops) RevokeCode(ctx context.Context, from node.Origin, id string) (CodeChange, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return CodeChange{}, err
	}
	c, ok := o.code(h, strings.ToLower(strings.TrimSpace(id)))
	switch {
	case !ok:
		return CodeChange{}, refuse("unknown-code", "the Hub has no code %s", id)
	case c.State == Used:
		return CodeChange{}, refuse("code-used", "code %s enrolled %s, so there's nothing to revoke: remove the node instead", c.ID, c.Node)
	case c.State != Open:
		return CodeChange{Code: c}, nil
	}
	if err := o.apply(ctx, pool.RevokeCode{ID: c.ID, Origin: from}); err != nil {
		return CodeChange{}, err
	}
	if h, err = o.state(ctx); err != nil {
		return CodeChange{}, err
	}
	c, _ = o.code(h, c.ID)
	return CodeChange{Code: c, Changed: true}, nil
}

// code returns the code with id.
func (o *Ops) code(h hubstore.Hub, id string) (Code, bool) {
	i := slices.IndexFunc(h.Pool.Codes, func(c pool.Code) bool { return c.ID == id })
	if i < 0 {
		return Code{}, false
	}
	return o.codeOf(h, h.Pool.Codes[i]), true
}

// codeOf returns c as the operator sees it.
func (o *Ops) codeOf(h hubstore.Hub, c pool.Code) Code {
	out := Code{ID: c.ID, Made: c.Made.UTC(), Expires: c.Made.Add(pool.CodeLifetime).UTC(), State: Open}
	switch {
	case c.Used != "":
		out.State = Used
		if i := slices.IndexFunc(h.Pool.Nodes, func(n pool.Node) bool { return n.ID == c.Used }); i >= 0 {
			out.Node = h.Pool.Nodes[i].Name
		}
	case c.Revoked:
		out.State = Revoked
	case !o.c.Now().Before(out.Expires):
		out.State = Expired
	}
	return out
}

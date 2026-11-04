package pool

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/node"
)

// CodeLifetime is how long an enrollment code lasts after the Hub makes
// it, and CodeRetention how long the Hub keeps it, whatever its state.
// A code has ended at least 29 days before it's dropped.
const (
	CodeLifetime  = 24 * time.Hour
	CodeRetention = 30 * 24 * time.Hour
)

// The rules under which an enrollment is refused. Nothing is saved when
// one is.
var (
	// ErrUnknownCode refuses a code the Hub never made.
	ErrUnknownCode = errors.New("the enrollment code isn't one this Hub made")
	// ErrCodeUsed refuses a code that has already enrolled a machine,
	// unless it's that machine's enrollment sent again.
	ErrCodeUsed = errors.New("the enrollment code has been used: make a new one in the Hub")
	// ErrCodeRevoked refuses a code the operator revoked.
	ErrCodeRevoked = errors.New("the enrollment code was revoked: make a new one in the Hub")
	// ErrCodeExpired refuses a code CodeLifetime old or older.
	ErrCodeExpired = errors.New("the enrollment code has expired: make a new one in the Hub")
)

// NameTaken refuses a name another node has, ignoring case, and suggests
// the first free one of Name-2, Name-3 and so on. A tombstone's name is
// free. A name that breaks node.CheckName's rule is refused with
// node.InvalidName before it's checked against the others.
type NameTaken struct{ Name, Suggestion string }

func (e NameTaken) Error() string {
	return fmt.Sprintf("the name %s is taken: enroll with --name %s, or another name", e.Name, e.Suggestion)
}

// enroll enrolls the machine e is from as a new node, or refuses it and
// saves nothing. A machine whose Enrolled was lost on its way sends the
// same code with the same key again, and is answered again.
func (a *applying) enroll(e Enroll) {
	p := a.p
	i := slices.IndexFunc(p.Codes, func(c Code) bool { return c.Hash == e.Hash })
	if n := a.enrolled(e, i); n != nil {
		a.emit(Enrolled{Node: n.ID, Name: n.Name, Target: n.Target})
		return
	}
	if err := a.refuse(e, i); err != nil {
		a.emit(EnrollRefused{Hash: e.Hash, Reason: err})
		return
	}
	p.Codes = slices.Clone(p.Codes)
	p.Codes[i].Used = e.Node
	p.Nodes = append(p.Nodes, Node{ID: e.Node, Name: e.Name, Key: e.Key, Target: p.DefaultTarget})
	a.emit(Enrolled{Node: e.Node, Name: e.Name, Target: p.DefaultTarget})
	a.store(Entry{Node: e.Node, At: a.now, Origin: e.Origin, What: Enrollment{Name: e.Name, Target: p.DefaultTarget}})
}

// enrolled returns the node the i-th code enrolled, if e is that
// node's enrollment sent again: the key is the node's, and it isn't a
// tombstone. It's answered as the Hub now holds the node, whatever name
// e asks for, and whether or not the code has expired since.
func (a *applying) enrolled(e Enroll, i int) *Node {
	if i < 0 || a.p.Codes[i].Used == "" {
		return nil
	}
	j := slices.IndexFunc(a.p.Nodes, func(n Node) bool { return n.ID == a.p.Codes[i].Used })
	if j < 0 || a.p.Nodes[j].Key != e.Key || !a.p.Nodes[j].Removed.IsZero() {
		return nil
	}
	return &a.p.Nodes[j]
}

// refuse returns the rule that refuses e, whose code is the i-th, or nil.
// The code comes first, so a machine without a good one learns nothing
// else, and the name's rule comes before the check that it's free.
func (a *applying) refuse(e Enroll, i int) error {
	if i < 0 {
		return ErrUnknownCode
	}
	mismatch := a.p.accepts(e.Protocol)
	switch c := a.p.Codes[i]; {
	case c.Used != "":
		return ErrCodeUsed
	case c.Revoked:
		return ErrCodeRevoked
	case !a.now.Before(c.Made.Add(CodeLifetime)):
		return ErrCodeExpired
	case mismatch != nil:
		return mismatch
	}
	if err := node.CheckName(e.Name); err != nil {
		return err
	}
	if a.p.taken(e.Name, "") {
		return NameTaken{Name: e.Name, Suggestion: a.p.suggest(e.Name, "")}
	}
	return nil
}

// suggest returns the first of name-2, name-3 and so on that's free for
// the node except, or for a new one if it's empty, with name cut to
// leave room for the number.
func (p Pool) suggest(name, except string) string {
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		if s := name[:min(len(name), node.MaxNameLength-len(suffix))] + suffix; !p.taken(s, except) {
			return s
		}
	}
}

// taken reports whether a node that isn't a tombstone, other than the
// node except, has name, ignoring case.
func (p Pool) taken(name, except string) bool {
	return slices.ContainsFunc(p.Nodes, func(n Node) bool {
		return n.Removed.IsZero() && n.ID != except && strings.EqualFold(n.Name, name)
	})
}

// revoke revokes the code e names, and writes it, unless it's been used
// or revoked already.
func (a *applying) revoke(e RevokeCode) {
	p := a.p
	i := slices.IndexFunc(p.Codes, func(c Code) bool { return c.ID == e.ID })
	if i < 0 || p.Codes[i].Used != "" || p.Codes[i].Revoked {
		return
	}
	p.Codes = slices.Clone(p.Codes)
	p.Codes[i].Revoked = true
	a.store(Entry{At: a.now, Origin: e.Origin, What: CodeRevoked{Code: e.ID}})
}

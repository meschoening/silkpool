package roleaccount

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Unresolved is Remove's error for a record it can't show is Silkpool's
// to delete, though it may be: one holding a GeneratedUID Silkpool
// claimed, which it can't show is the record it made, or a group a user
// still has as its primary group. Remove leaves it, keeps its claim, and
// stops.
type Unresolved struct {
	Kind Kind
	Name string
	// Why says why, with the record's evidence.
	Why string
}

func (e *Unresolved) Error() string {
	return fmt.Sprintf("%s/%s is left, since %s", e.Kind, e.Name, e.Why)
}

// A Left is a record of the account's name that Silkpool didn't make,
// which Remove left as it is.
type Left struct {
	Kind Kind
	Name string
	// Evidence is what makes the record an identity, as the directory
	// holds it: its UniqueID, PrimaryGroupID, GeneratedUID and RealName.
	Evidence string
}

// Remove deletes the role account name, as ledger has it, its user
// before its group, and returns the ledger as it leaves it and the
// records of the name it left as not Silkpool's. It hands keep each
// change of the ledger, after the change in the directory it records,
// and stops if keep fails.
//
// It judges each record as it finds it, at every call, so a Remove that
// failed or was cut short finishes on the next:
//
//   - A claim owned, and the record holds its GeneratedUID: Silkpool's.
//     It's deleted, read back gone, and only then is its claim cleared.
//   - A claim, and no record: gone, as after a Remove cut short between
//     the delete and keep. Its claim is cleared.
//   - A claim pending, and the record holds its GeneratedUID and nothing
//     that makes it an identity: Silkpool's first write and nothing
//     since. It's owned, as Ensure would, then deleted as above.
//   - A claim pending, and the record holds its GeneratedUID and more:
//     it may be a record that was there before Silkpool's first write
//     gave it the GeneratedUID. It's Unresolved.
//   - A record holding the claim's GeneratedUID among others: Unresolved.
//   - A record holding another GeneratedUID, or none, whatever the
//     claim: someone else's, perhaps since it replaced Silkpool's. It's
//     left, reported in Left, and its claim cleared. A record that
//     carries Silkpool's marker is no exception: Remove never deletes by
//     a name or a marker.
//
// A group goes only once no user in any directory the search policy
// holds has its GID as its primary group, which would leave that user
// without one; otherwise it's Unresolved.
func Remove(ctx context.Context, d Directory, name string, ledger Ledger, keep func(Ledger) error) (Ledger, []Left, error) {
	if err := checkName(name); err != nil {
		return ledger, nil, err
	}
	r := remover{ctx: ctx, d: d, name: name, ledger: ledger, keep: keep}
	var left []Left
	for _, kind := range []Kind{Users, Groups} {
		l, err := r.remove(kind)
		if l != nil {
			left = append(left, *l)
		}
		if err != nil {
			return r.ledger, left, err
		}
	}
	return r.ledger, left, nil
}

type remover struct {
	ctx    context.Context
	d      Directory
	name   string
	ledger Ledger
	keep   func(Ledger) error
}

func (r *remover) claimOf(kind Kind) *Claim {
	if kind == Users {
		return &r.ledger.User
	}
	return &r.ledger.Group
}

// remove deletes the record of kind if it's Silkpool's, and returns it as
// a Left if it isn't.
func (r *remover) remove(kind Kind) (*Left, error) {
	c := r.claimOf(kind)
	rec, found, err := r.d.Read(r.ctx, kind, r.name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, r.clear(kind)
	}
	switch {
	case c.GUID != "" && slices.Contains(rec["GeneratedUID"], c.GUID) && !rec.Is("GeneratedUID", c.GUID):
		return nil, &Unresolved{kind, r.name, fmt.Sprintf("it holds the GeneratedUID Silkpool gave it among others: %s", evidence(rec))}
	case c.GUID == "" || !rec.Is("GeneratedUID", c.GUID):
		return &Left{kind, r.name, evidence(rec)}, r.clear(kind)
	case !c.Owned && !bare(kind, rec):
		return nil, &Unresolved{kind, r.name, fmt.Sprintf("it holds the GeneratedUID Silkpool wrote first, but more besides, "+
			"so it may be a record that was there before: %s", evidence(rec))}
	case !c.Owned:
		*c = Claim{GUID: c.GUID, Owned: true}
		if err := r.keep(r.ledger); err != nil {
			return nil, err
		}
	}
	if kind == Groups {
		if err := r.noPrimary(rec); err != nil {
			return nil, err
		}
	}
	if err := r.d.remove(r.ctx, kind, r.name); err != nil {
		return nil, err
	}
	if _, found, err := r.d.Read(r.ctx, kind, r.name); err != nil || found {
		return nil, fmt.Errorf("%s/%s is still there, or can't be read, once deleted (%v)", kind, r.name, err)
	}
	return nil, r.clear(kind)
}

// noPrimary fails with Unresolved if a user in any directory the search
// policy holds has the group g's GID as its primary group.
func (r *remover) noPrimary(g Record) error {
	if _, ok := g["PrimaryGroupID"]; !ok {
		return nil
	}
	gid, ok := id(g, "PrimaryGroupID")
	if !ok {
		return &Unresolved{Groups, r.name, fmt.Sprintf("its GID can't be read: %s", evidence(g))}
	}
	users, err := r.d.All(r.ctx, "/Search", Users, "PrimaryGroupID")
	if err != nil {
		return err
	}
	for _, u := range users {
		if slices.Contains(u["PrimaryGroupID"], strconv.Itoa(gid)) {
			return &Unresolved{Groups, r.name, fmt.Sprintf("user %s has its GID %d as its primary group", u["RecordName"][0], gid)}
		}
	}
	return nil
}

// clear clears the claim of kind, if there's one.
func (r *remover) clear(kind Kind) error {
	c := r.claimOf(kind)
	if *c == (Claim{}) {
		return nil
	}
	*c = Claim{}
	return r.keep(r.ledger)
}

// evidence says what makes r an identity, for a record Silkpool leaves.
func evidence(r Record) string {
	var parts []string
	for _, a := range []string{"UniqueID", "PrimaryGroupID", "GeneratedUID", "RealName"} {
		if v, ok := r[a]; ok {
			parts = append(parts, fmt.Sprintf("%s %q", a, strings.Join(v, ", ")))
		}
	}
	if len(parts) == 0 {
		return "no UniqueID, PrimaryGroupID, GeneratedUID or RealName"
	}
	return strings.Join(parts, ", ")
}

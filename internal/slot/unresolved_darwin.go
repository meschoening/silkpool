package slot

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/meschoening/silkpool/internal/roleaccount"
	"github.com/meschoening/silkpool/internal/secret"
)

// Unresolved says what shows the service account may still be
// Silkpool's, so that sudo silkpool uninstall, which deletes the
// account's ledger with Silkpool's folder, never leaves such an account
// with nothing to prove whose it is. No tool could delete it then, and a
// reinstall would adopt it by its marker. A forced removal leaves one
// when a reset or the account's deletion fails.
//
// The account is unresolved while its ledger holds a claim, owned or
// pending, whose GeneratedUID the record of its name, of the claim's
// kind, still holds, or while a live process holds the ledger's ID as
// its effective, real or saved UID. What it says names the account, and
// gives only what the ledger and the records show: each such record's
// UniqueID and PrimaryGroupID, the GeneratedUID it holds and whether the
// ledger owns it or only claimed it, and the processes' PIDs, with the
// text from the machine scrubbed. It says it in one line, or none.
//
// A claim whose record is gone isn't unresolved. A record of the name
// that doesn't hold the claim's GeneratedUID is someone else's: it's
// logged and left. Unresolved only reads, and acts on nothing by a name,
// a marker or a UID. It fails, rather than passing for none, when the
// ledger, a record or the processes can't be read.
func (s *Slots) Unresolved(ctx context.Context) ([]string, error) {
	l, err := readLedger(s.ledger())
	if err != nil {
		return nil, fmt.Errorf("can't read the service account's ledger: %w", err)
	}
	said, err := s.claimed(ctx, l)
	if err != nil {
		return nil, err
	}
	if l.ID != 0 {
		procs, err := s.procs(ctx)
		if err != nil {
			return nil, fmt.Errorf("can't list the processes, to find any holding the service account's ID: %w", err)
		}
		if pids := holding(procs, l.ID); len(pids) == 1 {
			said = append(said, fmt.Sprintf("process %d holds its ledger's ID %d as a UID", pids[0], l.ID))
		} else if len(pids) > 1 {
			said = append(said, fmt.Sprintf("processes %s hold its ledger's ID %d as a UID", list(itoas(pids)), l.ID))
		}
	}
	if len(said) == 0 {
		return nil, nil
	}
	return []string{secret.Scrub(fmt.Sprintf("the service account %s: %s", s.Name, strings.Join(said, "; ")))}, nil
}

// claimed says what shows that each record of the service account's
// name that still holds a GeneratedUID its ledger l claims for that kind
// of record may be Silkpool's. It logs a record of the name that doesn't
// hold the claim's GeneratedUID, as someone else's.
func (s *Slots) claimed(ctx context.Context, l roleaccount.Ledger) ([]string, error) {
	name := s.Name
	var said []string
	for _, c := range []struct {
		kind  roleaccount.Kind
		what  string
		claim roleaccount.Claim
		ids   []string
	}{
		{roleaccount.Users, "user", l.User, []string{"UniqueID", "PrimaryGroupID"}},
		{roleaccount.Groups, "group", l.Group, []string{"PrimaryGroupID"}},
	} {
		if c.claim.GUID == "" {
			continue
		}
		r, ok, err := s.machine.Read(ctx, c.kind, name)
		if err != nil {
			return nil, fmt.Errorf("can't read the service account's %s record: %w", c.what, err)
		}
		switch {
		case !ok:
		case !slices.Contains(r["GeneratedUID"], c.claim.GUID):
			s.log.Warn("A record of the service account's name isn't Silkpool's, so it's left",
				"record", string(c.kind)+"/"+name,
				"evidence", secret.Scrub(attrs(r, "UniqueID", "PrimaryGroupID", "GeneratedUID", "RealName")))
		case c.claim.Owned:
			said = append(said, fmt.Sprintf("its %s record (%s) holds GeneratedUID %s, which its ledger owns",
				c.what, attrs(r, c.ids...), c.claim.GUID))
		default:
			said = append(said, fmt.Sprintf("its %s record (%s) holds GeneratedUID %s, which its ledger claimed, "+
				"though Silkpool never confirmed the record is the one it made", c.what, attrs(r, c.ids...), c.claim.GUID))
		}
	}
	return said, nil
}

// attrs says what the record r holds of each of the attributes names, as
// the directory holds it.
func attrs(r roleaccount.Record, names ...string) string {
	var parts []string
	for _, a := range names {
		if v, ok := r[a]; ok {
			parts = append(parts, fmt.Sprintf("%s %q", a, strings.Join(v, ", ")))
		} else {
			parts = append(parts, "no "+a)
		}
	}
	return strings.Join(parts, ", ")
}

// itoas writes each of ns in decimal.
func itoas(ns []int) []string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return s
}

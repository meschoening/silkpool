package slot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/meschoening/silkpool/internal/roleaccount"
	"github.com/meschoening/silkpool/internal/secret"
)

// accountMarker is the description that marks the service account as
// Silkpool's, its RealName.
const accountMarker = "Silkpool service account"

// MakeAccount makes the Mac's one service account, as install does
// (ADR 0007), or finds it as its ledger has it, and returns it. Every
// slot's runners run as it. It's a role account, Name, with a primary
// group of its own, both with the highest ID from 499 down to 400 that
// nothing on the Mac holds, picked once, no password, no shell, its home
// in Silkpool's folder (Home), and Silkpool's marker as its RealName. It
// keeps the account's one ledger in Silkpool's state, which only root
// may open (roleaccount.Ensure), so it knows the account's records by
// the GeneratedUIDs it gave them, not by their names alone. A record
// with the marker that the ledger doesn't name, as after an uninstall
// that lost the ledger, is adopted.
//
// The ledger says when the account's folders must be emptied, since a
// record of it was made or adopted: its home, and each slot's runner
// root and temporary folder. MakeAccount empties them before it clears
// that, so a failure or a crash between them can't keep what was there.
// Its caller runs it with no runner running, as install does.
func (s *Slots) MakeAccount(ctx context.Context) (ServiceAccount, error) {
	s.account.Lock()
	defer s.account.Unlock()
	if err := s.stateFolder(); err != nil {
		return ServiceAccount{}, err
	}
	ledger, err := readLedger(s.ledger())
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("can't read the service account's ledger: %w", err)
	}
	spec := roleaccount.Spec{Name: s.Name, Marker: accountMarker, Home: s.Home(), Adopt: true}
	made, err := s.ensure(ctx, spec, ledger, s.keepLedger)
	if err != nil {
		return ServiceAccount{}, accountError(s.Name, err)
	}
	if made.Made {
		if err := s.emptyAccountFolders(); err != nil {
			return ServiceAccount{}, err
		}
	}
	a, err := s.Account(ctx)
	if err != nil {
		return ServiceAccount{}, err
	}
	if err := ownedDir(s.Home(), a.UID, a.GID, 0o700); err != nil {
		return ServiceAccount{}, fmt.Errorf("can't make the service account's home: %w", err)
	}
	if made.Made {
		// Only now are the folders empty: until then, every MakeAccount
		// empties them again.
		l := made.Ledger
		l.Rebuild = false
		if err := s.keepLedger(l); err != nil {
			return ServiceAccount{}, fmt.Errorf("can't keep the service account's ledger: %w", err)
		}
	}
	return a, nil
}

// NoteClaims has MakeAccount, and each check of the account, tell note
// of the service account's user claim each time its ledger is about to
// be kept, and keep nothing if note fails: pending before the record's
// first write, and owned once it has read back the record it made. A
// test that makes the account as root tells the privileged hand-off so,
// since the hand-off deletes only an account a test recorded owning
// (docs/testing.md). Only tests call it, before anything else.
func (s *Slots) NoteClaims(note func(roleaccount.Claim) error) {
	inner := s.ensure
	s.ensure = func(ctx context.Context, spec roleaccount.Spec, l roleaccount.Ledger, keep func(roleaccount.Ledger) error) (roleaccount.Account, error) {
		return inner(ctx, spec, l, func(next roleaccount.Ledger) error {
			if err := note(next.User); err != nil {
				return err
			}
			return keep(next)
		})
	}
}

// Prepare readies slot n for a runner, before each runner. It checks the
// service account for drift (CheckAccount), and makes the slot's own
// temporary folder, TMPDIR to its runners (TempDir), the account's, mode
// 0700. Then it runs the account-wide clean if it's due, holding the slot
// while it waits for every slot to be idle, once the clean has been due
// for CleanWithin (cleanFirst). A clean that fails fails the slot.
func (s *Slots) Prepare(ctx context.Context, n int) error {
	if err := s.prepare(ctx, n); err != nil {
		return err
	}
	return s.cleanFirst(ctx)
}

// prepare is Prepare without the clean: the slot's folders, the
// account's check and the slot's temporary folder.
func (s *Slots) prepare(ctx context.Context, n int) error {
	if err := s.slotFolders(n); err != nil {
		return err
	}
	s.account.Lock()
	defer s.account.Unlock()
	a, err := s.checkAccount(ctx)
	if err != nil {
		return err
	}
	if err := ownedDir(s.TempDir(n), a.UID, a.GID, 0o700); err != nil {
		return fmt.Errorf("can't make slot %d's temporary folder: %w", n, err)
	}
	return nil
}

// CheckAccount checks the service account for drift, as the agent starts
// and as each slot is prepared. It never makes the account, which install
// does (MakeAccount): it restores the settings Silkpool owns silently, and
// fails closed with a *CantStart when the account isn't as install made
// it, when it was added to another group directly, which it never
// removes, and when a record of its name isn't the one Silkpool made.
// Memberships through nested groups don't count. Each fails every slot,
// since they share the account. It then makes the account's home, if
// it's gone, the account's, mode 0700.
func (s *Slots) CheckAccount(ctx context.Context) error {
	s.account.Lock()
	defer s.account.Unlock()
	_, err := s.checkAccount(ctx)
	return err
}

func (s *Slots) checkAccount(ctx context.Context) (ServiceAccount, error) {
	ledger, err := readLedger(s.ledger())
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("can't read the service account's ledger: %w", err)
	}
	spec := roleaccount.Spec{Name: s.Name, Marker: accountMarker, Home: s.Home(), Made: true}
	if _, err := s.ensure(ctx, spec, ledger, s.keepLedger); err != nil {
		return ServiceAccount{}, accountError(s.Name, err)
	}
	a, err := s.Account(ctx)
	if err != nil {
		return ServiceAccount{}, err
	}
	if err := ownedDir(s.Home(), a.UID, a.GID, 0o700); err != nil {
		return ServiceAccount{}, fmt.Errorf("can't make the service account's home: %w", err)
	}
	return a, nil
}

// slotFolders makes the folders above slot n's runner root and temporary
// folder: Root/slots and the slot's folder, which the agent owns, root on
// an installed Mac, open to every account, so that the service account
// reaches its folders in them. Root must be open to every account
// already.
func (s *Slots) slotFolders(n int) error {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(s.Root); err != nil || fi.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("the service account can't reach its folders through %s (%v): it must be open to every account", s.Root, err)
	}
	for _, dir := range []string{filepath.Dir(s.Folder(n)), s.Folder(n)} {
		if err := ownedDir(dir, os.Geteuid(), os.Getegid(), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// stateFolder makes Silkpool's state folder, which only its owner, root
// on an installed Mac, may open, and the folder above it, open to every
// account, so that the service account reaches its home.
func (s *Slots) stateFolder() error {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(s.state(), 0o700); err != nil {
		return err
	}
	if err := ownDir(s.state(), os.Geteuid(), 0o700); err != nil {
		return fmt.Errorf("the state folder isn't Silkpool's own: %w", err)
	}
	return nil
}

// emptyAccountFolders deletes the folders a new or adopted service
// account owns, which hold nothing known to be Silkpool's: its home, and
// each slot's runner root and temporary folder.
func (s *Slots) emptyAccountFolders() error {
	slots, err := s.slotNumbers()
	if err != nil {
		return err
	}
	errs := []error{os.RemoveAll(s.Home())}
	for _, n := range slots {
		errs = append(errs, os.RemoveAll(s.RunnerRoot(n)), os.RemoveAll(s.TempDir(n)))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("can't empty the service account's folders: %w", err)
	}
	return nil
}

// slotNumbers returns the numbers of the slots that have folders, in
// order.
func (s *Slots) slotNumbers() ([]int, error) {
	entries, err := names(filepath.Join(s.Root, "slots"))
	if err != nil {
		return nil, fmt.Errorf("can't read the slots' folder: %w", err)
	}
	var slots []int
	for _, e := range entries {
		// Silkpool names a slot's folder by its number alone (Folder).
		if n, err := strconv.Atoi(e); err == nil && n > 0 && strconv.Itoa(n) == e {
			slots = append(slots, n)
		}
	}
	slices.Sort(slots)
	return slots, nil
}

// accountError says why the service account, name, fails every slot
// closed, as Ensure's err has it: a *CantStart for an account install
// didn't make, a record Silkpool didn't make, or a membership someone
// added, or the error as it is.
func accountError(name string, err error) error {
	var unmade *roleaccount.Unmade
	var foreign *roleaccount.Foreign
	var added *roleaccount.Added
	switch {
	case errors.As(err, &unmade):
		return &CantStart{fmt.Sprintf("Silkpool's service account %s isn't as Silkpool's install made it: %s. "+
			"Install Silkpool again to make it.", name, unmade.Why)}
	case errors.As(err, &foreign) && foreign.Kind == roleaccount.Groups:
		return &CantStart{fmt.Sprintf("A group named %s is there, and Silkpool didn't make it. "+
			"Rename or delete it, then install Silkpool again, so that its service account can have the name.", name)}
	case errors.As(err, &foreign):
		return &CantStart{fmt.Sprintf("An account named %s is there, and Silkpool didn't make it. "+
			"Rename or delete it, then install Silkpool again, so that its service account can have the name.", name)}
	case errors.As(err, &added):
		// The groups' names are the machine's, not Silkpool's.
		return &CantStart{fmt.Sprintf("Silkpool's service account %s was added to %s. It can't be in other groups.",
			name, secret.Scrub(list(added.Groups)))}
	}
	return fmt.Errorf("can't check Silkpool's service account %s: %w", name, err)
}

// readLedger reads the ledger at path, or an empty one if there's none.
func readLedger(path string) (roleaccount.Ledger, error) {
	var l roleaccount.Ledger
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	return l, l.UnmarshalText(b)
}

// keepLedger keeps l as the service account's ledger.
func (s *Slots) keepLedger(l roleaccount.Ledger) error { return writeLedger(s.ledger(), l) }

// writeLedger writes l at path, through a new file renamed into place,
// so a crash leaves the old ledger or the new one.
func writeLedger(path string, l roleaccount.Ledger) error {
	b, err := l.MarshalText()
	if err != nil {
		return err
	}
	return writeRecord(path, strings.TrimSuffix(string(b), "\n"))
}

// Account returns the service account, once it has shown the account's
// record is the one Silkpool made: it holds the GeneratedUID the ledger
// owns (whose).
func (s *Slots) Account(ctx context.Context) (ServiceAccount, error) {
	a, err := s.whose(ctx)
	if err != nil {
		return ServiceAccount{}, err
	}
	if !a.proven {
		return ServiceAccount{}, fmt.Errorf("the service account %s isn't the one Silkpool made", s.Name)
	}
	return ServiceAccount{Name: s.Name, UID: a.uid, GID: a.gid}, nil
}

// accountIDs returns the service account's UID and GID, as Account
// proves them.
func (s *Slots) accountIDs(ctx context.Context) (uid, gid int, err error) {
	a, err := s.Account(ctx)
	return a.UID, a.GID, err
}

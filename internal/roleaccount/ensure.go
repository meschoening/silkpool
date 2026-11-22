package roleaccount

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The settings every role account Silkpool makes holds, as macOS gives
// its own role accounts: no shell, and the password *, which nothing can
// authenticate as. Its ID is from First to Last, below 501, so the
// sign-in window hides it, and it's both its UID and its own group's GID.
// Silkpool makes one, the machine's service account, and picks its ID
// once, at install (ADR 0007).
const (
	Shell      = "/usr/bin/false"
	NoPassword = "*"
	First      = 400
	Last       = 499
)

// A Spec is a role account as Silkpool keeps it: the name its user and
// its own group share, the marker each carries as its RealName, and its
// home.
type Spec struct {
	Name   string
	Marker string
	Home   string
	// Adopt lets Ensure take on a record of the name that carries the
	// marker, though the ledger doesn't name it: one Silkpool made and
	// then forgot, as when its state is lost. Without it, such a record
	// is Foreign.
	Adopt bool
	// Made says install made the account already, so Ensure only checks
	// it: it restores what Silkpool owns of it, but makes no record,
	// adopts none and gives it no ID but the ledger's. An account that
	// isn't as install left it is Unmade, and Ensure changes nothing.
	Made bool
}

// A Ledger is what Silkpool keeps of a role account it makes: the
// GeneratedUID it gave each of its records, whether it has shown each
// record is the one it made, and the ID it gave them. It's how Silkpool
// knows its own records, by more than their names.
type Ledger struct {
	User, Group Claim
	ID          int
	// Rebuild reports that the account's folders hold nothing known to be
	// Silkpool's, since it made or adopted a record of the account, and
	// that they haven't been emptied since. It's kept before the record
	// is made or adopted, so a crash or a failure after can't lose it,
	// and only the caller clears it, once it has emptied them.
	Rebuild bool
}

// A Claim is a record Silkpool made, or began to make, by the
// GeneratedUID it gave it first. A claim not yet owned is pending:
// Silkpool wrote the GeneratedUID, but hasn't shown the record holding
// it is the one it made, and not one of that name already there, which
// its first write would also give the GeneratedUID.
type Claim struct {
	GUID  string
	Owned bool
}

// An Account is a role account as Ensure leaves it.
type Account struct {
	Name string
	// ID is its UID and its own group's GID.
	ID int
	// User and Group are its records' GeneratedUIDs.
	User, Group string
	// Made reports that the ledger says the account's folders must be
	// emptied (Ledger.Rebuild): the caller empties them, then keeps the
	// ledger, as Ensure left it, with Rebuild cleared.
	Made bool
	// Ledger is the ledger as Ensure left it.
	Ledger Ledger
}

// Foreign is Ensure's error for a record of the account's name that
// Silkpool didn't make, which it leaves as it is.
type Foreign struct {
	Kind Kind
	Name string
}

func (e *Foreign) Error() string {
	return fmt.Sprintf("a record %s/%s is there that Silkpool didn't make", e.Kind, e.Name)
}

// Unmade is Ensure's error, for an account it only checks (Spec.Made),
// when the account isn't as install made it: the ledger doesn't own both
// its records and an ID, a record of it is missing, or its ID isn't the
// ledger's. Install makes it again.
type Unmade struct {
	Name string
	// Why says what's missing or moved.
	Why string
}

func (e *Unmade) Error() string {
	return fmt.Sprintf("the account %s isn't as Silkpool made it: %s", e.Name, e.Why)
}

// Added is Ensure's error for an account someone added to other groups
// directly, which it never removes. Groups are their names, sorted. A
// membership that comes only through a nested group isn't one, since
// every local account gets everyone, localaccounts, _lpoperator and the
// File Sharing groups that way.
type Added struct {
	Groups []string
}

func (e *Added) Error() string {
	return "the account was added to " + strings.Join(e.Groups, ", ")
}

// identity are the attributes that make a record an account or a group,
// or one that can be authenticated as. A record dscl has only just made
// holds none of them.
var identity = map[Kind][]string{
	Users: {"UniqueID", "PrimaryGroupID", "Password", "UserShell", "NFSHomeDirectory",
		"AuthenticationAuthority", "dsAttrTypeNative:ShadowHashData", "RealName"},
	Groups: {"PrimaryGroupID", "GroupMembership", "GroupMembers", "NestedGroups", "RealName", "Password"},
}

// credentials are the attributes a password lives in, which no role
// account of Silkpool's holds: its authentication authority and shadow
// hash, and the Kerberos and SRP keys and the password's writers that
// setting a password adds beside them, which stay when only the first
// two go, as they did on the Mac (#229).
var credentials = []string{"AuthenticationAuthority", "dsAttrTypeNative:ShadowHashData",
	"dsAttrTypeNative:HeimdalSRPKey", "dsAttrTypeNative:KerberosKeys", "dsAttrTypeNative:_writers_passwd"}

// Ensure makes the role account spec describes, or finds it as ledger
// has it, and gives it back what Silkpool owns of it, as ADR 0007 has
// it. It hands keep each change of the ledger before it acts on it, and
// stops if keep fails, so the ledger is never behind the directory.
//
// A record of the name is Silkpool's only if it holds the GeneratedUID
// the ledger claims for it, and, for a claim still pending, holds
// nothing else that makes it an identity; or, with spec.Adopt, if it
// carries the marker. Anything else is Foreign, and Ensure changes
// nothing. An account someone added to another group directly is Added,
// and Ensure changes nothing either: the operator removes the
// membership. A membership by name in a group can outlive the account
// it named, so a new account of that name would have it too.
//
// A missing record it makes: it claims a new GeneratedUID as pending,
// writes it as the record's first write, reads the record straight back,
// and claims it as owned only if it holds that GeneratedUID and nothing
// else that makes it an identity. Only then does the record get
// anything more. Its own group comes first, then the user. Each gets its
// marker and its password *, the user its shell and home, which are
// read back, then its ID and, last, its primary group.
//
// The ID is the account's as it stands, if it's in range, else the
// ledger's, as long as nothing else holds it, else the highest from Last
// down to First that nothing holds (Machine.Taken).
//
// It then reads both records back, and fails unless each holds exactly
// what Silkpool owns of it.
//
// With spec.Made, it only checks an account install made: before it
// changes anything, it fails with Unmade unless the ledger owns both
// records and keeps an ID, says the account's folders were emptied, both
// records are there, and the ID it would give the account is the
// ledger's.
func Ensure(ctx context.Context, m Machine, spec Spec, ledger Ledger, keep func(Ledger) error) (Account, error) {
	if err := checkName(spec.Name); err != nil {
		return Account{}, err
	}
	e := ensurer{ctx: ctx, m: m, spec: spec, ledger: ledger, keep: keep}
	return e.run()
}

type ensurer struct {
	ctx    context.Context
	m      Machine
	spec   Spec
	ledger Ledger
	keep   func(Ledger) error
}

// claim says whose record of kind is: ours, missing, or foreign. A
// pending claim the record bears out, or an adoption, it keeps as owned.
func (e *ensurer) claim(kind Kind, r Record, found bool) (ours bool, err error) {
	c := e.claimOf(kind)
	if !found {
		return false, nil
	}
	guid, _ := r.One("GeneratedUID")
	switch {
	case c.GUID != "" && guid == c.GUID && c.Owned:
		return true, nil
	case c.GUID != "" && guid == c.GUID:
		// Silkpool's first write, and nothing since: its own, made.
		if bare(kind, r) {
			e.ledger.Rebuild = true
			return true, e.own(kind, guid)
		}
		return false, &Foreign{kind, e.spec.Name}
	case e.spec.Adopt && guid != "" && r.Is("RealName", e.spec.Marker):
		e.ledger.Rebuild = true
		return true, e.own(kind, guid)
	}
	return false, &Foreign{kind, e.spec.Name}
}

func (e *ensurer) claimOf(kind Kind) *Claim {
	if kind == Users {
		return &e.ledger.User
	}
	return &e.ledger.Group
}

// own claims the record of kind holding guid as owned.
func (e *ensurer) own(kind Kind, guid string) error {
	*e.claimOf(kind) = Claim{GUID: guid, Owned: true}
	return e.keep(e.ledger)
}

// bare reports whether r holds nothing that makes it an identity.
func bare(kind Kind, r Record) bool {
	for _, a := range identity[kind] {
		if _, ok := r[a]; ok {
			return false
		}
	}
	return true
}

func (e *ensurer) run() (Account, error) {
	d, name := e.m.Directory, e.spec.Name
	group, gFound, err := d.Read(e.ctx, Groups, name)
	if err != nil {
		return Account{}, err
	}
	user, uFound, err := d.Read(e.ctx, Users, name)
	if err != nil {
		return Account{}, err
	}
	if err := e.made(gFound, uFound); err != nil {
		return Account{}, err
	}
	gOurs, err := e.claim(Groups, group, gFound)
	if err != nil {
		return Account{}, err
	}
	uOurs, err := e.claim(Users, user, uFound)
	if err != nil {
		return Account{}, err
	}
	// Refuse an added membership before changing anything.
	userGUID := ""
	if uOurs {
		userGUID = e.ledger.User.GUID
	}
	if err := e.memberships(userGUID, 0); err != nil {
		return Account{}, err
	}
	id, err := e.pickID(user, uOurs, group, gOurs)
	if err != nil {
		return Account{}, err
	}
	if e.spec.Made && id != e.ledger.ID {
		return Account{}, &Unmade{name, fmt.Sprintf("its ID would be %d, not the %d it was given", id, e.ledger.ID)}
	}
	if err := e.keepID(id); err != nil {
		return Account{}, err
	}
	if !gOurs {
		if err := e.make(Groups); err != nil {
			return Account{}, err
		}
	}
	if err := e.restore(Groups, id); err != nil {
		return Account{}, err
	}
	if !uOurs {
		if err := e.make(Users); err != nil {
			return Account{}, err
		}
	}
	if err := e.restore(Users, id); err != nil {
		return Account{}, err
	}
	for _, kind := range []Kind{Groups, Users} {
		r, found, err := d.Read(e.ctx, kind, name)
		if err != nil {
			return Account{}, err
		}
		if !found {
			return Account{}, fmt.Errorf("%s/%s is gone, as Silkpool finished it", kind, name)
		}
		if c := e.claimOf(kind); !r.Is("GeneratedUID", c.GUID) {
			return Account{}, fmt.Errorf("%s/%s holds GeneratedUID %q, not the %s Silkpool made it with", kind, name, r["GeneratedUID"], c.GUID)
		}
		if diff := e.drift(kind, r, id); len(diff) > 0 {
			return Account{}, fmt.Errorf("%s/%s didn't take Silkpool's settings: %s", kind, name, strings.Join(diff, ", "))
		}
	}
	if err := e.memberships(e.ledger.User.GUID, id); err != nil {
		return Account{}, err
	}
	return Account{Name: name, ID: id, User: e.ledger.User.GUID, Group: e.ledger.Group.GUID, Made: e.ledger.Rebuild, Ledger: e.ledger}, nil
}

// memberships fails with Added if a group names the account as a direct
// member, by its name, or by guid, its user's GeneratedUID, if it has
// one; or, for id, if a group other than its own has id as its GID, which
// would give the account that group too. Its own group, by its name and
// the ledger's GeneratedUID, doesn't count. Groups are read from every
// directory the search policy holds.
func (e *ensurer) memberships(guid string, id int) error {
	added, err := directGroups(e.ctx, e.m.Directory, e.spec.Name, guid, e.ledger.Group.GUID, id)
	if err != nil {
		return err
	}
	if len(added) > 0 {
		return &Added{Groups: added}
	}
	return nil
}

// directGroups returns the groups, sorted, in any directory the search
// policy holds, that have the account name as a member directly: by its
// name in GroupMembership, by guid, its user's GeneratedUID, in
// GroupMembers, or, for an id that isn't 0, by having it as their GID.
// The account's own group, by its name and own, its GeneratedUID,
// doesn't count. Neither does a group that only nests another, which
// NestedGroups says: that's how every local account is in everyone,
// localaccounts, _lpoperator and the File Sharing groups.
func directGroups(ctx context.Context, d Directory, name, guid, own string, id int) ([]string, error) {
	groups, err := d.All(ctx, "/Search", Groups, "GeneratedUID", "PrimaryGroupID", "GroupMembership", "GroupMembers")
	if err != nil {
		return nil, err
	}
	var added []string
	for _, g := range groups {
		if slices.Contains(g["RecordName"], name) && own != "" && g.Is("GeneratedUID", own) {
			continue
		}
		gid, hasID := idOf(g, "PrimaryGroupID")
		if slices.Contains(g["GroupMembership"], name) ||
			guid != "" && slices.Contains(g["GroupMembers"], guid) ||
			id != 0 && hasID && gid == id {
			added = append(added, g["RecordName"][0])
		}
	}
	slices.Sort(added)
	return slices.Compact(added), nil
}

// made fails with Unmade, for an account Ensure only checks
// (Spec.Made), unless the ledger owns both its records and keeps an ID,
// with the account's folders emptied since install made or adopted it
// (no Rebuild), and both records, the group's found as gFound and the
// user's as uFound, are there.
func (e *ensurer) made(gFound, uFound bool) error {
	if !e.spec.Made {
		return nil
	}
	l, name := e.ledger, e.spec.Name
	switch {
	case l == (Ledger{}):
		return &Unmade{name, "Silkpool has no record of making it"}
	case !l.User.Owned || !l.Group.Owned || l.ID == 0 || l.Rebuild:
		return &Unmade{name, "Silkpool didn't finish making it"}
	case !uFound && !gFound:
		return &Unmade{name, "its user and its group are missing"}
	case !uFound:
		return &Unmade{name, "its user is missing"}
	case !gFound:
		return &Unmade{name, "its group is missing"}
	}
	return nil
}

// pickID returns the ID the account keeps or gets. The caller keeps it
// in the ledger.
//
// A user of Silkpool's whose UID is in range keeps it, since its own
// processes may hold it: its group's GID may then be held by no other
// record, or Ensure fails rather than move the account. Any other user
// gets a UID that nothing else holds (Machine.Taken), its own records
// aside: the ledger's, if it's free, else the highest free one.
//
// It reads what holds each ID only if a record lacks the ID it needs.
func (e *ensurer) pickID(user Record, uOurs bool, group Record, gOurs bool) (int, error) {
	u, hasUID := idOf(user, "UniqueID")
	keeps := uOurs && hasUID && inRange(u)
	if keeps && gOurs && user.Is("PrimaryGroupID", strconv.Itoa(u)) && group.Is("PrimaryGroupID", strconv.Itoa(u)) {
		return u, nil
	}
	taken, err := e.m.Taken(e.ctx)
	if err != nil {
		return 0, err
	}
	if keeps {
		for _, use := range taken[u] {
			if (use.What == "user" || use.What == "group") && use.Name != e.spec.Name {
				return 0, fmt.Errorf("the account's ID %d is also %s %s's, so Silkpool can't give its group that GID", u, use.What, use.Name)
			}
		}
		return u, nil
	}
	if uOurs && hasUID {
		// Moving the account: a process with its UID would keep it, and
		// outlive what's done to the account by its UID from then on.
		for _, use := range taken[u] {
			if use.What == "process" {
				return 0, fmt.Errorf("the account's UID %d is out of range, but process %s holds it, so Silkpool can't move the account", u, use.Name)
			}
		}
	}
	id := 0
	if inRange(e.ledger.ID) && e.free(taken[e.ledger.ID]) {
		id = e.ledger.ID
	}
	for n := Last; n >= First && id == 0; n-- {
		if e.free(taken[n]) {
			id = n
		}
	}
	if id == 0 {
		return 0, fmt.Errorf("every ID from %d to %d is taken", First, Last)
	}
	return id, nil
}

// keepID keeps id in the ledger, if it isn't there already.
func (e *ensurer) keepID(id int) error {
	if id == e.ledger.ID {
		return nil
	}
	e.ledger.ID = id
	return e.keep(e.ledger)
}

// free reports whether nothing holds an ID but the account's own
// records. A process might be anyone's, even one of an account with the
// ID long deleted, and so might a folder.
func (e *ensurer) free(uses []Use) bool {
	for _, u := range uses {
		if !(u.What == "user" || u.What == "group") || u.Name != e.spec.Name {
			return false
		}
	}
	return true
}

// make makes the record of kind: it claims a new GeneratedUID as pending,
// writes it first, reads the record straight back, and claims it as
// owned only if it's the record it made.
func (e *ensurer) make(kind Kind) error {
	guid, err := newGUID()
	if err != nil {
		return err
	}
	c := e.claimOf(kind)
	*c = Claim{GUID: guid}
	e.ledger.Rebuild = true
	if err := e.keep(e.ledger); err != nil {
		return err
	}
	if err := e.m.set(e.ctx, kind, e.spec.Name, "GeneratedUID", guid); err != nil {
		return err
	}
	r, found, err := e.m.Read(e.ctx, kind, e.spec.Name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s/%s isn't there once made", kind, e.spec.Name)
	}
	if !r.Is("GeneratedUID", guid) || !bare(kind, r) {
		return &Foreign{kind, e.spec.Name}
	}
	return e.own(kind, guid)
}

// A change is one write that restores a setting Silkpool owns.
type change struct {
	attr, value string
	// unset deletes the attribute instead.
	unset bool
}

// changes are the writes that restore what Silkpool owns of the record
// of kind, r, with id: first what keeps it inert, then its ID, then, for
// a user, its primary group, last.
func (e *ensurer) changes(kind Kind, r Record, id int) (inert, ids []change) {
	want := func(list *[]change, attr, value string) {
		if !r.Is(attr, value) {
			*list = append(*list, change{attr: attr, value: value})
		}
	}
	want(&inert, "RealName", e.spec.Marker)
	want(&inert, "Password", NoPassword)
	if kind == Users {
		want(&inert, "UserShell", Shell)
		want(&inert, "NFSHomeDirectory", e.spec.Home)
		for _, a := range credentials {
			if _, ok := r[a]; ok {
				inert = append(inert, change{attr: a, unset: true})
			}
		}
		want(&ids, "UniqueID", strconv.Itoa(id))
	}
	want(&ids, "PrimaryGroupID", strconv.Itoa(id))
	return inert, ids
}

// drift names what the record of kind, r, holds that Silkpool doesn't
// own, with id.
func (e *ensurer) drift(kind Kind, r Record, id int) []string {
	inert, ids := e.changes(kind, r, id)
	var names []string
	for _, c := range append(inert, ids...) {
		names = append(names, c.attr)
	}
	return names
}

// restore gives the record of kind back what Silkpool owns of it: what
// keeps it inert, read back before anything more, then its ID.
func (e *ensurer) restore(kind Kind, id int) error {
	r, err := e.readOwn(kind)
	if err != nil {
		return err
	}
	inert, ids := e.changes(kind, r, id)
	if err := e.apply(kind, inert); err != nil {
		return err
	}
	if len(inert) > 0 && len(ids) > 0 {
		r, err := e.readOwn(kind)
		if err != nil {
			return err
		}
		if again, _ := e.changes(kind, r, id); len(again) > 0 {
			return fmt.Errorf("%s/%s didn't keep the settings that keep it inert", kind, e.spec.Name)
		}
	}
	return e.apply(kind, ids)
}

// readOwn reads the record of kind, and fails unless it's the one the
// ledger owns, holding its GeneratedUID: a record replaced since Ensure
// claimed it is Foreign, and gets nothing.
func (e *ensurer) readOwn(kind Kind) (Record, error) {
	r, found, err := e.m.Read(e.ctx, kind, e.spec.Name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%s/%s is gone, as Silkpool restores it", kind, e.spec.Name)
	}
	if c := e.claimOf(kind); !c.Owned || !r.Is("GeneratedUID", c.GUID) {
		return nil, &Foreign{kind, e.spec.Name}
	}
	return r, nil
}

func (e *ensurer) apply(kind Kind, changes []change) error {
	for _, c := range changes {
		var err error
		if c.unset {
			err = e.m.unset(e.ctx, kind, e.spec.Name, c.attr)
		} else {
			err = e.m.set(e.ctx, kind, e.spec.Name, c.attr, c.value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func inRange(id int) bool { return id >= First && id <= Last }

// idOf reads an attribute that holds one ID, and false if it doesn't
// hold exactly one decimal number.
func idOf(r Record, attr string) (int, bool) {
	if r == nil {
		return 0, false
	}
	return id(r, attr)
}

// newGUID returns a random GeneratedUID, as macOS writes them.
func newGUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("no randomness for a GeneratedUID")
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])), nil
}

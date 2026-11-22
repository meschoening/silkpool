// Package winaccount makes and keeps the machine's one service account
// on Windows, which every slot's runners run as (ADRs 0005 and 0007, as
// "Use one Silkpool service account per machine instead of per-slot
// accounts" amended them). Decided in "Build the Windows slot accounts".
//
// The account is local, with a random password that's never stored, a
// member of Users only. Batch is the only logon right it holds, which is
// all S4U needs, and every other logon right is denied it. It's hidden
// from the sign-in screen, and carries Silkpool's marker as its
// description. Disabling it is the kill switch for new logons, since a
// disabled account can't log on by S4U.
//
// Ensure makes it, or checks it for drift, before each runner and when
// the agent starts. Delete deletes it at uninstall. What the package
// reads and changes of Windows goes through a Machine: Local on Windows,
// and fakes in the tests, so the rules here are tested on every OS.
package winaccount

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/meschoening/silkpool/internal/secret"
)

// Service is the machine's service account as Silkpool keeps it.
var Service = Spec{Name: "silkpool", Marker: "Silkpool service account"}

// A Spec names an account Silkpool keeps: its name and the marker it
// carries as its description. Names are parameters, so privileged tests
// can use names of their own.
type Spec struct {
	Name   string
	Marker string
}

// UsersSID is BUILTIN\Users, the one group the account is a member of.
const UsersSID = "S-1-5-32-545"

// Rights are the account's logon rights, exactly: Batch, which S4U
// needs, and a denial of every other way to log on.
var Rights = []string{
	"SeBatchLogonRight",
	"SeDenyInteractiveLogonRight",
	"SeDenyNetworkLogonRight",
	"SeDenyRemoteInteractiveLogonRight",
	"SeDenyServiceLogonRight",
}

// A User is a local account as the machine shows it.
type User struct {
	SID     string
	Comment string
	// Disabled and LockedOut are its flags. Either keeps it from logging
	// on.
	Disabled, LockedOut bool
	// Expires reports that it has an expiry date.
	Expires bool
}

// A Group is a local group an account is a member of directly.
type Group struct {
	Name, SID string
}

// A Machine is what the package reads and changes of Windows' local
// accounts. Accounts are named by their names, and rights and processes
// by an account's SID, as a string.
type Machine interface {
	// Lookup finds the local account name, and reports whether there's
	// one.
	Lookup(name string) (User, bool, error)
	// Create makes the local account name, a member of Users only, with
	// comment as its description and password as its password.
	Create(name, comment, password string) error
	SetComment(name, comment string) error
	// Enable clears its disabled and locked-out flags, and Disable sets
	// the first.
	Enable(name string) error
	Disable(name string) error
	// NeverExpire clears its expiry date.
	NeverExpire(name string) error
	// Groups are the local groups it's a member of directly.
	Groups(name string) ([]Group, error)
	// AddToUsers adds the account sid to Users.
	AddToUsers(sid string) error
	// Rights are the rights and privileges held by the SID itself, not
	// through its groups, and AddRights, RemoveRights and
	// RemoveAllRights change them.
	Rights(sid string) ([]string, error)
	AddRights(sid string, rights []string) error
	RemoveRights(sid string, rights []string) error
	RemoveAllRights(sid string) error
	// Hidden reports whether the sign-in screen hides name, and Hide and
	// Unhide change it.
	Hidden(name string) (bool, error)
	Hide(name string) error
	Unhide(name string) error
	// Processes counts the processes running as sid.
	Processes(sid string) (int, error)
	// DeleteProfile deletes sid's profile, if it has one, and Delete the
	// account name, if there is one.
	DeleteProfile(sid string) error
	Delete(name string) error
}

// Foreign is the error for an account with the spec's name that
// Silkpool didn't make: one it didn't record, without the marker. It's
// left as it is.
type Foreign struct {
	Name string
}

func (e *Foreign) Error() string {
	return fmt.Sprintf("an account named %s is on this machine, and Silkpool didn't make it", e.Name)
}

// Added is the error for an account someone added to groups other than
// Users, named here, in the machine's order. Silkpool never removes it
// from them.
type Added struct {
	Name   string
	Groups []string
}

func (e *Added) Error() string {
	return fmt.Sprintf("the account %s was added to %s", e.Name, strings.Join(e.Groups, ", "))
}

// Busy is Delete's error while processes of the account run.
type Busy struct {
	Name      string
	Processes int
}

func (e *Busy) Error() string {
	return fmt.Sprintf("%d processes of the account %s are running", e.Processes, e.Name)
}

// Ensure makes the account spec names, or checks it for drift, and
// returns its SID. recorded is the SID the caller recorded for it, or ""
// for none; the caller records what Ensure returns.
//
//   - An account that isn't there it makes, with a random password it
//     never keeps, and the rest of what Silkpool owns of it.
//   - An account with the name that isn't the one recorded it adopts only
//     if it carries the marker. Otherwise it fails with *Foreign.
//   - An account in a group other than Users fails with *Added, naming
//     the groups. Silkpool never removes it from them.
//
// Neither failure changes anything. Otherwise it restores silently what
// Silkpool owns of the account: the marker as its description, enabled
// and not locked out, no expiry date, a member of Users, exactly Rights
// as the rights it holds itself, and hidden from the sign-in screen. It
// then reads the account back, and fails unless all of that holds.
func Ensure(m Machine, spec Spec, recorded string) (string, error) {
	if err := checkName(spec.Name); err != nil {
		return "", err
	}
	u, ok, err := m.Lookup(spec.Name)
	if err != nil {
		return "", err
	}
	switch {
	case !ok:
		if err := m.Create(spec.Name, spec.Marker, password()); err != nil {
			return "", fmt.Errorf("can't make the account %s: %w", spec.Name, err)
		}
		if u, ok, err = m.Lookup(spec.Name); err != nil {
			return "", err
		} else if !ok {
			return "", fmt.Errorf("the account %s isn't there after Silkpool made it", spec.Name)
		}
	case u.SID != recorded && u.Comment != spec.Marker:
		return "", &Foreign{spec.Name}
	}
	v, err := read(m, spec.Name, u)
	if err != nil {
		return "", err
	}
	if others := v.others(); len(others) > 0 {
		return "", &Added{spec.Name, others}
	}
	if err := restore(m, spec, v); err != nil {
		return "", fmt.Errorf("can't restore the account %s: %w", spec.Name, err)
	}
	u, ok, err = m.Lookup(spec.Name)
	if err == nil && !ok {
		err = fmt.Errorf("it's gone")
	}
	if err == nil {
		v, err = read(m, spec.Name, u)
	}
	if err != nil {
		return "", fmt.Errorf("can't read the account %s back: %w", spec.Name, err)
	}
	if others := v.others(); len(others) > 0 {
		return "", &Added{spec.Name, others}
	}
	if d := v.drift(spec); len(d) > 0 {
		return "", fmt.Errorf("the account %s didn't take Silkpool's settings: %s", spec.Name, strings.Join(d, ", "))
	}
	return u.SID, nil
}

// A view is all Ensure reads of an account.
type view struct {
	User
	groups []Group
	rights []string
	hidden bool
}

func read(m Machine, name string, u User) (view, error) {
	v := view{User: u}
	var err error
	if v.groups, err = m.Groups(name); err != nil {
		return v, fmt.Errorf("can't read the groups of %s: %w", name, err)
	}
	if v.rights, err = m.Rights(u.SID); err != nil {
		return v, fmt.Errorf("can't read the rights of %s: %w", name, err)
	}
	if v.hidden, err = m.Hidden(name); err != nil {
		return v, fmt.Errorf("can't read whether the sign-in screen hides %s: %w", name, err)
	}
	return v, nil
}

// others are the names of the groups other than Users that v is in.
func (v view) others() []string {
	var names []string
	for _, g := range v.groups {
		if g.SID != UsersSID {
			names = append(names, g.Name)
		}
	}
	return names
}

func (v view) inUsers() bool {
	return slices.ContainsFunc(v.groups, func(g Group) bool { return g.SID == UsersSID })
}

// missing are the Rights v doesn't hold, and extra the rights it holds
// that aren't Rights.
func (v view) missing() []string {
	var r []string
	for _, want := range Rights {
		if !slices.Contains(v.rights, want) {
			r = append(r, want)
		}
	}
	return r
}

func (v view) extra() []string {
	var r []string
	for _, have := range v.rights {
		if !slices.Contains(Rights, have) {
			r = append(r, have)
		}
	}
	return r
}

// drift names each setting Silkpool owns that v doesn't hold.
func (v view) drift(spec Spec) []string {
	var d []string
	if v.Comment != spec.Marker {
		d = append(d, "its description")
	}
	if v.Disabled {
		d = append(d, "disabled")
	}
	if v.LockedOut {
		d = append(d, "locked out")
	}
	if v.Expires {
		d = append(d, "an expiry date")
	}
	if !v.inUsers() {
		d = append(d, "not in Users")
	}
	if m := v.missing(); len(m) > 0 {
		d = append(d, "without "+strings.Join(m, ", "))
	}
	if e := v.extra(); len(e) > 0 {
		d = append(d, "with "+strings.Join(e, ", "))
	}
	if !v.hidden {
		d = append(d, "shown on the sign-in screen")
	}
	return d
}

// restore gives v back each setting Silkpool owns that it lacks.
func restore(m Machine, spec Spec, v view) error {
	if v.Comment != spec.Marker {
		if err := m.SetComment(spec.Name, spec.Marker); err != nil {
			return err
		}
	}
	if v.Disabled || v.LockedOut {
		if err := m.Enable(spec.Name); err != nil {
			return err
		}
	}
	if v.Expires {
		if err := m.NeverExpire(spec.Name); err != nil {
			return err
		}
	}
	if !v.inUsers() {
		if err := m.AddToUsers(v.SID); err != nil {
			return err
		}
	}
	if r := v.missing(); len(r) > 0 {
		if err := m.AddRights(v.SID, r); err != nil {
			return err
		}
	}
	if r := v.extra(); len(r) > 0 {
		if err := m.RemoveRights(v.SID, r); err != nil {
			return err
		}
	}
	if !v.hidden {
		if err := m.Hide(spec.Name); err != nil {
			return err
		}
	}
	return nil
}

// Disable disables the account spec names, the kill switch for new
// logons, if it's the one recorded as sid. An account that isn't there
// can't log on, so that's done already. Ensure enables it again.
func Disable(m Machine, spec Spec, sid string) error {
	u, ok, err := m.Lookup(spec.Name)
	switch {
	case err != nil:
		return err
	case !ok:
		return nil
	case u.SID != sid:
		return &Foreign{spec.Name}
	}
	return m.Disable(spec.Name)
}

// Enable enables the account spec names again, if it's the one recorded
// as sid, as the account-wide clean does once it's done: it clears the
// disabled and locked-out flags, and nothing else. An account that isn't
// there is left to Ensure, which makes it again.
func Enable(m Machine, spec Spec, sid string) error {
	u, ok, err := m.Lookup(spec.Name)
	switch {
	case err != nil:
		return err
	case !ok:
		return nil
	case u.SID != sid:
		return &Foreign{spec.Name}
	}
	return m.Enable(spec.Name)
}

// Delete deletes the account spec names, at uninstall: the account, its
// profile, the rights it holds, and its entry hiding it from the sign-in
// screen. recorded is the SID the caller recorded for it, or "".
//
// It deletes only an account Silkpool made: the one recorded, or one
// carrying the marker. It fails with *Foreign for any other, and with
// *Busy while a process of the account runs, deleting nothing either
// way, as CheckDelete finds. An account that's gone already, but was
// recorded, still has its profile and rights deleted, so a Delete cut
// short can run again.
func Delete(m Machine, spec Spec, recorded string) error {
	sid, there, err := deletable(m, spec, recorded)
	if err != nil || sid == "" {
		return err
	}
	if there {
		if err := m.Delete(spec.Name); err != nil {
			return fmt.Errorf("can't delete the account %s: %w", spec.Name, err)
		}
	}
	if err := m.DeleteProfile(sid); err != nil {
		return fmt.Errorf("can't delete the profile of %s: %w", spec.Name, err)
	}
	if err := m.RemoveAllRights(sid); err != nil {
		return fmt.Errorf("can't remove the rights of %s: %w", spec.Name, err)
	}
	return m.Unhide(spec.Name)
}

// CheckDelete returns the error Delete would fail with before it deletes
// anything, changing nothing: *Foreign for an account Silkpool didn't
// make, and *Busy while a process of the account runs. An uninstall
// asks it before it changes anything, so that it refuses whole.
func CheckDelete(m Machine, spec Spec, recorded string) error {
	_, _, err := deletable(m, spec, recorded)
	return err
}

// deletable finds the account Delete would delete: its SID, or "" if
// there's nothing to delete, and whether the account is there. It fails
// as CheckDelete says.
func deletable(m Machine, spec Spec, recorded string) (sid string, there bool, err error) {
	if err := checkName(spec.Name); err != nil {
		return "", false, err
	}
	u, ok, err := m.Lookup(spec.Name)
	if err != nil {
		return "", false, err
	}
	sid = recorded
	if ok {
		if u.SID != recorded && u.Comment != spec.Marker {
			return "", false, &Foreign{spec.Name}
		}
		sid = u.SID
	}
	if sid == "" {
		return "", false, nil
	}
	n, err := m.Processes(sid)
	if err != nil {
		return "", false, fmt.Errorf("can't tell whether processes of %s are running: %w", spec.Name, err)
	}
	if n > 0 {
		return "", false, &Busy{spec.Name, n}
	}
	return sid, ok, nil
}

// Detail is a failure of Ensure as a slot's Slot can't start shows it:
// Silkpool's own words, with any text from the machine scrubbed.
func Detail(err error) string {
	var foreign *Foreign
	var added *Added
	switch {
	case errors.As(err, &foreign):
		return fmt.Sprintf("An account named %s is already on this machine, and Silkpool didn't make it. "+
			"Rename or delete it, so that Silkpool's service account can have its name.", foreign.Name)
	case errors.As(err, &added):
		// The groups' names are the machine's, not Silkpool's.
		return fmt.Sprintf("Silkpool's service account %s was added to %s. It can't be in groups other than Users, "+
			"so no slot on this machine can start until it's removed from them.",
			added.Name, secret.Scrub(sentence(added.Groups)))
	}
	return "Silkpool couldn't check its service account: " + secret.Scrub(err.Error())
}

// sentence joins names as a sentence does: "a", "a and b", "a, b and c".
func sentence(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// checkName refuses a name Windows wouldn't take for a local account: up
// to 20 characters, none of them a control character or one of
// "/\[]:;|=,+*?<>@, and not only dots and spaces.
func checkName(name string) error {
	bad := name == "" || len([]rune(name)) > 20 || strings.Trim(name, ". ") == "" ||
		strings.ContainsAny(name, `"/\[]:;|=,+*?<>@`) || strings.ContainsFunc(name, unicode.IsControl)
	if bad {
		return fmt.Errorf("%q isn't a name Windows takes for a local account", name)
	}
	return nil
}

// password returns a random password that meets Windows' complexity
// rule, three kinds of character of the five, with about 260 bits from
// crypto/rand: upper case, lower case, and a symbol.
func password() string {
	return rand.Text() + strings.ToLower(rand.Text()) + "-"
}

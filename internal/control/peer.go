package control

import (
	"errors"
	"net"
	"os/user"
	"slices"
	"strconv"
)

// errNoPeerCredentials fails the peer check on an OS whose control
// channel isn't built yet.
var errNoPeerCredentials = errors.New("control: this OS's control channel isn't built yet")

// peerUID returns the uid of the process at the other end of c, from
// the socket's peer credentials. Each OS's file sets it.
var peerUID = func(net.Conn) (int, error) { return 0, errNoPeerCredentials }

// An Account is the OS account a peer runs as, as the peer check sees
// it.
type Account struct {
	// Name is the account's name, or its uid if it has none.
	Name string
	// Admin reports that it's root or a member of one of the
	// AdminGroups.
	Admin bool
	// Elevated reports that it's uid 0, which enroll, remove and
	// set-hub-url need.
	Elevated bool
}

// LookupAccount returns the account uid names, from the OS's account
// database. Root is an administrator, and so is a member of one of the
// AdminGroups, as its primary group or a supplementary one.
func LookupAccount(uid int) (Account, error) {
	id := strconv.Itoa(uid)
	u, err := user.LookupId(id)
	if err != nil {
		var unknown user.UnknownUserIdError
		if errors.As(err, &unknown) {
			return Account{Name: id, Admin: uid == 0, Elevated: uid == 0}, nil
		}
		return Account{}, err
	}
	a := Account{Name: u.Username, Admin: uid == 0, Elevated: uid == 0}
	groups, err := u.GroupIds()
	if err != nil {
		return a, err
	}
	for _, gid := range groups {
		g, err := user.LookupGroupId(gid)
		if err == nil && slices.Contains(AdminGroups, g.Name) {
			a.Admin = true
		}
	}
	return a, nil
}

package slot

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/meschoening/silkpool/internal/secret"
)

// A found is an account as the local account files show it.
type found struct {
	exists bool
	uid    int
	// gid is its primary group's ID, and group the ID of the group named
	// like the account, or -1 if there's none.
	gid, group int
	gecos      string
	home       string
	// password and expire are its shadow entry's password and expiry
	// date fields.
	password, expire string
	// groups are the other groups that list it as a member, in the
	// group file's order.
	groups []string
}

// readAccount reads the account name from the passwd, shadow and group
// files.
func readAccount(name string, passwd, shadow, group io.Reader) (found, error) {
	f := found{group: -1}
	err := entries(passwd, func(e []string) error {
		if len(e) < 7 || e[0] != name {
			return nil
		}
		uid, err1 := strconv.Atoi(e[2])
		gid, err2 := strconv.Atoi(e[3])
		if err := errors.Join(err1, err2); err != nil {
			return fmt.Errorf("the passwd entry of %s: %w", name, err)
		}
		f.exists, f.uid, f.gid, f.gecos, f.home = true, uid, gid, e[4], e[5]
		return nil
	})
	if err != nil {
		return f, err
	}
	err = entries(shadow, func(e []string) error {
		if len(e) >= 8 && e[0] == name {
			f.password, f.expire = e[1], e[7]
		}
		return nil
	})
	if err != nil {
		return f, err
	}
	err = entries(group, func(e []string) error {
		if len(e) < 4 {
			return nil
		}
		if e[0] == name {
			gid, err := strconv.Atoi(e[2])
			if err != nil {
				return fmt.Errorf("the group entry of %s: %w", name, err)
			}
			f.group = gid
			return nil
		}
		for m := range strings.SplitSeq(e[3], ",") {
			if m == name {
				f.groups = append(f.groups, e[0])
			}
		}
		return nil
	})
	return f, err
}

// entries calls each for each line of an account file, split at its
// colons.
func entries(r io.Reader, each func([]string) error) error {
	s := bufio.NewScanner(r)
	for s.Scan() {
		if err := each(strings.Split(s.Text(), ":")); err != nil {
			return err
		}
	}
	return s.Err()
}

// A plan is what it takes to make the service account what Silkpool
// owns.
type plan struct {
	// create makes the account, which doesn't exist.
	create bool
	// addGroup makes the account's own group, which doesn't exist.
	addGroup bool
	// fix holds usermod's flags that restore the settings Silkpool owns.
	fix []string
	// rebuild empties the account's home: the account wasn't recorded,
	// so who knows what's in it.
	rebuild bool
	// refused is why every slot fails closed, or nil.
	refused *CantStart
}

// expired is the expiry date Silkpool gives the service account: the day
// after the epoch, so it's always expired.
const expired = "1"

// serviceMarker is the description that marks the service account as
// Silkpool's.
const serviceMarker = "Silkpool service account"

// judge plans the service account from f, the account files' view of it,
// and record, the UID its record holds, or 0 for none.
func (l Layout) judge(f found, record int) plan {
	name := l.Account()
	if !f.exists {
		return plan{create: true}
	}
	var p plan
	if f.uid != record {
		if f.gecos != serviceMarker {
			p.refused = &CantStart{fmt.Sprintf("An account named %s already exists, and Silkpool didn't make it. "+
				"Rename or delete it, so that Silkpool's slots can run as it.", name)}
			return p
		}
		p.rebuild = true
	}
	if len(f.groups) > 0 {
		// The groups' names are the machine's, not Silkpool's.
		p.refused = &CantStart{fmt.Sprintf("Silkpool's service account was added to %s. "+
			"It can't be in other groups, so no slot can start.", secret.Scrub(list(f.groups)))}
		return p
	}
	if f.gecos != serviceMarker {
		p.fix = append(p.fix, "--comment", serviceMarker)
	}
	if f.expire != expired {
		p.fix = append(p.fix, "--expiredate", expired)
	}
	if f.group < 0 {
		p.addGroup = true
	}
	if f.group < 0 || f.gid != f.group {
		p.fix = append(p.fix, "--gid", name)
	}
	if f.home != l.Home() {
		p.fix = append(p.fix, "--home", l.Home())
	}
	if !strings.HasPrefix(f.password, "!") {
		p.fix = append(p.fix, "--password", "!")
	}
	return p
}

// PrepareAccount readies the machine's service account, which every slot
// runs as (#214). The package's postinstall makes it, and the agent
// checks it for drift when it starts and before each runner. It makes
// the account if it isn't there, as ADR 0007 has it: a system user with
// its own group, expired and password-locked, with a normal shell for
// jobs, its home in Root/home, and Silkpool's marker as its description.
// It records the account in Root, by its UID.
//
// After that it checks the account for drift. It restores the settings
// Silkpool owns silently, and makes a missing account again. It fails
// closed with a *CantStart, which closes every slot, when the account
// was added to another group, which it never removes, and when an
// account with the service account's name that it didn't record lacks
// the marker. An unrecorded account with the marker is adopted, and its
// home emptied, as is a new one's.
//
// It changes the account, so only one may run at a time on a machine:
// Slots.Ready holds a lock around it, and the package runs it before the
// agent starts.
func (l Layout) PrepareAccount() error {
	if err := l.openRoot(); err != nil {
		return err
	}
	name := l.Account()
	f, err := readLocalAccount(name)
	if err != nil {
		return err
	}
	p := l.judge(f, l.recorded())
	if p.refused != nil {
		return p.refused
	}
	if p.create {
		if err := l.create(f); err != nil {
			return err
		}
		if f, err = readLocalAccount(name); err != nil {
			return err
		}
		p = l.judge(f, f.uid)
		p.rebuild = true
	}
	if p.addGroup {
		if err := system("groupadd", "--system", name); err != nil {
			return err
		}
	}
	if len(p.fix) > 0 {
		if err := system("usermod", append(p.fix, name)...); err != nil {
			return err
		}
	}
	if p.rebuild {
		if err := os.RemoveAll(l.Home()); err != nil {
			return err
		}
	}
	if f, err = readLocalAccount(name); err != nil {
		return err
	}
	if q := l.judge(f, f.uid); q.refused != nil || q.create || q.addGroup || len(q.fix) > 0 {
		return fmt.Errorf("the service account didn't take Silkpool's settings: it still needs %+v", q)
	}
	if err := ownedDir(l.Home(), f.uid, f.gid, 0o700); err != nil {
		return fmt.Errorf("can't make the service account's home: %w", err)
	}
	return l.record(f.uid)
}

// openRoot makes Root, if it isn't there, and checks it's open to every
// account, so that the service account reaches its home and the slots'
// folders.
func (l Layout) openRoot() error {
	if err := os.MkdirAll(l.Root, 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(l.Root); err != nil || fi.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("the service account can't reach its home through %s (%v): it must be open to every account", l.Root, err)
	}
	return nil
}

// create makes the service account, which f says doesn't exist.
func (l Layout) create(f found) error {
	args := []string{"--system", "--no-create-home", "--home-dir", l.Home(),
		"--shell", shell(), "--comment", serviceMarker}
	if f.group >= 0 {
		// Its group outlived it.
		args = append(args, "--gid", l.Account())
	} else {
		args = append(args, "--user-group")
	}
	return system("useradd", append(args, l.Account())...)
}

// shell is the normal shell the service account keeps, so that jobs that
// read $SHELL work: bash where there is one.
func shell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// recordFile is the file in Root that records the service account, by
// the UID Silkpool made or adopted it with. Root is root's, so no slot
// can change it.
const recordFile = "account"

// recorded returns the UID the service account's record holds, or 0 if
// there's none.
func (l Layout) recorded() int {
	b, err := os.ReadFile(filepath.Join(l.Root, recordFile))
	if err != nil {
		return 0
	}
	uid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return uid
}

// record records uid as the service account's.
func (l Layout) record(uid int) error {
	if l.recorded() == uid {
		return nil
	}
	if err := writeRecord(filepath.Join(l.Root, recordFile), strconv.Itoa(uid)); err != nil {
		return fmt.Errorf("can't record the service account: %w", err)
	}
	return nil
}

// readLocalAccount reads the account name from the local account files.
func readLocalAccount(name string) (found, error) {
	var files [3]*os.File
	for i, path := range []string{"/etc/passwd", "/etc/shadow", "/etc/group"} {
		f, err := os.Open(path)
		if err != nil {
			return found{}, err
		}
		defer f.Close()
		files[i] = f
	}
	return readAccount(name, files[0], files[1], files[2])
}

// system runs a system command, and fails with its output.
func system(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

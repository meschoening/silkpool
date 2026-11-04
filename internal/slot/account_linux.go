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

// A found is a slot account as the local account files show it.
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

// A plan is what it takes to make a slot account what Silkpool owns.
type plan struct {
	// create makes the account, which doesn't exist.
	create bool
	// addGroup makes the account's own group, which doesn't exist.
	addGroup bool
	// fix holds usermod's flags that restore the settings Silkpool owns.
	fix []string
	// rebuild empties the slot's folders: the account wasn't recorded,
	// so who knows what's in them.
	rebuild bool
	// refused is why the slot fails closed, or nil.
	refused *CantStart
}

// CantStart is a slot that fails closed with Slot can't start, until the
// operator changes what its Detail names.
type CantStart struct {
	// Detail is the plain detail the slot shows: Silkpool's own words,
	// with any text from the machine it names scrubbed.
	Detail string
}

func (e *CantStart) Error() string { return e.Detail }

// marker is the description that marks slot n's account as Silkpool's.
func marker(n int) string { return fmt.Sprintf("Silkpool slot %d", n) }

// expired is the expiry date Silkpool gives every slot account: the day
// after the epoch, so it's always expired.
const expired = "1"

// judge plans slot n's account from f, the account files' view of it,
// and record, the UID its record holds, or 0 for none.
func (l Layout) judge(n int, f found, record int) plan {
	name := l.Account(n)
	if !f.exists {
		return plan{create: true}
	}
	var p plan
	if f.uid != record {
		if f.gecos != marker(n) {
			p.refused = &CantStart{fmt.Sprintf("An account named %s already exists, and Silkpool didn't make it. "+
				"Rename or delete it, so that slot %d can have it.", name, n)}
			return p
		}
		p.rebuild = true
	}
	if len(f.groups) > 0 {
		// The groups' names are the machine's, not Silkpool's.
		p.refused = &CantStart{fmt.Sprintf("Slot %d's account was added to %s. Slot accounts can't be in other groups.",
			n, secret.Scrub(list(f.groups)))}
		return p
	}
	if f.gecos != marker(n) {
		p.fix = append(p.fix, "--comment", marker(n))
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
	if f.home != l.Home(n) {
		p.fix = append(p.fix, "--home", l.Home(n))
	}
	if !strings.HasPrefix(f.password, "!") {
		p.fix = append(p.fix, "--password", "!")
	}
	return p
}

// list joins names as a sentence does: "a", "a and b", "a, b and c".
func list(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// Prepare readies slot n's account and folders, before each runner and
// when the agent starts. It makes the account the first time, as ADR
// 0007 has it: a system user with its own group, expired and
// password-locked, with a normal shell for jobs, its home in the slot's
// folder, and Silkpool's marker as its description. It records the
// account beside the slot's folders, by its UID.
//
// After that it checks the account for drift. It restores the settings
// Silkpool owns silently, and makes a missing account again. It fails
// closed with a *CantStart when the account was added to another group,
// which it never removes, and when an account with the slot's name that
// it didn't record lacks the marker. An unrecorded account with the
// marker is adopted, and rebuilt with empty folders, as is a new one.
func (s *Slots) Prepare(n int) error {
	if err := s.folders(n); err != nil {
		return err
	}
	name := s.Account(n)
	f, err := readLocalAccount(name)
	if err != nil {
		return err
	}
	p := s.judge(n, f, s.recorded(n))
	if p.refused != nil {
		return p.refused
	}
	if p.create {
		if err := s.create(n, f); err != nil {
			return err
		}
		if f, err = readLocalAccount(name); err != nil {
			return err
		}
		p = s.judge(n, f, f.uid)
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
		if err := errors.Join(os.RemoveAll(s.Home(n)), os.RemoveAll(s.RunnerRoot(n))); err != nil {
			return err
		}
	}
	if f, err = readLocalAccount(name); err != nil {
		return err
	}
	if q := s.judge(n, f, f.uid); q.refused != nil || q.create || q.addGroup || len(q.fix) > 0 {
		return fmt.Errorf("slot %d's account didn't take Silkpool's settings: it still needs %+v", n, q)
	}
	if err := ownedDir(s.Home(n), f.uid, f.gid, 0o700); err != nil {
		return fmt.Errorf("can't make slot %d's home: %w", n, err)
	}
	return s.record(n, f.uid)
}

// folders makes the folders above slot n's home: Root/slots and the
// slot's folder, which root owns, open to every account so that the slot
// account reaches its own folders. Root must be open to every account
// already.
func (s *Slots) folders(n int) error {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(s.Root); err != nil || fi.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("slot accounts can't reach their folders through %s (%v): it must be open to every account", s.Root, err)
	}
	for _, dir := range []string{filepath.Dir(s.Folder(n)), s.Folder(n)} {
		if err := ownedDir(dir, 0, 0, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ownedDir makes dir, if it isn't there, and gives it to uid and gid with
// mode perm. It refuses a link or a file in its place.
func ownedDir(dir string, uid, gid int, perm os.FileMode) error {
	if err := os.Mkdir(dir, perm); err != nil && !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s isn't a directory", dir)
	}
	return errors.Join(os.Lchown(dir, uid, gid), os.Chmod(dir, perm))
}

// create makes slot n's account, which f says doesn't exist.
func (s *Slots) create(n int, f found) error {
	args := []string{"--system", "--no-create-home", "--home-dir", s.Home(n),
		"--shell", shell(), "--comment", marker(n)}
	if f.group >= 0 {
		// Its group outlived it.
		args = append(args, "--gid", s.Account(n))
	} else {
		args = append(args, "--user-group")
	}
	return system("useradd", append(args, s.Account(n))...)
}

// shell is the normal shell slot accounts keep, so that jobs that read
// $SHELL work: bash where there is one.
func shell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// recordFile is the file in a slot's folder that records its account,
// by the UID Silkpool made or adopted it with.
const recordFile = "account"

// recorded returns the UID slot n's record holds, or 0 if there's none.
func (s *Slots) recorded(n int) int {
	b, err := os.ReadFile(filepath.Join(s.Folder(n), recordFile))
	if err != nil {
		return 0
	}
	uid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return uid
}

// record records uid as slot n's account.
func (s *Slots) record(n, uid int) error {
	if s.recorded(n) == uid {
		return nil
	}
	if err := writeRecord(filepath.Join(s.Folder(n), recordFile), strconv.Itoa(uid)); err != nil {
		return fmt.Errorf("can't record slot %d's account: %w", n, err)
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

//go:build unix

package slot

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/meschoening/silkpool/internal/secret"
)

// CantStart is a slot that fails closed with Slot can't start, until the
// operator changes what its Detail names.
type CantStart struct {
	// Detail is the plain detail the slot shows: Silkpool's own words,
	// with any text from the machine it names scrubbed.
	Detail string
}

func (e *CantStart) Error() string { return e.Detail }

// list joins names as a sentence does: "a", "a and b", "a, b and c".
func list(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
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

// ownDir checks that path is a directory, not a link, that uid owns,
// with mode perm.
func ownDir(path string, uid int, perm os.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, _ := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || st == nil || int(st.Uid) != uid || fi.Mode().Perm() != perm {
		return fmt.Errorf("%s isn't a directory of its own with mode %o", path, perm)
	}
	return nil
}

// writeConfig writes config to a new file at path that only uid and gid
// may read. It never follows a link there, and replaces a file an
// earlier attempt left.
func writeConfig(path string, config secret.Value, uid, gid int) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(config.Reveal())
	err = errors.Join(err, f.Chown(uid, gid), f.Close())
	if err != nil {
		os.Remove(path)
	}
	return err
}

// noCrontab reports whether crontab's output says the account has none.
func noCrontab(out []byte) bool { return bytes.Contains(out, []byte("no crontab for")) }

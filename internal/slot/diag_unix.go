//go:build unix

package slot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/node"
)

// copyLogs copies the _diag logs of slot n's last runner, as its record
// names it, into the runner's folder in the agent's logs, beside its
// listener's output (copyDiag), and returns the run its Worker log
// names. uid is the service account's, which must own each log. Then it
// prunes the runners' folders, as of now. Without a recorded runner or
// an account, there's nothing to copy.
func (s *Slots) copyLogs(n, uid int, now time.Time) (node.Run, error) {
	runner := s.recordedRunner(n)
	if runner == "" || uid <= 0 {
		return node.Run{}, nil
	}
	run, err := copyDiag(s.RunnerRoot(n), s.RunnerLogs(runner), uid, diagCap)
	if err != nil {
		return node.Run{}, err
	}
	return run, s.pruneLogs(now)
}

// logOwner is the owner a log in _diag must have for the reset to copy
// it: the service account's UID.
type logOwner = int

// createCopy makes the file path, or empties it, for a log's copy, which
// is root's, mode 0600. It follows no link there.
func createCopy(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW, 0o600)
}

// openDiag opens the _diag folder in the runner root root, following no
// link, or returns nil if either isn't there or isn't a folder.
func openDiag(root string) (*os.File, error) {
	dirFlags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	rfd, err := unix.Open(root, dirFlags, 0)
	if notThere(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("can't open %s: %w", root, err)
	}
	defer unix.Close(rfd)
	dfd, err := unix.Openat(rfd, "_diag", dirFlags, 0)
	if notThere(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("can't open %s: %w", filepath.Join(root, "_diag"), err)
	}
	return os.NewFile(uintptr(dfd), filepath.Join(root, "_diag")), nil
}

// notThere reports whether err says a path is missing, a link or not a
// folder: in a runner root, each is nothing to copy.
func notThere(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

// openLog opens the log name in the folder diag, following no link, and
// returns it with its size, or nil if it isn't a plain file that uid
// owns. A FIFO doesn't block it.
func openLog(diag *os.File, name string, uid logOwner) (*os.File, int64, error) {
	fd, err := unix.Openat(int(diag.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENXIO) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != uid {
		unix.Close(fd)
		return nil, 0, nil
	}
	return os.NewFile(uintptr(fd), name), st.Size, nil
}

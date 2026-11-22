package slot

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/secret"
)

// A job's output on the Mac follows #238's probe (#241). launchd opens a
// job's StandardOutPath and StandardErrorPath, and enters its
// WorkingDirectory, after it has switched to the job's UserName. A job
// whose account can't reach them exits 78 (EX_CONFIG) before its program
// starts, and launchd logs nothing that says why. So each job that runs
// as the service account, a slot's or the exposure check's, gets an
// output file of its own:
//
//   - The file is in the output folder, Root/output, which root owns with
//     mode 0711, so the account may open a file there by its name, but
//     can't list the folder or create a file in it.
//   - Before each job, the agent makes a new file there, named with 128
//     random bits, and gives it to the account, mode 0600 (newOutput).
//     Both of the job's output paths name it.
//   - After the job, the agent takes the file back: root's, mode 0600, so
//     the account can't open it again, even through a hard link
//     (reclaim). It copies a slot's listener output into the agent's
//     logs, or reads what the check's helper said, and then deletes it.
//
// launchctl print shows a job's output paths to every account, so every
// job of the service account may find a running job's output file and
// write to it. Between slots that's no weaker than before: every slot's
// job reaches every slot's runner root through the account. So the
// exposure check takes its report over a socket from its job's own
// process instead (acceptReport), and uses its output file only for what
// the helper says when it fails. No other account may open an output
// file, and once its job has ended, neither may the service account.

// exConfig is the code launchd ends a job with when, as the job's
// account, it can't open the job's output or enter its working folder.
const exConfig = 78

// output is the folder of the jobs' output files, root's, mode 0711.
func (l Layout) output() string { return filepath.Join(l.Root, "output") }

// outputRecord is the record of the output file of slot n's job, which
// the agent reads to copy it, in the jobs' folder, which only root may
// open.
func (l Layout) outputRecord(n int) string {
	return filepath.Join(l.jobs(), l.JobLabel(n)+".output")
}

// outputName is the name of an output file: 128 random bits, in hex.
var outputName = regexp.MustCompile(`^[0-9a-f]{32}$`)

// outputPerm is the output folder's mode: 0711, unless a test set
// outputMode.
func (s *Slots) outputPerm() os.FileMode {
	if s.outputMode == 0 {
		return 0o711
	}
	return s.outputMode
}

// logger is where the slots log what they leave or lose, slog's default
// unless Open set one.
func (s *Slots) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

// outputFolder makes the output folder, or checks it: the agent's, with
// mode outputPerm, not a link. One that isn't, it leaves, and fails.
func (s *Slots) outputFolder() error {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	dir := s.output()
	err := os.Mkdir(dir, 0o700)
	if err == nil {
		err = os.Chmod(dir, s.outputPerm())
	}
	if err != nil && !os.IsExist(err) {
		return err
	}
	if err := ownDir(dir, os.Geteuid(), s.outputPerm()); err != nil {
		return fmt.Errorf("the jobs' output folder isn't Silkpool's own: %w", err)
	}
	return nil
}

// LiveLog is the file runner's listener in slot n writes as it runs: its
// job's output file, as the slot's record names it, while runner is the
// slot's recorded runner and its output isn't copied yet; otherwise its
// ListenerLog, which holds the copy. Every job of the service account
// may write to a running job's output file (above), so what the agent
// reads there may be another job's as well as the listener's; the worker
// check (WorkerUp) reads the job's processes, not its output.
func (s *Slots) LiveLog(n int, runner string) string {
	if s.recordedRunner(n) == runner {
		if b, err := os.ReadFile(s.outputRecord(n)); err == nil {
			if name := strings.TrimSpace(string(b)); outputName.MatchString(name) {
				return filepath.Join(s.output(), name)
			}
		}
	}
	return s.ListenerLog(runner)
}

// newOutput makes a new output file for a job that runs as uid, with its
// group gid, and returns its path. The file is uid's, mode 0600, in the
// output folder, which uid must be able to reach (enterable).
func (s *Slots) newOutput(uid, gid int) (string, error) {
	if err := s.outputFolder(); err != nil {
		return "", err
	}
	if err := enterable(s.output(), uid, gid); err != nil {
		return "", fmt.Errorf("the job couldn't write its output: %w", err)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	path := filepath.Join(s.output(), hex.EncodeToString(b[:]))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", err
	}
	if err = errors.Join(f.Chown(uid, gid), f.Chmod(0o600), f.Close()); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// reclaim opens the output file at path once its job has ended, and
// takes it back from the account: the agent's, mode 0600. It never
// follows a link, and fails unless it's a plain file. The account keeps
// what it has open, but can't open the file again, even through a hard
// link it made, which reclaim logs.
func (s *Slots) reclaim(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, fmt.Errorf("%s isn't a plain file", path)
	}
	if err := errors.Join(unix.Fchown(fd, os.Geteuid(), os.Getegid()), unix.Fchmod(fd, 0o600)); err != nil {
		f.Close()
		return nil, &os.PathError{Op: "take back", Path: path, Err: err}
	}
	if st.Nlink != 1 {
		s.logger().Warn("A job linked its output file elsewhere. The file is root's now, so the link opens nothing",
			"file", path, "links", st.Nlink)
	}
	return f, nil
}

// collectOutput takes back the output file of slot n's last job, as its
// record names it, copies its end, at most diagCap bytes, with every
// secret scrubbed, into the listener's log of the slot's recorded runner
// (ListenerLog), root's, mode 0600, and deletes the file and its record.
// It returns how many bytes the file held. Without a record, there's
// nothing to collect; without a recorded runner, the file is deleted
// uncopied. A copy that fails keeps the file and its record, for the
// next attempt.
func (s *Slots) collectOutput(n int) (int64, error) {
	rec := s.outputRecord(n)
	b, err := os.ReadFile(rec)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	name := strings.TrimSpace(string(b))
	if !outputName.MatchString(name) {
		return 0, fmt.Errorf("%s names %q, not an output file", rec, name)
	}
	path := filepath.Join(s.output(), name)
	f, err := s.reclaim(path)
	if errors.Is(err, unix.ENOENT) {
		s.logger().Warn("A slot's output file is gone, so its listener's output is lost", "slot", n, "file", path)
		return 0, removeFiles(rec)
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if runner := s.recordedRunner(n); runner != "" {
		if err := copyOutput(f, fi.Size(), s.RunnerLogs(runner), s.ListenerLog(runner)); err != nil {
			return fi.Size(), fmt.Errorf("can't copy runner %s's listener output: %w", runner, err)
		}
	}
	return fi.Size(), removeFiles(path, rec)
}

// copyOutput copies the end of f, which is size long, into the file
// dest, in the folder dir, as copyLog copies a _diag log.
func copyOutput(f *os.File, size int64, dir, dest string) error {
	tail, err := readTail(f, size, diagCap)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(secret.Copy(out, bytes.NewReader(tail)), out.Close())
}

// removeFiles deletes each of paths, a file, if it's there.
func removeFiles(paths ...string) error {
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// clearOutput deletes the files in the output folder that no slot's
// output record names, or every file there with all. uid is the service
// account's, whose files' user flags it clears, or -1. The account-wide
// clean runs it once every slot is idle and no check is running, to
// delete what a crashed agent left there, and DeleteAccount with all.
func (s *Slots) clearOutput(uid int, all bool) error {
	fd, err := unix.Open(s.output(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: s.output(), Err: err}
	}
	dir := os.NewFile(uintptr(fd), s.output())
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	if !all {
		slots, err := s.slotNumbers()
		if err != nil {
			return err
		}
		for _, n := range slots {
			b, err := os.ReadFile(s.outputRecord(n))
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			keep[strings.TrimSpace(string(b))] = true
		}
	}
	r := remover{uid: uid}
	var errs []error
	for _, name := range names {
		if !keep[name] {
			errs = append(errs, r.removeAt(fd, name, filepath.Join(s.output(), name), false))
		}
	}
	return errors.Join(errs...)
}

// enterable fails unless uid, whose primary group is gid, may enter dir
// and each folder above it, by their owners and modes, as launchd needs
// of a job's working folder and the folder of its output. It reads them
// with the links in dir resolved. An access control list may still
// refuse uid, which the job's 78 then shows.
func enterable(dir string, uid, gid int) error {
	if uid == 0 {
		return nil
	}
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	for p := abs; ; p = filepath.Dir(p) {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			return &os.PathError{Op: "stat", Path: p, Err: err}
		}
		bit := uint16(0o001)
		switch {
		case int(st.Uid) == uid:
			bit = 0o100
		case int(st.Gid) == gid:
			bit = 0o010
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&bit == 0 {
			return fmt.Errorf("UID %d can't enter %s, so launchd would end the job with %d before it starts", uid, p, exConfig)
		}
		if p == "/" {
			return nil
		}
	}
}

// exConfigHint explains a job that ended with code having written wrote
// bytes of output, if it's most likely one launchd couldn't start: one
// that ended with 78 and wrote nothing. launchd logs nothing that says
// so.
func exConfigHint(code int, wrote int64) string {
	if code != exConfig || wrote > 0 {
		return ""
	}
	return fmt.Sprintf("it ended with %d and wrote nothing, so launchd most likely couldn't start it: "+
		"as the service account, launchd couldn't open the job's output or enter its working folder", exConfig)
}

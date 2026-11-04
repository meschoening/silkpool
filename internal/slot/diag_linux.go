package slot

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
)

// What the reset keeps of a runner's _diag logs is in "Define how the
// agent installs, verifies and updates the runner application"
// and "Define Silkpool's events, audit trail, notices and redaction".
const (
	// diagCap is how much of each log the copy keeps: its last 8 MB, the
	// runner's own page size, because a failure is at the end.
	diagCap = 8 << 20
	// diagFiles is how many logs of each kind, Runner_ and Worker_, the
	// copy keeps: the newest, by their names, which hold the time each
	// was started. A job can write as many as it likes.
	diagFiles = 4
	// diagNames is how many names in _diag the copy reads at most.
	diagNames = 4096
	// logsKept is how long the agent keeps a runner's folder of logs, and
	// logsCap how much it keeps in all, deleting the oldest first.
	logsKept = 14 * 24 * time.Hour
	logsCap  = 1 << 30
)

// diagLog is the name of a log the copy takes from _diag. It skips
// everything else, the pages and blocks folders included: GitHub keeps
// the job's log.
var diagLog = regexp.MustCompile(`^(Runner|Worker)_[A-Za-z0-9_.-]{1,100}\.log$`)

// copyLogs copies the _diag logs of slot n's last runner, as its record
// names it, into the runner's folder in the agent's logs, beside its
// listener's output (copyDiag), and returns the run its Worker log
// names. uid is the slot account's, which must own each log. Then it
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

// copyDiag copies the logs in the _diag folder of the runner root root
// into the folder dest, and returns the run the first Worker log names.
// A job can write anything there, so it follows no link, from the runner
// root down, and copies only plain files that uid owns, so that a link
// to a file of root's can't pass as a log. Each copy keeps the end of its
// log, at most limit bytes, with every secret in it scrubbed, and is
// root's, mode 0600. Without a runner root or a _diag folder, it copies
// nothing.
func copyDiag(root, dest string, uid int, limit int64) (node.Run, error) {
	diag, err := openDiag(root)
	if diag == nil || err != nil {
		return node.Run{}, err
	}
	defer diag.Close()
	names, err := diag.Readdirnames(diagNames)
	if err != nil && err != io.EOF {
		return node.Run{}, err
	}
	slices.Sort(names)
	var runners, workers []string
	for _, name := range names {
		switch m := diagLog.FindStringSubmatch(name); {
		case m == nil:
		case m[1] == "Runner":
			runners = append(runners, name)
		default:
			workers = append(workers, name)
		}
	}
	var run node.Run
	for _, name := range workers {
		f, size, err := openLog(diag, name, uid)
		if err != nil {
			return node.Run{}, err
		}
		if f != nil {
			run = parseRun(io.NewSectionReader(f, 0, min(size, diagCap)))
			f.Close()
		}
		if run != (node.Run{}) {
			break
		}
	}
	for _, name := range slices.Concat(newest(runners), newest(workers)) {
		if err := copyLog(diag, name, uid, dest, limit); err != nil {
			return node.Run{}, fmt.Errorf("can't copy %s: %w", name, err)
		}
	}
	return run, nil
}

// newest returns the last diagFiles of the sorted names.
func newest(names []string) []string { return names[max(0, len(names)-diagFiles):] }

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
func openLog(diag *os.File, name string, uid int) (*os.File, int64, error) {
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

// copyLog copies the end of the log name in the folder diag, at most
// limit bytes (readTail), if it's one to copy (openLog), into the folder
// dest, under the same name.
func copyLog(diag *os.File, name string, uid int, dest string, limit int64) error {
	f, size, err := openLog(diag, name, uid)
	if f == nil || err != nil {
		return err
	}
	defer f.Close()
	tail, err := readTail(f, size, limit)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(dest, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(secret.Copy(out, bytes.NewReader(tail)), out.Close())
}

// keyMarker is either marker of a private key block, as the scrubber
// knows them (secret.Copy).
var keyMarker = regexp.MustCompile(`-----(BEGIN|END)[A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)

// readTail reads the end of the file f, which is size long: at most
// limit bytes, from the start of a line. A cut that lands in a private key
// block leaves its END marker without its BEGIN, so the scrubber
// wouldn't know the lines before it for a key's: the tail then starts
// with a BEGIN marker of its own, and they're scrubbed.
func readTail(f *os.File, size, limit int64) ([]byte, error) {
	start := max(0, size-limit)
	tail := make([]byte, size-start)
	n, err := f.ReadAt(tail, start)
	if err != nil && err != io.EOF {
		return nil, err
	}
	tail = tail[:n]
	if start > 0 {
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		} else {
			tail = nil
		}
	}
	if m := keyMarker.FindSubmatch(tail); m != nil && string(m[1]) == "END" {
		tail = append([]byte("-----BEGIN PRIVATE KEY-----\n"), tail...)
	}
	return tail, nil
}

// jobMessage is the line the Worker log dumps the job message after, as
// JSON, starting on the next line.
var jobMessage = regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}Z INFO Worker\] Job message:\r?\n?$`)

// repository is a repository's owner/name, as the github context gives it.
var repository = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// parseRun returns the run a Worker log names: the github context's
// repository, run_id and run_number, from the job message it dumps. The
// runner writes its context data as {"t": 2, "d": [{"k": …, "v": …}]}.
// It returns the zero Run for a log that dumps no job message, or names
// no run in full. A job can write the log, so the node keeps a run only
// if it's one of the node's own target.
func parseRun(log io.Reader) node.Run {
	r := bufio.NewReader(log)
	for {
		line, err := r.ReadString('\n')
		if jobMessage.MatchString(line) {
			return decodeRun(r)
		}
		if err != nil {
			return node.Run{}
		}
	}
}

// decodeRun decodes the job message that starts r, and returns the run
// it names.
func decodeRun(r io.Reader) node.Run {
	var message struct {
		ContextData struct {
			GitHub struct {
				D []struct {
					K string          `json:"k"`
					V json.RawMessage `json:"v"`
				} `json:"d"`
			} `json:"github"`
		} `json:"contextData"`
	}
	if err := json.NewDecoder(r).Decode(&message); err != nil {
		return node.Run{}
	}
	var run node.Run
	for _, e := range message.ContextData.GitHub.D {
		switch e.K {
		case "repository":
			if json.Unmarshal(e.V, &run.Repository) != nil {
				run.Repository = ""
			}
		case "run_id":
			run.ID = number(e.V)
		case "run_number":
			run.Number = number(e.V)
		}
	}
	// A job can write the log, so the repository is scrubbed, which
	// leaves a secret no repository's name.
	run.Repository = secret.Scrub(run.Repository)
	if !repository.MatchString(run.Repository) || run.ID <= 0 || run.Number <= 0 {
		return node.Run{}
	}
	return run
}

// number returns the number v holds, as a JSON string, as the runner
// writes them, or a JSON number, or 0.
func number(v json.RawMessage) int64 {
	var s string
	if json.Unmarshal(v, &s) == nil {
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	var n int64
	if json.Unmarshal(v, &n) == nil {
		return n
	}
	return 0
}

// A logFolder is a runner's folder in the agent's logs.
type logFolder struct {
	runner   string
	modified time.Time
	size     int64
}

// pruneLogs deletes the runners' folders in the agent's logs that are
// past their time or over the cap, as of now (prunable).
func (s *Slots) pruneLogs(now time.Time) error {
	dir := filepath.Join(s.Logs, "runners")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var folders []logFolder
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		size, err := folderSize(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		folders = append(folders, logFolder{e.Name(), info.ModTime(), size})
	}
	live, err := s.recordedRunners()
	if err != nil {
		return err
	}
	for _, runner := range prunable(folders, live, now) {
		if err := os.RemoveAll(filepath.Join(dir, runner)); err != nil {
			return err
		}
	}
	return nil
}

// prunable returns the runners whose folders of logs go, as of now: each
// one older than logsKept, and then the oldest of the rest, while all of
// them together are over logsCap. The folders of live runners stay:
// their slots haven't been reset yet.
func prunable(folders []logFolder, live []string, now time.Time) []string {
	folders = slices.Clone(folders)
	slices.SortFunc(folders, func(a, b logFolder) int {
		if c := a.modified.Compare(b.modified); c != 0 {
			return c
		}
		return cmp.Compare(a.runner, b.runner)
	})
	var total int64
	for _, f := range folders {
		total += f.size
	}
	var gone []string
	for _, f := range folders {
		if slices.Contains(live, f.runner) {
			continue
		}
		if now.Sub(f.modified) > logsKept || total > logsCap {
			gone = append(gone, f.runner)
			total -= f.size
		}
	}
	return gone
}

// folderSize returns the size of the files in dir, which holds no
// folders.
func folderSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var size int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			size += info.Size()
		}
	}
	return size, nil
}

// recordedRunners returns the runners the slots' records name: those
// started and not yet reset.
func (s *Slots) recordedRunners() ([]string, error) {
	recorded, err := s.Recorded()
	if err != nil {
		return nil, err
	}
	var runners []string
	for _, n := range slices.Sorted(maps.Keys(recorded)) {
		runners = append(runners, recorded[n])
	}
	return runners, nil
}

// Recorded returns, by slot, the runner each slot's record names: the
// one Start last started there, until the slot's reset is done. A
// restarted agent reads what the dead ones' logs say, and watches the
// live ones again. With no slots' folder, there are none.
func (s *Slots) Recorded() (map[int]string, error) {
	slots, err := names(filepath.Join(s.Root, "slots"))
	if err != nil {
		return nil, err
	}
	recorded := map[int]string{}
	for _, name := range slots {
		if n, err := strconv.Atoi(name); err == nil && n > 0 {
			if runner := s.recordedRunner(n); runner != "" {
				recorded[n] = runner
			}
		}
	}
	return recorded, nil
}

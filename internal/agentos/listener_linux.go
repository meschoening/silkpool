package agentos

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
)

// The listener's own lines, after the time in UTC, as actions/runner's
// listener prints them: `2026-11-02 15:04:05Z: Listening for Jobs`. The
// Worker's lines never reach its output, which the agent starts every
// listener without ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT for, so a job can
// write these only through the job's name.
var (
	listenerLine = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}Z: (.*)$`)
	runningJob   = regexp.MustCompile(`^Running job: (.+)$`)
	completedJob = regexp.MustCompile(`^Job (.+) completed with result: ([A-Za-z]+)$`)
)

// maxLine is the longest line the follower reads: a longer one is cut
// there, and read on as another.
const maxLine = 64 << 10

// A listenerLog is what a listener's log has said so far.
type listenerLog struct {
	listening bool
	// job is the job it's running, or ran, and result GitHub's result for
	// it, once completed reports it.
	job       string
	completed bool
	result    node.Result
}

// line reads one line of the log, scrubbed, and returns the event it
// tells the node, or nil. Each event comes once: the listener listens,
// runs a job, and completes it, at most once each.
func (l *listenerLog) line(runner, text string) node.Event {
	m := listenerLine.FindStringSubmatch(secret.Scrub(text))
	if m == nil {
		return nil
	}
	said := m[1]
	switch {
	case said == "Listening for Jobs":
		if l.listening || l.job != "" {
			return nil
		}
		l.listening = true
		return node.Listening{Runner: runner}
	case l.job == "" && runningJob.MatchString(said):
		l.listening, l.job = true, runningJob.FindStringSubmatch(said)[1]
		return node.JobStarted{Runner: runner, Job: l.job}
	case l.job != "" && !l.completed:
		m := completedJob.FindStringSubmatch(said)
		if m == nil || m[1] != l.job {
			return nil
		}
		l.completed, l.result = true, node.Result(m[2])
		return node.JobCompleted{Runner: runner, Result: l.result}
	}
	return nil
}

// dead returns what the log of runner, whose listener ended while the
// agent was down, tells the node before Started: the job it ran, and,
// if it completed it, its result and a clean exit. Started finds the
// rest: a runner that isn't alive ended, interrupting a job it hadn't
// completed.
func (l listenerLog) dead(runner string) []node.Event {
	if l.job == "" {
		return nil
	}
	events := []node.Event{node.JobStarted{Runner: runner, Job: l.job}}
	if l.completed {
		events = append(events, node.JobCompleted{Runner: runner, Result: l.result}, node.ListenerExited{Runner: runner, Code: 0})
	}
	return events
}

// readLog reads the whole log at path. A log that isn't there says
// nothing.
func readLog(path string) (listenerLog, error) {
	t := tail{path: path}
	err := t.read("", nil)
	return t.log, err
}

// A tail follows a listener's log as it's written, from its start.
type tail struct {
	path string
	// at is how far the log is read, and partial the start of a line not
	// yet ended there.
	at      int64
	partial []byte
	log     listenerLog
}

// read reads the lines written since the last read, and reports the
// events they tell the node. A line still being written waits for its
// end.
func (t *tail) read(runner string, report func(node.Event)) error {
	f, err := os.OpenFile(t.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(t.at, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		t.at += int64(n)
		data := append(t.partial, buf[:n]...)
		for {
			i := bytes.IndexByte(data, '\n')
			if i < 0 && len(data) < maxLine {
				break
			}
			if i < 0 {
				i = maxLine
			}
			if e := t.log.line(runner, string(bytes.TrimSuffix(data[:i], []byte("\r")))); e != nil && report != nil {
				report(e)
			}
			data = data[min(i+1, len(data)):]
		}
		t.partial = append([]byte(nil), data...)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// scrubFile rewrites the log at path with every secret in it scrubbed,
// through a new file renamed into place, root's alone.
func scrubFile(path string) error {
	in, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".scrubbed")
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := errors.Join(secret.Copy(out, in), out.Sync(), out.Close()); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

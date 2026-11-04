// Package agentlog is the agent's own log: a file in its log folder,
// which rotates at MaxSize and keeps Keep files, holding one slog text
// line, in UTC at info level, per event the agent applies and per effect
// it carries out. There's no debug switch. The decisions behind it are in
// "Define Silkpool's events, audit trail, notices and redaction".
//
// The folder is a path the caller passes, because its place is
// packaging's: /var/log/silkpool on Linux, which the unit's
// LogsDirectory= creates (ADR 0013).
package agentlog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const (
	// Name is the log file's name in the log folder. Older files add
	// ".1" and ".2".
	Name = "agent.log"
	// MaxSize is the size at which the file rotates.
	MaxSize = 10 << 20
	// Keep is how many files the log keeps, the current one included.
	Keep = 3
)

// A File is the agent's log file, safe for concurrent writes.
type File struct {
	mu   sync.Mutex
	dir  string
	max  int64
	f    *os.File
	size int64
}

// Open opens the log in dir, creating dir (mode 0700) and the file (mode
// 0600) if they're missing, and appending to a file that's there.
func Open(dir string) (*File, error) { return open(dir, MaxSize) }

func open(dir string, max int64) (*File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agentlog: making the log folder: %w", err)
	}
	l := &File{dir: dir, max: max}
	if err := l.openFile(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) path(i int) string {
	if i == 0 {
		return filepath.Join(l.dir, Name)
	}
	return filepath.Join(l.dir, fmt.Sprintf("%s.%d", Name, i))
}

func (l *File) openFile() error {
	f, err := os.OpenFile(l.path(0), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("agentlog: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("agentlog: %w", err)
	}
	l.f, l.size = f, info.Size()
	return nil
}

// Write writes p, rotating the file first if p would take it past its
// maximum size.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, errors.New("agentlog: the log is closed")
	}
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate moves each file one older, dropping the oldest, and starts a new
// file.
func (l *File) rotate() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("agentlog: %w", err)
	}
	l.f = nil
	for i := Keep - 1; i > 0; i-- {
		err := os.Rename(l.path(i-1), l.path(i))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("agentlog: rotating: %w", err)
		}
	}
	return l.openFile()
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Tail returns the last n lines of the log, oldest first, across its
// files.
func (l *File) Tail(n int) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var lines []string
	for i := range Keep {
		if len(lines) >= n {
			break
		}
		f, err := os.Open(l.path(i))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("agentlog: %w", err)
		}
		var file []string
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for s.Scan() {
			file = append(file, s.Text())
			if len(file) > n {
				file = file[1:]
			}
		}
		err = s.Err()
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("agentlog: %w", err)
		}
		lines = append(file, lines...)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return slices.Clip(lines), nil
}

// Handler returns the handler the agent logs through: slog's text lines
// to w, at info level, with times in UTC.
func Handler(w io.Writer) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	})
}

package slot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// CleanWithin is how long the service account may go without its
// account-wide clean once a slot's reset has marked it, while other
// slots keep it busy. Past that, a slot's preparation holds its next
// runner until every slot is idle and the clean has run (ADR 0005).
//
// A day bounds how long what a job leaves in the account's shared places
// can reach a later job: a file in the home or the profile, on every OS;
// on macOS also a file in /tmp, a crontab, an at job, an agent in the
// account's user domain, or a daemon that left its slot's launchd job;
// on Windows a scheduled task, a process started outside the slot host's
// job, such as by the task or by WMI, or a file in the account's Recycle
// Bin. And a machine that's never idle drains at most once a day, which
// costs it the rest of its longest job, six hours at GitHub's default job
// timeout. Linux, macOS and Windows share the bound.
const CleanWithin = 24 * time.Hour

// dirtyFile is the file, in a folder no slot may change (cleanDue), that
// records when a slot's reset first marked the service account for its
// clean, since the last clean. It outlives the agent.
const dirtyFile = "clean-due"

// holdPoll is how often a held slot looks again for every slot idle,
// besides when a slot's reset finishes.
const holdPoll = time.Minute

// markDirty records that the service account needs its clean, from now,
// unless it already does.
func (l Layout) markDirty(now time.Time) error {
	if _, dirty := l.dirtySince(); dirty {
		return nil
	}
	return writeRecord(l.cleanDue(), now.UTC().Format(time.RFC3339))
}

// dirtySince returns since when the service account has needed its
// clean, and whether it does. A record it can't read needs it now.
func (l Layout) dirtySince() (time.Time, bool) {
	b, err := os.ReadFile(l.cleanDue())
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false
	}
	t, perr := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil || perr != nil {
		return time.Time{}, true
	}
	return t, true
}

// markClean records that the service account has had its clean.
func (l Layout) markClean() error {
	if err := os.Remove(l.cleanDue()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Clean is the service account's account-wide clean, which runs only
// once every slot is idle, since every slot shares what it cleans (ADR
// 0005). Each OS has its own steps (clean).
//
// It holds the slots' lock throughout, so no runner or exposure check
// starts meanwhile, and does nothing, returning false, while a slot holds
// a runner. It returns true once the account is clean, whether this call
// cleaned it or it needed nothing.
func (s *Slots) Clean(ctx context.Context) (bool, error) {
	return s.cleanIfIdle(ctx, true)
}

// cleanIfIdle runs the clean if every slot is idle and the account needs
// it, as Clean does. If wait is false, it gives up at once, returning
// false, while a runner or an exposure check is starting or running.
func (s *Slots) cleanIfIdle(ctx context.Context, wait bool) (bool, error) {
	if wait {
		s.use.Lock()
	} else if !s.use.TryLock() {
		return false, nil
	}
	defer s.use.Unlock()
	s.account.Lock()
	defer s.account.Unlock()
	recorded, err := s.Recorded()
	if err != nil {
		return false, fmt.Errorf("can't tell whether every slot is idle: %w", err)
	}
	if len(recorded) > 0 {
		return false, nil
	}
	if _, dirty := s.dirtySince(); !dirty {
		return true, nil
	}
	clean := s.clean
	if s.cleaning != nil {
		clean = s.cleaning
	}
	if err := clean(ctx); err != nil {
		return true, fmt.Errorf("the service account's clean couldn't %w", err)
	}
	return true, s.markClean()
}

// cleanFirst is a slot's preparation's part of the clean, for a slot
// about to get a runner. If every slot is idle and the account needs its
// clean, it runs it. If another slot holds a runner, the slot goes ahead,
// unless the account has needed its clean for CleanWithin: then it holds
// the slot, waiting for every slot to be idle, and runs the clean. A
// clean that fails fails the slot.
func (s *Slots) cleanFirst(ctx context.Context) error {
	for {
		since, dirty := s.dirtySince()
		if !dirty {
			return nil
		}
		done := s.resetSignal()
		clean, err := s.Clean(ctx)
		if clean || err != nil {
			return err
		}
		if s.now().Sub(since) < CleanWithin {
			return nil
		}
		select {
		case <-done:
		case <-time.After(holdPoll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// markForClean marks the service account for its clean, as each slot's
// reset does, from now.
func (s *Slots) markForClean(now time.Time) error {
	s.account.Lock()
	defer s.account.Unlock()
	return s.markDirty(now)
}

// resetSignal returns a channel closed when the next slot's reset
// finishes.
func (s *Slots) resetSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resets == nil {
		s.resets = make(chan struct{})
	}
	return s.resets
}

// resetDone wakes every held slot, since a slot's reset has finished.
func (s *Slots) resetDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resets != nil {
		close(s.resets)
		s.resets = nil
	}
}

// now is the time, which tests may set.
func (s *Slots) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

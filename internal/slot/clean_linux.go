package slot

import (
	"context"
	"fmt"
	"path/filepath"
)

// cleanDue is the record of when the service account's clean fell due
// (dirtyFile), in Root, which is root's, so no slot can change it.
func (l Layout) cleanDue() string { return filepath.Join(l.Root, dirtyFile) }

// clean is the Linux clean's steps, in the order "Define how the agent runs and
// launches slots on each OS" gives Linux:
//
//  1. Stop new starts. The account is expired, so cron, at, sshd and its
//     user manager all refuse it. An expiry that drifted is restored.
//  2. Remove its scheduled work: its crontab, its at jobs and, as a
//     backstop, its linger.
//  3. Kill every process of the account: its user manager is stopped
//     and killed, and then a helper running as the account kills every
//     process it may signal (KillAll), until none is left. Its scheduled
//     work is removed again then.
//  4. Recreate the home empty.
//
// Little else reaches a later job through the Linux account: each unit
// has its own /tmp and /var/tmp, its processes can't leave its cgroup,
// and the expiry and the sandbox keep it from cron, at, linger and its
// user manager.
//
// clean runs these steps (Clean), failing closed: the steps after a
// failed one don't run, and the next clean starts again from the first.
func (s *Slots) clean(ctx context.Context) error {
	a, err := s.owner()
	if err != nil {
		return fmt.Errorf("read the account: %w", err)
	}
	steps := []struct {
		what string
		do   func() error
	}{
		{"stop new starts", func() error { return expire(a) }},
		{"remove its scheduled work", func() error { return s.unschedule(ctx, a) }},
		{"kill its processes", func() error { return s.killAll(ctx, a) }},
		{"recreate its home", func() error { return s.recreateHome(a) }},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
	}
	return nil
}

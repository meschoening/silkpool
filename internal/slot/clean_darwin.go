package slot

import (
	"context"
	"fmt"
	"path/filepath"
)

// cleanDue is the record of when the service account's clean fell due
// (dirtyFile), in Silkpool's state, which only root may open.
func (l Layout) cleanDue() string { return filepath.Join(l.state(), dirtyFile) }

// clean is the macOS clean's steps, in the order "Define how the agent
// runs and launches slots on each OS" gives the Mac, with the steps a
// slot's own reset took out (Reset):
//
//  1. Stop new starts: boot out the account's launchd user domain, which
//     ends the agents launchd runs for it (cfprefsd, distnoted and the
//     rest). This comes before the kill, since distnoted comes straight
//     back otherwise, as "Verify keychain signing from a launchd-launched
//     slot on the Mac" found.
//  2. Remove its scheduled work: its crontab and its at jobs. Where the
//     agent runs with Full Disk Access, a spool macOS refuses fails the
//     step (unschedule).
//  3. Kill every process of the account: until none is left, boot out
//     the user domain again and run a helper as the account that kills
//     every process it may signal, kill(-1, SIGKILL) (killAll). Then
//     remove its user domain and scheduled work again.
//  4. Reset the account's Launch Services database, where an app a job
//     registers goes: run lsregister -delete as the account, then kill
//     what that started, as in step 3 (resetLaunchServices, #297).
//  5. Sweep the shared places, such as /tmp, of what the account owns
//     there, as the latest survey found them (sweep).
//  6. Recreate the home empty.
//  7. Empty the account's /var/folders folders. SIP keeps the folders
//     there flagged sunlnk, so they're emptied, not deleted. SIP refuses
//     even root the data vault that holds the Launch Services database,
//     0/com.apple.LaunchServices.dv, which step 4 emptied: that vault is
//     left. Any other entry macOS refuses fails the step.
//  8. Delete the output files no slot's record names, which only an
//     agent that crashed during a job leaves (clearOutput).
//
// It acts on the account only as its record proves it Silkpool's, read
// again at each clean (whose): the user record of its name must hold the
// GeneratedUID its ledger owns, and its UID and GID, which every step
// takes from it, must be in the role accounts' range. Without that, it
// acts on nothing, and fails. clean runs these steps (Clean), failing
// closed: the steps after a failed one don't run, and the next clean
// starts again from the first.
func (s *Slots) clean(ctx context.Context) error {
	a, err := s.whose(ctx)
	if err == nil && !a.proven {
		err = fmt.Errorf("the service account %s isn't the one Silkpool made", s.Name)
	}
	if err != nil {
		return fmt.Errorf("read the account: %w", err)
	}
	steps := []struct {
		what string
		do   func() error
	}{
		{"stop new starts", func() error { return s.bootoutDomain(ctx, a.uid) }},
		{"remove its scheduled work", func() error { return s.unschedule(ctx, a.uid, s.FullDiskAccess) }},
		{"kill its processes", func() error { return s.killAll(ctx, a) }},
		{"reset its Launch Services database", func() error { return s.resetLaunchServices(ctx, a) }},
		{"sweep the shared places", func() error {
			return s.sweep(ctx, ServiceAccount{Name: s.Name, UID: a.uid, GID: a.gid})
		}},
		{"recreate its home", func() error { return s.recreateHome(a) }},
		{"empty its /var/folders folders", func() error { return s.emptyVarFolders(a.uid, nil, true) }},
		{"delete the jobs' leftover output", func() error { return s.clearOutput(a.uid, false) }},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
	}
	return nil
}

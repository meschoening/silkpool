package slot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winslot"
)

// Reset resets slot n once its runner has ended, and returns what the
// node applies, as ResetSlot asks: LogsCopied, then SlotReset, or
// ResetFailed if a step failed. A failed step fails closed: the steps
// after it don't run, and the next attempt starts again from the first.
//
// Every slot runs as the machine's one service account (#214), so the
// reset splits in two (ADR 0005). This is the slot's own part, which
// never touches another slot's job or folders, or anything of the
// account's that another slot's job may be using:
//
//  1. End the slot host's job object: if the slot's service is still
//     running, tell its host to end the listener's job at once
//     (winslot.ControlKill), and wait until the service has stopped. The
//     host stops only once nothing is left in the job, which allows no
//     breakaway, and a host that dies ends its job with it, since the
//     job kills what's in it once its last handle closes. Then wait
//     until the job lists no process, since a host that couldn't empty
//     it stops anyway (endHost, #270).
//  2. Copy the last runner's _diag logs into its folder in the agent's
//     logs, beside its listener's output, and read the run its Worker
//     log names, as on Linux: plain files of the account's only, no
//     links, each capped and scrubbed (copyLogs).
//  3. Delete the runner root whole, with the runner's credential files,
//     _diag and _work, following no link in it.
//  4. Delete the slot's temporary folder, its runners' TEMP and TMP.
//  5. Mark the account for its account-wide clean, and forget the slot's
//     runner, so that it counts as idle.
//
// What every slot shares waits for the account-wide clean, which runs
// only once every slot is idle (Clean): the account itself, its
// scheduled tasks, its processes outside every slot's job, such as one a
// scheduled task or WMI started, its profile, its Recycle Bin, and what it
// left in the shared places. The
// reset runs the clean itself if it leaves every slot idle.
func (s *Slots) Reset(ctx context.Context, n int) []node.Event {
	run, err := s.reset(ctx, n, s.now())
	events := []node.Event{node.LogsCopied{Slot: n, Run: run}}
	if err != nil {
		return append(events, node.ResetFailed{Slot: n, Detail: detail(err)})
	}
	// A clean that fails here fails the next slot's preparation instead,
	// which runs it again: this slot's own part is done.
	s.cleanIfIdle(ctx, false)
	return append(events, node.SlotReset{Slot: n})
}

func (s *Slots) reset(ctx context.Context, n int, now time.Time) (node.Run, error) {
	var run node.Run
	sid, err := winaccount.ReadRecord(s.accountRecord())
	if err != nil {
		return run, fmt.Errorf("slot %d's reset couldn't read the service account's record: %w", n, err)
	}
	steps := []struct {
		what string
		do   func() error
	}{
		{"end its slot host's job", func() error { return s.endHost(ctx, n) }},
		{"copy its runner's logs", func() (err error) { run, err = s.copyLogs(n, sid, now); return err }},
		{"delete its runner root", func() error { return os.RemoveAll(s.RunnerRoot(n)) }},
		{"delete its temporary folder", func() error { return os.RemoveAll(s.TempDir(n)) }},
		{"mark the service account for its clean", func() error { return s.markForClean(now) }},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			return run, fmt.Errorf("slot %d's reset couldn't %s: %w", n, step.what, err)
		}
	}
	if err := os.Remove(filepath.Join(s.Folder(n), runnerFile)); err != nil && !os.IsNotExist(err) {
		return run, fmt.Errorf("slot %d's reset couldn't forget its runner: %w", n, err)
	}
	s.resetDone()
	return run, nil
}

// endHost ends slot n's host and its listener's job object, if its
// service still runs, and waits, up to hostWait, until the service has
// stopped and nothing is left in the slot's job (jobEnded). It tells the
// host again at each look, since a host that's still starting refuses
// controls. A slot with no service has no host.
func (s *Slots) endHost(ctx context.Context, n int) error {
	name := s.ServiceName(n)
	deadline := time.Now().Add(s.hostWait)
	for {
		st, err := s.services.State(name)
		if err != nil {
			return err
		}
		if st != winslot.Active {
			return s.jobEnded(ctx, name, deadline)
		}
		told := s.services.Kill(name)
		if time.Now().After(deadline) {
			if told != nil {
				return fmt.Errorf("its slot host %s still runs %v after it was first told to end its job, and the last telling failed: %w", name, s.hostWait, told)
			}
			return fmt.Errorf("its slot host %s still runs %v after it was first told to end its job", name, s.hostWait)
		}
		select {
		case <-time.After(s.poll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// jobEnded waits, until deadline, for the job object of the slot whose
// service is name to hold no process, once its host has stopped. A
// process of the job that's still ending, or that the host couldn't end,
// keeps the job object, and its program, which the reset and DeleteSlot
// would then fail to delete with "Access is denied" (#270). If something
// still runs in the job at the deadline, it fails, naming it. A job
// that's gone holds nothing.
func (s *Slots) jobEnded(ctx context.Context, name string, deadline time.Time) error {
	read := winslot.Programs
	if s.programs != nil {
		read = s.programs
	}
	job := winslot.SlotJob(name)
	for {
		left, err := read(job)
		if err != nil {
			return fmt.Errorf("can't tell whether anything still runs in its job %s: %w", job, err)
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("its slot host %s has stopped, but %s still run in its job %v after it was first told to end it",
				name, strings.Join(left, ", "), s.hostWait)
		}
		select {
		case <-time.After(s.poll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// clean is the Windows clean's steps, in the order "Define how the agent
// runs and launches slots on each OS" gives Windows, as #214 split it
// from each slot's reset, since each would hit every slot:
//
//  1. Stop new logons: disable the account, which refuses S4U, the slot
//     hosts' logon, and the account's scheduled tasks
//     (winaccount.Disable).
//  2. End its scheduled tasks' running instances, through Task
//     Scheduler, which then lets go of the account's profile it loaded
//     for them. A task's process killed while Task Scheduler still ran
//     its instance left the profile loaded for good: the
//     privileged-windows lane at 188c0d3 found it still loaded minutes
//     later, until the VM was rebooted.
//  3. Delete its scheduled tasks.
//  4. Kill every process whose token's user is the account, repeating
//     until none is left. Then delete its scheduled tasks again, since a
//     process could have made one before it was killed.
//  5. Unload its profile: each slot's host unloaded what it loaded as
//     its listener ended, and nothing of the account runs now, so it
//     waits, up to profileWait, for Windows to let the profile go.
//  6. Delete its profile, which the next host's LoadUserProfile makes
//     again.
//  7. Empty its Recycle Bin: its own folder, $Recycle.Bin\<SID>, on each
//     fixed drive.
//  8. Sweep the shared places, as the latest survey found them, of what
//     the account owns (sweep). Without a survey that succeeded, of this
//     account, the clean fails closed here.
//  9. Enable the account again (winaccount.Enable).
//
// It acts on the account as the agent's record has its SID, read again
// at each clean, and stops at the first step if the account of its name
// isn't that one. clean runs these steps (Clean), failing closed: the
// steps after a failed one don't run, which leaves the account disabled,
// so no slot can start, and the next clean starts again from the first.
func (s *Slots) clean(ctx context.Context) error {
	sid, err := winaccount.ReadRecord(s.accountRecord())
	if err == nil && sid == "" {
		err = errors.New("no SID is recorded for it")
	}
	if err != nil {
		return fmt.Errorf("read the account: %w", err)
	}
	steps := []struct {
		what string
		do   func() error
	}{
		{"stop new logons", func() error { return winaccount.Disable(s.machine, s.Account, sid) }},
		{"end its scheduled tasks", func() error { return s.endTasks(sid) }},
		{"delete its scheduled tasks", func() error { return s.deleteTasks(sid) }},
		{"kill its processes", func() error { return s.killAll(ctx, sid) }},
		{"unload its profile", func() error { return s.waitUnloaded(ctx, sid) }},
		{"delete its profile", func() error { return s.machine.DeleteProfile(sid) }},
		{"empty its Recycle Bin", func() error { return s.emptyRecycleBins(sid) }},
		{"sweep the shared places", func() error { return s.sweep(ctx, sid) }},
		{"enable it again", func() error { return winaccount.Enable(s.machine, s.Account, sid) }},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
	}
	return nil
}

// endTasks ends the running instance of each scheduled task that runs
// as sid.
func (s *Slots) endTasks(sid string) error {
	tasks, err := s.machine.Tasks(sid)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if err := s.machine.EndTask(t); err != nil {
			return fmt.Errorf("can't end its task %s: %w", t, err)
		}
	}
	return nil
}

// deleteTasks deletes the scheduled tasks that run as sid, and checks
// none is left.
func (s *Slots) deleteTasks(sid string) error {
	tasks, err := s.machine.Tasks(sid)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if err := s.machine.DeleteTask(t); err != nil {
			return fmt.Errorf("can't delete its task %s: %w", t, err)
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	left, err := s.machine.Tasks(sid)
	if err == nil && len(left) > 0 {
		err = fmt.Errorf("its tasks %q are still there", left)
	}
	return err
}

// killRounds is how many rounds of kills the clean tries before it gives
// up on the processes still left.
const killRounds = 20

// killAll kills every process running as sid, until none is left, and
// then deletes its scheduled tasks again.
func (s *Slots) killAll(ctx context.Context, sid string) error {
	for round := 1; ; round++ {
		pids, err := s.machine.Pids(sid)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			break
		}
		var failed []error
		for _, pid := range pids {
			if err := s.machine.Terminate(pid); err != nil {
				failed = append(failed, fmt.Errorf("process %d: %w", pid, err))
			}
		}
		if round == killRounds {
			return fmt.Errorf("its processes %v still run after %d rounds of kills: %w", pids, round, errors.Join(failed...))
		}
		select {
		case <-time.After(time.Duration(round) * 50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.deleteTasks(sid)
}

// waitUnloaded waits, up to profileWait, until Windows no longer has
// sid's profile loaded.
func (s *Slots) waitUnloaded(ctx context.Context, sid string) error {
	deadline := time.Now().Add(s.profileWait)
	for {
		loaded, err := s.machine.ProfileLoaded(sid)
		if err != nil || !loaded {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("its profile is still loaded %v after nothing of it ran", s.profileWait)
		}
		select {
		case <-time.After(s.poll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// emptyRecycleBins deletes sid's own folder in each fixed drive's
// Recycle Bin, following no link in it. Windows makes it again when the
// account next deletes a file there.
func (s *Slots) emptyRecycleBins(sid string) error {
	bins, err := s.machine.RecycleBins(sid)
	if err != nil {
		return err
	}
	for _, b := range bins {
		if err := os.RemoveAll(b); err != nil {
			return err
		}
	}
	return nil
}

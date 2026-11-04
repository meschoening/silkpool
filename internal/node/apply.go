package node

import (
	"slices"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
)

// Apply applies e to n at now. It returns the node that results and the
// effects the agent must carry out, in order, ending with a WakeAt if
// the node has something to do later.
//
// Every Apply first lets availability catch up on a resume time that has
// passed. It then records what e reports, and settles each slot against
// what the node wants of it, highest-numbered first: a slot wants a
// runner while the node is On duty and the slot is within the slot
// limit. A runner that isn't wanted is retired DELETE-first, and outside
// Emergency stop and a lost target no listener is signalled before its
// registration is deleted. A command is answered once the node has
// settled, and new outbox entries are delivered while connected.
func (n Node) Apply(e Event, now time.Time) (Node, []Effect) {
	a := &applying{n: &n, now: now, sent: n.Seq}
	if !n.Enrolled && !a.enroll(e) {
		return n, a.effects
	}
	n.Slots = slices.Clone(n.Slots)
	n.Outbox = slices.Clone(n.Outbox)
	n.Notices = slices.Clone(n.Notices)
	a.forget()
	a.prior = n.Availability
	command := a.commanded(e)
	var effects []availability.Effect
	n.Availability, effects = n.Availability.Apply(command, now)
	for _, eff := range effects {
		switch eff := eff.(type) {
		case availability.Refused:
			a.replying.Refused = eff.Reason
		case availability.AutoResumed:
			a.emit(AutoResumed{At: eff.At})
			a.happened(AppliedCommand{Origin: Origin{Surface: AutoResume}, Event: availability.Resume{}})
			n.IntentSet = n.Outbox[len(n.Outbox)-1]
			a.prior.Intent, a.prior.ResumeAt = availability.OnDuty, time.Time{}
		case availability.CancelJobs:
			cancelled := a.emergencyStop()
			if a.replying != nil {
				a.replying.cancelled = cancelled
			}
		}
	}
	installs := n.installs()
	a.record(e)
	a.settle()
	a.budget(e)
	a.install(installs, e)
	a.reply()
	a.episode()
	a.settleNotices()
	a.deliver()
	a.wipe()
	if at := n.deadline(); !at.IsZero() {
		a.emit(WakeAt{At: at})
	}
	return n, a.effects
}

// applying is one Apply under way.
type applying struct {
	n        *Node
	now      time.Time
	effects  []Effect
	replying *replying
	// sent is the newest outbox entry already sent on this connection.
	sent int64
	// prior is the availability the command under way applies to.
	prior availability.Node
}

func (a *applying) emit(e Effect) { a.effects = append(a.effects, e) }

// slot returns slot number if it's in phase, or nil.
func (a *applying) slot(number int, phase Phase) *Slot {
	for i := range a.n.Slots {
		if s := &a.n.Slots[i]; s.Number == number && s.Phase == phase {
			return s
		}
	}
	return nil
}

// hosting returns the slot hosting runner, or nil.
func (a *applying) hosting(runner string) *Slot {
	for i := range a.n.Slots {
		if s := &a.n.Slots[i]; s.Phase == Hosting && s.Runner.Name == runner {
			return s
		}
	}
	return nil
}

// record records what e reports.
func (a *applying) record(e Event) {
	switch e := e.(type) {
	case HubSettings:
		a.n.Target, a.n.Scope = e.Target, e.Scope
		a.wanted(e.RunnerApp)
	case InstallReady:
		a.installReady(e.Version)
	case InstallFailed:
		a.installFailed(e.Version, e.Reason)
	case Started:
		a.n.Connected, a.n.Machine = false, e.Machine
		a.foundInstalls(e.Installs)
		a.started(e.Alive)
	case Connected:
		a.n.Connected, a.sent = true, 0
		a.connected()
	case Disconnected:
		a.n.Connected = false
	case Removed:
		a.n.Connected, a.n.Removal.Heard = false, true
	case Acknowledged:
		a.acknowledged(e.Through)
	case StopLostRunners:
		a.stopLost(e.Runners)
	case Prepared:
		// A preparation outlives a pause, so its result decides the slot
		// and no second one starts beside it.
		s := a.slot(e.Slot, Preparing)
		switch {
		case s == nil:
		case !a.n.wants(*s):
			s.Phase, s.Failure = Free, Failure{}
		case a.n.Installed == "" && a.n.Discarded:
			// The agent prepared it from the install the node discarded, as
			// it waited for that one, so no runner could start in it. While
			// the node's own install is under way, settling prepares it
			// again, to wait for that one. With none coming, it fails as a
			// slot with no install does.
			s.Phase = Free
			if !a.n.Update.Out {
				a.fail(s, RunnerAppUnavailable, "No runner application is installed yet")
			}
		default:
			s.Phase = Requesting
			a.emit(RequestRunner{Slot: s.Number})
		}
	case PrepareFailed:
		if s := a.slot(e.Slot, Preparing); s != nil {
			s.Phase = Free
			a.fail(s, e.Reason, e.Detail)
		}
	case Granted:
		a.granted(e)
	case GrantFailed:
		if s := a.slot(e.Slot, Requesting); s != nil {
			s.Phase = Free
			a.fail(s, e.Reason, e.Detail)
		}
	case StartFailed:
		if s := a.hosting(e.Runner); s != nil && !s.Runner.Listening {
			a.fail(s, e.Reason, e.Detail)
			a.emit(RetireRunner{Slot: s.Number, Runner: e.Runner})
			a.reset(s)
		}
	case Listening:
		if s := a.hosting(e.Runner); s != nil {
			s.Runner.Listening = true
		}
	case JobStarted:
		if s := a.hosting(e.Runner); s != nil && s.Runner.Job == "" {
			s.Runner.Listening, s.Runner.Job, s.Runner.JobSince = true, e.Job, a.now
		}
	case WorkerStarted:
		if s := a.hosting(e.Runner); s != nil {
			s.Runner.WorkerUp = true
		}
	case JobCompleted:
		if s := a.hosting(e.Runner); s != nil && s.Runner.Job != "" && s.Runner.Retirement != Cancelling && e.Result.known() {
			s.Runner.Result = e.Result
		}
	case ListenerExited:
		if s := a.hosting(e.Runner); s != nil {
			a.exited(s, e.Code)
			if e.OutOfMemory {
				a.outOfMemory(s, e.Runner)
			}
		}
	case Retired:
		if s := a.hosting(e.Runner); s != nil {
			a.retired(s, e)
		}
	case LogsCopied:
		if s := a.slot(e.Slot, Resetting); s != nil {
			a.jobRecorded(s, e.Run)
		}
	case SlotReset:
		if s := a.slot(e.Slot, Resetting); s != nil {
			s.Phase = Free
			a.jobRecorded(s, Run{})
		}
	case ResetFailed:
		if s := a.slot(e.Slot, Resetting); s != nil && a.n.Removal.Force {
			s.Phase = Free
			a.jobRecorded(s, Run{})
			a.emit(NotCleanedUp(e))
		} else if s != nil {
			s.Phase = RetryingReset
			a.fail(s, SlotCantStart, e.Detail)
		}
	case AccountDeleted:
		if s := a.slot(e.Slot, DeletingAccount); s != nil {
			s.Phase, s.Failure = Deleted, Failure{}
		}
	case DeleteFailed:
		if s := a.slot(e.Slot, DeletingAccount); s != nil && a.n.Removal.Force {
			s.Phase = Deleted
			a.emit(NotCleanedUp(e))
		} else if s != nil {
			s.Phase = RetryingDelete
			a.fail(s, SlotCantStart, e.Detail)
		}
	}
}

// fail records a failed attempt in s.
func (a *applying) fail(s *Slot, reason Reason, detail string) {
	f := &s.Failure
	if f.Attempts == 0 {
		f.Since = a.now
	}
	f.Attempts++
	f.Reason, f.Detail = reason, detail
	f.RetryAt = a.now.Add(Backoff(f.Attempts))
}

// reset starts resetting s, whose runner has ended.
func (a *applying) reset(s *Slot) {
	s.Phase, s.Runner = Resetting, Runner{}
	a.emit(ResetSlot{Slot: s.Number})
}

func (a *applying) granted(e Granted) {
	s := a.slot(e.Slot, Requesting)
	if s == nil || !a.n.wants(*s) {
		// Never started, the registration can't take a job, so the slot
		// needn't wait for its deletion.
		if s != nil {
			s.Phase = Free
		}
		a.emit(RetireRunner(e))
		return
	}
	s.Phase, s.Runner = Hosting, Runner{Name: e.Runner, Started: a.now, Version: a.n.Installed}
	a.emit(StartListener{Slot: s.Number, Runner: e.Runner, Version: a.n.Installed})
}

func (a *applying) exited(s *Slot, code int) {
	r := s.Runner
	switch {
	case r.Job != "":
		a.jobEnded(s, code == 0)
	case code == ExitVersionRefused:
		a.refused(s)
	case r.Retirement == NotRetiring && !r.Late:
		// Nobody asked it to go, and it took no job.
		a.fail(s, RunnerOffline, "")
	}
	a.reset(s)
}

func (a *applying) retired(s *Slot, e Retired) {
	r := &s.Runner
	if r.Retirement.signalled() {
		return
	}
	switch {
	case e.Status == 204 || e.Status == 404 && e.TargetVisible:
		r.Retirement, r.RetireAt, r.KillAt = Stopping, time.Time{}, a.now.Add(KillAfter)
		a.emit(Interrupt{Slot: s.Number, Runner: r.Name})
	case e.Status == 422:
		r.Retirement, r.RetireAt = ClosingAfterJob, time.Time{}
	case r.Retirement == Retiring && r.RetireAt.IsZero():
		r.RetireAttempts++
		r.RetireAt = a.now.Add(Backoff(r.RetireAttempts))
	}
}

// emergencyStop interrupts every listener not already stopping, and asks
// the Hub to delete every registration. It returns the jobs it cancels,
// in slot order.
func (a *applying) emergencyStop() []CancelledJob {
	var cancelled []CancelledJob
	for i := len(a.n.Slots) - 1; i >= 0; i-- {
		s := &a.n.Slots[i]
		r := &s.Runner
		if s.Phase != Hosting || r.Retirement.signalled() {
			continue
		}
		if r.Job != "" && r.Result == "" {
			cancelled = append(cancelled, CancelledJob{Slot: s.Number, Runner: r.Name, Job: r.Job})
		}
		a.stop(s, Cancelling)
		a.emit(RetireRunner{Slot: s.Number, Runner: r.Name})
	}
	slices.Reverse(cancelled)
	return cancelled
}

// stop stops s's runner gracefully, as retirement: it interrupts the
// listener at once, or, while the runner holds a job whose worker isn't
// up, holds the Interrupt until the worker is up or WorkerWait has
// passed. The kill counts from the Interrupt.
func (a *applying) stop(s *Slot, retirement Retirement) {
	r := &s.Runner
	r.Retirement, r.RetireAt = retirement, time.Time{}
	if r.waitsForWorker() {
		r.InterruptAt, r.KillAt = a.now.Add(WorkerWait), time.Time{}
		return
	}
	a.interrupt(s)
}

// interrupt interrupts s's listener, to be killed after KillAfter.
func (a *applying) interrupt(s *Slot) {
	r := &s.Runner
	r.InterruptAt, r.KillAt = time.Time{}, a.now.Add(KillAfter)
	a.emit(Interrupt{Slot: s.Number, Runner: r.Name})
}

// stopLost interrupts the listeners of runners, whose target is lost,
// busy or not, without asking the Hub to delete their registrations.
func (a *applying) stopLost(runners []string) {
	for i := len(a.n.Slots) - 1; i >= 0; i-- {
		s := &a.n.Slots[i]
		r := &s.Runner
		if s.Phase != Hosting || r.Retirement.signalled() || !slices.Contains(runners, r.Name) {
			continue
		}
		a.stop(s, StoppingLost)
	}
}

// started recovers the slot table after the agent starts: a live runner
// is re-adopted, a dead one frees its slot, and what was under way is
// asked for again.
func (a *applying) started(alive []string) {
	for i := len(a.n.Slots) - 1; i >= 0; i-- {
		s := &a.n.Slots[i]
		r := s.Runner
		switch {
		case s.Phase == Preparing, s.Phase == Requesting:
			// The Hub deletes a registration granted to the agent that
			// went down.
			s.Phase = Free
		case s.Phase == Resetting:
			a.emit(ResetSlot{Slot: s.Number})
		case s.Phase == DeletingAccount:
			a.emit(DeleteAccount{Slot: s.Number})
		case s.Phase != Hosting:
		case !slices.Contains(alive, r.Name):
			if r.Job != "" {
				a.jobEnded(s, false)
			}
			a.reset(s)
		case r.Retirement == Retiring && r.RetireAt.IsZero():
			a.emit(RetireRunner{Slot: s.Number, Runner: r.Name})
		case !r.InterruptAt.IsZero():
			// The stop still holds: the agent reports the worker again if
			// it's up, and the deadline stands.
		case r.Retirement.signalled() && r.KillAt.IsZero():
			a.emit(Kill{Slot: s.Number, Runner: r.Name})
		case r.Retirement.signalled():
			a.emit(Interrupt{Slot: s.Number, Runner: r.Name})
		}
	}
}

// connected makes whatever waits on the Hub due now.
func (a *applying) connected() {
	for i := range a.n.Slots {
		s := &a.n.Slots[i]
		if s.Phase == Free && s.Failure.Reason.offHub() {
			s.Failure.RetryAt = a.now
		}
		if s.Phase == Hosting && s.Runner.Retirement == Retiring && !s.Runner.RetireAt.IsZero() {
			s.Runner.RetireAt = a.now
		}
	}
}

// settle grows the slot table to the slot limit, settles each slot, and
// drops free slots above the limit.
func (a *applying) settle() {
	n := a.n
	limit := n.Availability.SlotLimit
	for len(n.Slots) < limit {
		n.Slots = append(n.Slots, Slot{Number: len(n.Slots) + 1})
	}
	for i := len(n.Slots) - 1; i >= 0; i-- {
		a.settleSlot(&n.Slots[i])
	}
	for len(n.Slots) > limit && n.Slots[len(n.Slots)-1].Phase == Free {
		n.Slots = n.Slots[:len(n.Slots)-1]
	}
}

// settleSlot starts whatever s needs next, and works out its grace.
func (a *applying) settleSlot(s *Slot) {
	wanted := a.n.wants(*s)
	switch s.Phase {
	case Free:
		switch {
		case a.n.Removal.Started:
			s.Phase, s.Failure = DeletingAccount, Failure{}
			a.emit(DeleteAccount{Slot: s.Number})
		case !wanted && s.Failure.Reason != SlotExposed:
			s.Failure = Failure{}
		case !wanted:
		case !a.now.Before(s.Failure.RetryAt):
			s.Phase = Preparing
			a.emit(PrepareSlot{Slot: s.Number})
		}
	case RetryingReset:
		if !a.now.Before(s.Failure.RetryAt) {
			s.Phase = Resetting
			a.emit(ResetSlot{Slot: s.Number})
		}
	case RetryingDelete:
		if !a.now.Before(s.Failure.RetryAt) {
			s.Phase = DeletingAccount
			a.emit(DeleteAccount{Slot: s.Number})
		}
	case Hosting:
		a.settleRunner(s, wanted)
	}

	switch {
	case !wanted:
		s.RunnerlessSince = time.Time{}
	case s.serving():
		s.RunnerlessSince, s.Failure = time.Time{}, Failure{}
	case s.RunnerlessSince.IsZero():
		s.RunnerlessSince = a.now
	}
	s.Degraded = wanted && s.Failure.Attempts > 0 && !s.RunnerlessSince.IsZero() &&
		!a.now.Before(s.RunnerlessSince.Add(Grace))
}

// settleRunner retires s's runner if it isn't wanted, if it's late and
// due for another attempt, or if it's idle on an install new runners no
// longer start from; takes it back if it's wanted again; and acts on its
// deadlines.
func (a *applying) settleRunner(s *Slot, wanted bool) {
	r := &s.Runner
	outdated := a.n.outdated(*r)
	if r.Retirement == Retiring && !r.RetireAt.IsZero() && wanted && !outdated && (r.Listening || !r.Late) {
		// The Hub didn't answer, and the runner is wanted again: the
		// drain is cancelled.
		r.Retirement, r.RetireAttempts, r.RetireAt = NotRetiring, 0, time.Time{}
	}
	switch r.Retirement {
	case NotRetiring:
		switch {
		case !wanted, r.Late && !r.Listening && !a.now.Before(s.Failure.RetryAt), outdated && r.Job == "":
			r.Retirement, r.RetireAt = Retiring, time.Time{}
			a.emit(RetireRunner{Slot: s.Number, Runner: r.Name})
		case !r.Listening && !r.Late && !a.now.Before(r.Started.Add(OnlineTimeout)):
			r.Late = true
			a.fail(s, RunnerOffline, "")
		}
	case Retiring:
		// While a request is out, only its result decides.
		if !r.RetireAt.IsZero() && !a.now.Before(r.RetireAt) {
			r.RetireAt = time.Time{}
			a.emit(RetireRunner{Slot: s.Number, Runner: r.Name})
		}
	case Stopping, Cancelling, StoppingLost:
		if !r.InterruptAt.IsZero() && (!r.waitsForWorker() || !a.now.Before(r.InterruptAt)) {
			a.interrupt(s)
		} else if !r.KillAt.IsZero() && !a.now.Before(r.KillAt) {
			r.KillAt = time.Time{}
			a.emit(Kill{Slot: s.Number, Runner: r.Name})
		}
	}
}

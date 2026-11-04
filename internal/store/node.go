package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
)

// Node is a node.Node's stored form, which the agent keeps as its node
// and the Hub as each node's mirror.
type Node struct {
	Enrolled      bool         `json:"enrolled,omitempty"`
	Availability  Availability `json:"availability"`
	Machine       Machine      `json:"machine,omitzero"`
	InForce       Budget       `json:"in_force,omitzero"`
	Slots         []Slot       `json:"slots,omitempty"`
	Target        string       `json:"target,omitempty"`
	Scope         Scope        `json:"scope,omitzero"`
	RunnerApp     RunnerApp    `json:"runner_app,omitzero"`
	Installed     string       `json:"installed,omitempty"`
	Discarded     bool         `json:"discarded,omitempty"`
	Update        Update       `json:"update,omitzero"`
	DegradedSince string       `json:"degraded_since,omitempty"`
	Connected     bool         `json:"connected,omitempty"`
	Removal       Removal      `json:"removal,omitzero"`
	Notices       []Notice     `json:"notices,omitempty"`
	Replies       []Reply      `json:"replies,omitempty"`
	Outbox        []Entry      `json:"outbox,omitempty"`
	Seq           int64        `json:"seq,omitempty"`
	IntentSet     *Entry       `json:"intent_set,omitempty"`
}

// Availability is an availability.Node's stored form. Its zone is an
// IANA name.
type Availability struct {
	Intent     string     `json:"intent"`
	AutoResume AutoResume `json:"auto_resume"`
	SlotLimit  int        `json:"slot_limit"`
	Budget     Budget     `json:"budget,omitzero"`
	Revision   int64      `json:"revision"`
	ResumeAt   string     `json:"resume_at,omitempty"`
}

// Budget is an availability.Budget's stored form: memory in bytes, and
// CPUs, each left out while it's none.
type Budget struct {
	Memory int64 `json:"memory,omitempty"`
	CPUs   int   `json:"cpus,omitempty"`
}

// Machine is a node.Machine's stored form: RAM in bytes, and the logical
// CPU count.
type Machine struct {
	RAM   int64 `json:"ram,omitempty"`
	CPUs  int   `json:"cpus,omitempty"`
	MacOS bool  `json:"macos,omitempty"`
}

// AutoResume is an availability.AutoResume's stored form: its time of
// day as HH:MM, and its days by name, Sunday first.
type AutoResume struct {
	On   bool     `json:"on"`
	Time string   `json:"time"`
	Days []string `json:"days"`
	Zone string   `json:"zone,omitempty"`
}

// Slot is a node.Slot's stored form.
type Slot struct {
	Number          int     `json:"number"`
	Phase           string  `json:"phase"`
	Runner          Runner  `json:"runner,omitzero"`
	Failure         Failure `json:"failure,omitzero"`
	RunnerlessSince string  `json:"runnerless_since,omitempty"`
	Degraded        bool    `json:"degraded,omitempty"`
	Ended           Job     `json:"ended,omitzero"`
}

// Runner is a node.Runner's stored form.
type Runner struct {
	Name           string `json:"name"`
	Started        string `json:"started,omitempty"`
	Listening      bool   `json:"listening,omitempty"`
	Late           bool   `json:"late,omitempty"`
	Job            string `json:"job,omitempty"`
	JobSince       string `json:"job_since,omitempty"`
	Result         string `json:"result,omitempty"`
	WorkerUp       bool   `json:"worker_up,omitempty"`
	Version        string `json:"version,omitempty"`
	Retirement     string `json:"retirement,omitempty"`
	RetireAttempts int    `json:"retire_attempts,omitempty"`
	RetireAt       string `json:"retire_at,omitempty"`
	KillAt         string `json:"kill_at,omitempty"`
	InterruptAt    string `json:"interrupt_at,omitempty"`
}

// Failure is a node.Failure's stored form.
type Failure struct {
	Reason   string `json:"reason,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Since    string `json:"since,omitempty"`
	RetryAt  string `json:"retry_at,omitempty"`
}

// RunnerApp is a node.RunnerApp's stored form.
type RunnerApp struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// Update is a node.Update's stored form.
type Update struct {
	Version  string `json:"version"`
	Out      bool   `json:"out,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	RetryAt  string `json:"retry_at,omitempty"`
	Failed   string `json:"failed,omitempty"`
}

// Removal is a node.Removal's stored form.
type Removal struct {
	Started bool `json:"started,omitempty"`
	Force   bool `json:"force,omitempty"`
	Heard   bool `json:"heard,omitempty"`
}

// Reply is a node.Reply's stored form.
type Reply struct {
	ID       string       `json:"id,omitempty"`
	At       string       `json:"at"`
	Refused  *Error       `json:"refused,omitempty"`
	Settings Availability `json:"settings"`
}

// Origin is a node.Origin's stored form.
type Origin struct {
	Surface string `json:"surface,omitempty"`
	Account string `json:"account,omitempty"`
	Login   string `json:"login,omitempty"`
}

// Scope is a node.Scope's stored form.
type Scope struct {
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
}

// Job is a node.Job's stored form. Its result is GitHub's own word.
type Job struct {
	Name    string `json:"name"`
	Slot    int    `json:"slot,omitempty"`
	Runner  string `json:"runner,omitempty"`
	Started string `json:"started,omitempty"`
	Ended   string `json:"ended,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Result  string `json:"result,omitempty"`
	Run     Run    `json:"run,omitzero"`
	// OutOfMemory reports a process the OS ended for memory, with the
	// memory budget in force then, if there was one.
	OutOfMemory  bool  `json:"out_of_memory,omitempty"`
	MemoryBudget int64 `json:"memory_budget,omitempty"`
}

// Run is a node.Run's stored form.
type Run struct {
	Repository string `json:"repository,omitempty"`
	ID         int64  `json:"id,omitempty"`
	Number     int64  `json:"number,omitempty"`
}

// Notice is a node.Notice's stored form.
type Notice struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Raised string `json:"raised,omitempty"`
	Slot   int    `json:"slot,omitempty"`
	Detail string `json:"detail,omitempty"`
	Job    Job    `json:"job,omitzero"`
}

// CancelledJob is a node.CancelledJob's stored form.
type CancelledJob struct {
	Slot   int    `json:"slot,omitempty"`
	Runner string `json:"runner,omitempty"`
	Job    string `json:"job"`
}

// Remove is a node.Remove's stored form.
type Remove struct {
	ID     string `json:"id"`
	Origin Origin `json:"origin,omitzero"`
	Local  bool   `json:"local,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

// Entry is a node.Entry's stored form.
type Entry struct {
	Seq  int64     `json:"seq"`
	At   string    `json:"at"`
	What Happening `json:"what"`
}

// Happening is a node.Happening's stored form: its kind, and the fields
// of that kind.
type Happening struct {
	Kind string `json:"kind"`
	// An applied command's.
	ID        string         `json:"id,omitempty"`
	Origin    *Origin        `json:"origin,omitempty"`
	Event     *Event         `json:"event,omitempty"`
	Cancelled []CancelledJob `json:"cancelled,omitempty"`
	Budget    *Budget        `json:"budget,omitempty"`
	// A job's.
	Job *Job `json:"job,omitempty"`
	// A Degraded episode's start, and a version refusal's slot.
	Slot   int    `json:"slot,omitempty"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// A Degraded episode's end.
	Began string `json:"began,omitempty"`
	Why   string `json:"why,omitempty"`
	// A raised notice's.
	Notice *Notice `json:"notice,omitempty"`
	// A version refusal's.
	Runner  string `json:"runner,omitempty"`
	Version string `json:"version,omitempty"`
}

// Event is an availability.Event's stored form: its kind, and the fields
// of that kind.
type Event struct {
	Kind       string      `json:"kind"`
	Base       int64       `json:"base,omitempty"`
	AutoResume *AutoResume `json:"auto_resume,omitempty"`
	SlotLimit  int         `json:"slot_limit,omitempty"`
	// A budget's halves: each one it sets, 0 to clear it, or nil to leave
	// it alone.
	Memory *int64 `json:"memory,omitempty"`
	CPUs   *int   `json:"cpus,omitempty"`
}

// The kinds of happening and of availability event.
const (
	appliedCommand       = "applied-command"
	jobKind              = "job"
	degradedBegan        = "degraded-began"
	degradedEnded        = "degraded-ended"
	noticeRaised         = "notice-raised"
	runnerVersionRefused = "runner-version-refused"

	pause             = "pause"
	pauseIndefinitely = "pause-indefinitely"
	resume            = "resume"
	emergencyStop     = "emergency-stop"
	editAutoResume    = "edit-auto-resume"
	turnOffAutoResume = "turn-off-auto-resume"
	setSlotLimit      = "set-slot-limit"
	setBudget         = "set-budget"
)

// FromNode returns n's stored form.
func FromNode(n node.Node) (Node, error) {
	var e encoder
	s := Node{
		Enrolled:     n.Enrolled,
		Availability: e.availability(n.Availability),
		Machine:      Machine(n.Machine),
		InForce:      Budget(n.InForce),
		Target:       n.Target,
		Scope:        Scope(n.Scope),
		RunnerApp:    RunnerApp(n.RunnerApp),
		Installed:    n.Installed,
		Discarded:    n.Discarded,
		Update: Update{
			Version:  n.Update.Version,
			Out:      n.Update.Out,
			Attempts: n.Update.Attempts,
			RetryAt:  Stamp(n.Update.RetryAt),
			Failed:   n.Update.Failed,
		},
		DegradedSince: Stamp(n.DegradedSince),
		Connected:     n.Connected,
		Removal:       Removal(n.Removal),
		Seq:           n.Seq,
	}
	for _, slot := range n.Slots {
		s.Slots = append(s.Slots, e.slot(slot))
	}
	for _, notice := range n.Notices {
		s.Notices = append(s.Notices, e.notice(notice))
	}
	for _, r := range n.Replies {
		s.Replies = append(s.Replies, Reply{ID: r.ID, At: Stamp(r.At), Refused: FromError(r.Refused), Settings: e.availability(r.Settings)})
	}
	for _, entry := range n.Outbox {
		s.Outbox = append(s.Outbox, e.entry(entry))
	}
	if n.IntentSet.What != nil {
		set := e.entry(n.IntentSet)
		s.IntentSet = &set
	}
	return s, e.err
}

// ToNode returns the node.Node s stores.
func (s Node) ToNode() (node.Node, error) {
	var d decoder
	n := node.Node{
		Enrolled:     s.Enrolled,
		Availability: d.availability(s.Availability),
		Machine:      node.Machine(s.Machine),
		InForce:      availability.Budget(s.InForce),
		Target:       s.Target,
		Scope:        node.Scope(s.Scope),
		RunnerApp:    node.RunnerApp(s.RunnerApp),
		Installed:    s.Installed,
		Discarded:    s.Discarded,
		Update: node.Update{
			Version:  s.Update.Version,
			Out:      s.Update.Out,
			Attempts: s.Update.Attempts,
			RetryAt:  d.time(s.Update.RetryAt),
			Failed:   s.Update.Failed,
		},
		DegradedSince: d.time(s.DegradedSince),
		Connected:     s.Connected,
		Removal:       node.Removal(s.Removal),
		Seq:           s.Seq,
	}
	for _, slot := range s.Slots {
		n.Slots = append(n.Slots, d.slot(slot))
	}
	for _, notice := range s.Notices {
		n.Notices = append(n.Notices, d.notice(notice))
	}
	for _, r := range s.Replies {
		n.Replies = append(n.Replies, node.Reply{ID: r.ID, At: d.time(r.At), Refused: r.Refused.ToError(), Settings: d.availability(r.Settings)})
	}
	for _, entry := range s.Outbox {
		n.Outbox = append(n.Outbox, d.entry(entry))
	}
	if s.IntentSet != nil {
		n.IntentSet = d.entry(*s.IntentSet)
	}
	return n, d.err
}

// FromOrigin returns o's stored form.
func FromOrigin(o node.Origin) (Origin, error) {
	var e encoder
	return e.origin(o), e.err
}

// ToOrigin returns the node.Origin s stores.
func (s Origin) ToOrigin() (node.Origin, error) {
	var d decoder
	return d.origin(s), d.err
}

// FromRemove returns r's stored form.
func FromRemove(r node.Remove) (Remove, error) {
	var e encoder
	return Remove{ID: r.ID, Origin: e.origin(r.Origin), Local: r.Local, Force: r.Force}, e.err
}

// ToRemove returns the node.Remove s stores.
func (s Remove) ToRemove() (node.Remove, error) {
	var d decoder
	return node.Remove{ID: s.ID, Origin: d.origin(s.Origin), Local: s.Local, Force: s.Force}, d.err
}

// FromReply returns r's stored form.
func FromReply(r node.Reply) (Reply, error) {
	var e encoder
	return Reply{ID: r.ID, At: Stamp(r.At), Refused: FromError(r.Refused), Settings: e.availability(r.Settings)}, e.err
}

// ToReply returns the node.Reply s stores.
func (s Reply) ToReply() (node.Reply, error) {
	var d decoder
	return node.Reply{ID: s.ID, At: d.time(s.At), Refused: s.Refused.ToError(), Settings: d.availability(s.Settings)}, d.err
}

// FromEvent returns ev's stored form: a command's availability event.
func FromEvent(ev availability.Event) (Event, error) {
	var e encoder
	s := e.event(ev)
	if s == nil {
		return Event{}, e.err
	}
	return *s, e.err
}

// ToEvent returns the availability.Event s stores.
func (s Event) ToEvent() (availability.Event, error) {
	var d decoder
	ev := d.event(&s)
	return ev, d.err
}

// FromReason returns r's stable code.
func FromReason(r node.Reason) (string, error) { return reasons.Code(r) }

// ToReason returns the node.Reason whose stable code is s.
func ToReason(s string) (node.Reason, error) { return reasons.Value(s) }

// FromEntry returns entry's stored form.
func FromEntry(entry node.Entry) (Entry, error) {
	var e encoder
	return e.entry(entry), e.err
}

// ToEntry returns the node.Entry s stores.
func (s Entry) ToEntry() (node.Entry, error) {
	var d decoder
	return d.entry(s), d.err
}

// encoder builds stored forms, keeping the first error.
type encoder struct{ err error }

func (e *encoder) fail(err error) {
	if e.err == nil && err != nil {
		e.err = err
	}
}

func code[T comparable](e *encoder, c Codes[T], v T) string {
	s, err := c.Code(v)
	e.fail(err)
	return s
}

func (e *encoder) availability(a availability.Node) Availability {
	return Availability{
		Intent:     code(e, intents, a.Intent),
		AutoResume: e.autoResume(a.AutoResume),
		SlotLimit:  a.SlotLimit,
		Budget:     Budget(a.Budget),
		Revision:   a.Revision,
		ResumeAt:   Stamp(a.ResumeAt),
	}
}

func (e *encoder) autoResume(a availability.AutoResume) AutoResume {
	s := AutoResume{On: a.On, Time: fmt.Sprintf("%02d:%02d", a.Time.Hour, a.Time.Minute), Days: []string{}}
	if a.Time.Hour < 0 || a.Time.Hour > 23 || a.Time.Minute < 0 || a.Time.Minute > 59 {
		e.fail(fmt.Errorf("%v isn't a time of day", a.Time))
	}
	if a.Days&^availability.EveryDay != 0 {
		e.fail(fmt.Errorf("days %b hold more than the days of the week", a.Days))
	}
	for d, name := range days {
		if a.Days.Has(time.Weekday(d)) {
			s.Days = append(s.Days, name)
		}
	}
	if a.Zone != nil {
		s.Zone = a.Zone.String()
	}
	return s
}

func (e *encoder) slot(s node.Slot) Slot {
	r := s.Runner
	return Slot{
		Number: s.Number,
		Phase:  code(e, phases, s.Phase),
		Runner: Runner{
			Name:           r.Name,
			Started:        Stamp(r.Started),
			Listening:      r.Listening,
			Late:           r.Late,
			Job:            r.Job,
			JobSince:       Stamp(r.JobSince),
			Result:         string(r.Result),
			WorkerUp:       r.WorkerUp,
			Version:        r.Version,
			Retirement:     code(e, retirements, r.Retirement),
			RetireAttempts: r.RetireAttempts,
			RetireAt:       Stamp(r.RetireAt),
			KillAt:         Stamp(r.KillAt),
			InterruptAt:    Stamp(r.InterruptAt),
		},
		Failure: Failure{
			Reason:   code(e, reasons, s.Failure.Reason),
			Detail:   s.Failure.Detail,
			Attempts: s.Failure.Attempts,
			Since:    Stamp(s.Failure.Since),
			RetryAt:  Stamp(s.Failure.RetryAt),
		},
		RunnerlessSince: Stamp(s.RunnerlessSince),
		Degraded:        s.Degraded,
		Ended:           e.job(s.Ended),
	}
}

func (e *encoder) origin(o node.Origin) Origin {
	return Origin{Surface: code(e, surfaces, o.Surface), Account: o.Account, Login: o.Login}
}

func (e *encoder) job(j node.Job) Job {
	return Job{
		Name: j.Name, Slot: j.Slot, Runner: j.Runner,
		Started: Stamp(j.Started), Ended: Stamp(j.Ended),
		Outcome: code(e, outcomes, j.Outcome), Result: string(j.Result), Run: Run(j.Run),
		OutOfMemory: j.OutOfMemory, MemoryBudget: j.MemoryBudget,
	}
}

func (e *encoder) notice(n node.Notice) Notice {
	return Notice{ID: n.ID, Kind: code(e, noticeKinds, n.Kind), Raised: Stamp(n.Raised), Slot: n.Slot, Detail: n.Detail, Job: e.job(n.Job)}
}

func (e *encoder) entry(entry node.Entry) Entry {
	return Entry{Seq: entry.Seq, At: Stamp(entry.At), What: e.happening(entry.What)}
}

func (e *encoder) happening(h node.Happening) Happening {
	switch h := h.(type) {
	case node.AppliedCommand:
		o := e.origin(h.Origin)
		s := Happening{Kind: appliedCommand, ID: h.ID, Origin: &o, Event: e.event(h.Event)}
		for _, c := range h.Cancelled {
			s.Cancelled = append(s.Cancelled, CancelledJob(c))
		}
		if _, ok := h.Event.(availability.SetBudget); ok {
			b := Budget(h.Budget)
			s.Budget = &b
		}
		return s
	case node.Job:
		j := e.job(h)
		return Happening{Kind: jobKind, Job: &j}
	case node.DegradedBegan:
		return Happening{Kind: degradedBegan, Slot: h.Slot, Reason: code(e, reasons, h.Reason), Detail: h.Detail}
	case node.DegradedEnded:
		return Happening{Kind: degradedEnded, Began: Stamp(h.Began), Why: code(e, endings, h.Why)}
	case node.NoticeRaised:
		n := e.notice(h.Notice)
		return Happening{Kind: noticeRaised, Notice: &n}
	case node.RunnerVersionRefused:
		return Happening{Kind: runnerVersionRefused, Slot: h.Slot, Runner: h.Runner, Version: h.Version}
	}
	e.fail(fmt.Errorf("%T has no stored form", h))
	return Happening{}
}

func (e *encoder) event(ev availability.Event) *Event {
	switch ev := ev.(type) {
	case availability.Pause:
		return &Event{Kind: pause}
	case availability.PauseIndefinitely:
		return &Event{Kind: pauseIndefinitely}
	case availability.Resume:
		return &Event{Kind: resume, Base: ev.Base}
	case availability.EmergencyStop:
		return &Event{Kind: emergencyStop}
	case availability.EditAutoResume:
		to := e.autoResume(ev.To)
		return &Event{Kind: editAutoResume, Base: ev.Base, AutoResume: &to}
	case availability.TurnOffAutoResume:
		return &Event{Kind: turnOffAutoResume}
	case availability.SetSlotLimit:
		return &Event{Kind: setSlotLimit, Base: ev.Base, SlotLimit: ev.To}
	case availability.SetBudget:
		s := &Event{Kind: setBudget, Base: ev.Base}
		if ev.SetMemory {
			s.Memory = &ev.Memory
		}
		if ev.SetCPUs {
			s.CPUs = &ev.CPUs
		}
		return s
	}
	e.fail(fmt.Errorf("%T has no stored form", ev))
	return nil
}

// decoder reads stored forms, keeping the first error.
type decoder struct{ err error }

func (d *decoder) fail(err error) {
	if d.err == nil && err != nil {
		d.err = err
	}
}

func value[T comparable](d *decoder, c Codes[T], s string) T {
	v, err := c.Value(s)
	d.fail(err)
	return v
}

func (d *decoder) time(s string) time.Time {
	t, err := Unstamp(s)
	d.fail(err)
	return t
}

func (d *decoder) availability(s Availability) availability.Node {
	return availability.Node{
		Intent:     value(d, intents, s.Intent),
		AutoResume: d.autoResume(s.AutoResume),
		SlotLimit:  s.SlotLimit,
		Budget:     availability.Budget(s.Budget),
		Revision:   s.Revision,
		ResumeAt:   d.time(s.ResumeAt),
	}
}

func (d *decoder) autoResume(s AutoResume) availability.AutoResume {
	a := availability.AutoResume{On: s.On}
	hour, minute, ok := strings.Cut(s.Time, ":")
	h, herr := strconv.Atoi(hour)
	m, merr := strconv.Atoi(minute)
	if !ok || herr != nil || merr != nil || len(s.Time) != 5 {
		d.fail(fmt.Errorf("%q isn't a stored time of day", s.Time))
	}
	a.Time = availability.TimeOfDay{Hour: h, Minute: m}
	for _, name := range s.Days {
		found := false
		for i, day := range days {
			if name == day {
				a.Days |= availability.DaysOf(time.Weekday(i))
				found = true
			}
		}
		if !found {
			d.fail(fmt.Errorf("%q isn't a stored day", name))
		}
	}
	if s.Zone != "" {
		zone, err := time.LoadLocation(s.Zone)
		if err != nil {
			d.fail(fmt.Errorf("the stored zone %q can't be loaded: %w", s.Zone, err))
		}
		a.Zone = zone
	}
	return a
}

func (d *decoder) slot(s Slot) node.Slot {
	r := s.Runner
	return node.Slot{
		Number: s.Number,
		Phase:  value(d, phases, s.Phase),
		Runner: node.Runner{
			Name:           r.Name,
			Started:        d.time(r.Started),
			Listening:      r.Listening,
			Late:           r.Late,
			Job:            r.Job,
			JobSince:       d.time(r.JobSince),
			Result:         node.Result(r.Result),
			WorkerUp:       r.WorkerUp,
			Version:        r.Version,
			Retirement:     value(d, retirements, r.Retirement),
			RetireAttempts: r.RetireAttempts,
			RetireAt:       d.time(r.RetireAt),
			KillAt:         d.time(r.KillAt),
			InterruptAt:    d.time(r.InterruptAt),
		},
		Failure: node.Failure{
			Reason:   value(d, reasons, s.Failure.Reason),
			Detail:   s.Failure.Detail,
			Attempts: s.Failure.Attempts,
			Since:    d.time(s.Failure.Since),
			RetryAt:  d.time(s.Failure.RetryAt),
		},
		RunnerlessSince: d.time(s.RunnerlessSince),
		Degraded:        s.Degraded,
		Ended:           d.job(s.Ended),
	}
}

func (d *decoder) origin(s Origin) node.Origin {
	return node.Origin{Surface: value(d, surfaces, s.Surface), Account: s.Account, Login: s.Login}
}

func (d *decoder) job(s Job) node.Job {
	return node.Job{
		Name: s.Name, Slot: s.Slot, Runner: s.Runner,
		Started: d.time(s.Started), Ended: d.time(s.Ended),
		Outcome: value(d, outcomes, s.Outcome), Result: node.Result(s.Result), Run: node.Run(s.Run),
		OutOfMemory: s.OutOfMemory, MemoryBudget: s.MemoryBudget,
	}
}

func (d *decoder) notice(s Notice) node.Notice {
	return node.Notice{ID: s.ID, Kind: value(d, noticeKinds, s.Kind), Raised: d.time(s.Raised), Slot: s.Slot, Detail: s.Detail, Job: d.job(s.Job)}
}

func (d *decoder) entry(s Entry) node.Entry {
	return node.Entry{Seq: s.Seq, At: d.time(s.At), What: d.happening(s.What)}
}

func (d *decoder) happening(s Happening) node.Happening {
	switch s.Kind {
	case appliedCommand:
		c := node.AppliedCommand{ID: s.ID, Event: d.event(s.Event)}
		if s.Origin != nil {
			c.Origin = d.origin(*s.Origin)
		}
		for _, j := range s.Cancelled {
			c.Cancelled = append(c.Cancelled, node.CancelledJob(j))
		}
		if s.Budget != nil {
			c.Budget = availability.Budget(*s.Budget)
		}
		return c
	case jobKind:
		if s.Job == nil {
			d.fail(fmt.Errorf("a stored job entry has no job"))
			return nil
		}
		return d.job(*s.Job)
	case degradedBegan:
		return node.DegradedBegan{Slot: s.Slot, Reason: value(d, reasons, s.Reason), Detail: s.Detail}
	case degradedEnded:
		return node.DegradedEnded{Began: d.time(s.Began), Why: value(d, endings, s.Why)}
	case noticeRaised:
		if s.Notice == nil {
			d.fail(fmt.Errorf("a stored notice entry has no notice"))
			return nil
		}
		return node.NoticeRaised{Notice: d.notice(*s.Notice)}
	case runnerVersionRefused:
		return node.RunnerVersionRefused{Slot: s.Slot, Runner: s.Runner, Version: s.Version}
	}
	d.fail(fmt.Errorf("%q isn't a stored kind of outbox entry", s.Kind))
	return nil
}

func (d *decoder) event(s *Event) availability.Event {
	if s == nil {
		d.fail(fmt.Errorf("a stored command has no event"))
		return nil
	}
	switch s.Kind {
	case pause:
		return availability.Pause{}
	case pauseIndefinitely:
		return availability.PauseIndefinitely{}
	case resume:
		return availability.Resume{Base: s.Base}
	case emergencyStop:
		return availability.EmergencyStop{}
	case editAutoResume:
		var to availability.AutoResume
		if s.AutoResume != nil {
			to = d.autoResume(*s.AutoResume)
		}
		return availability.EditAutoResume{Base: s.Base, To: to}
	case turnOffAutoResume:
		return availability.TurnOffAutoResume{}
	case setSlotLimit:
		return availability.SetSlotLimit{Base: s.Base, To: s.SlotLimit}
	case setBudget:
		e := availability.SetBudget{Base: s.Base}
		if s.Memory != nil {
			e.SetMemory, e.Memory = true, *s.Memory
		}
		if s.CPUs != nil {
			e.SetCPUs, e.CPUs = true, *s.CPUs
		}
		return e
	}
	d.fail(fmt.Errorf("%q isn't a stored kind of command", s.Kind))
	return nil
}

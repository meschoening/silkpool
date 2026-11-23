package panel

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/cli"
	"github.com/meschoening/silkpool/internal/control"
	"github.com/meschoening/silkpool/internal/wording"
)

// The panel's forms, from the shared UI package's parts and the panel's
// own: the verbs and Emergency stop, the slot-limit stepper, auto-resume's
// switch and editor, and a notice's Dismiss. Each sends its command to
// the agent through the panel's Sender, with the revision the panel
// showed where the command takes one, and answers with the panel, which
// says what came of it if it didn't apply. Revisions and
// resends work as they do in the CLI, through the tray's link.

// command sends one of the node's verbs, or Emergency stop from its
// confirmation.
func (p *Panel) command(w http.ResponseWriter, r *http.Request) {
	var e availability.Event
	switch r.PostFormValue("verb") {
	case "pause":
		e = availability.Pause{}
	case "pause-indefinitely":
		e = availability.PauseIndefinitely{}
	case "resume":
		e = availability.Resume{Base: revision(r)}
	case "stop":
		e = availability.EmergencyStop{}
	default:
		http.Error(w, "That isn't one of the node's commands.", http.StatusBadRequest)
		return
	}
	p.sendCommand(w, r, e)
}

// slotLimit sets the slot limit the stepper's button gives.
func (p *Panel) slotLimit(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(r.PostFormValue("limit"))
	if err != nil {
		http.Error(w, "The slot limit is a number.", http.StatusBadRequest)
		return
	}
	p.sendCommand(w, r, availability.SetSlotLimit{Base: revision(r), To: limit})
}

// autoResume saves auto-resume, from its switch or its editor. Turning it
// off and changing nothing else is the off-only edit, which always
// applies, as the CLI's `auto-resume off` is; anything else is based on
// the revision the form was read at.
func (p *Panel) autoResume(w http.ResponseWriter, r *http.Request) {
	draft, err := parseAutoResume(r)
	if err != nil {
		p.mu.Lock()
		p.say(wording.Sentence(err.Error()))
		p.mu.Unlock()
		p.answer(w)
		return
	}
	p.mu.Lock()
	before := p.shown.node.Availability.AutoResume
	p.mu.Unlock()
	var e availability.Event = availability.EditAutoResume{Base: revision(r), To: draft}
	if !draft.On && sameSchedule(draft, before) {
		e = availability.TurnOffAutoResume{}
	}
	p.sendCommand(w, r, e)
}

// sameSchedule reports whether a and b resume at the same times.
func sameSchedule(a, b availability.AutoResume) bool {
	return a.Time == b.Time && a.Days == b.Days && a.Zone.String() == b.Zone.String()
}

// parseAutoResume reads auto-resume's settings from the form: on, the
// time as HH:MM, each day as its number, and the zone by its IANA name,
// as the dashboard reads them.
func parseAutoResume(r *http.Request) (availability.AutoResume, error) {
	var out availability.AutoResume
	_ = r.ParseForm()
	out.On = r.PostForm.Get("on") != ""
	for _, v := range r.PostForm["days"] {
		if day, err := strconv.Atoi(v); err == nil && day >= 0 && day <= 6 {
			out.Days |= availability.DaysOf(time.Weekday(day))
		}
	}
	t, err := time.Parse("15:04", r.PostForm.Get("time"))
	if err != nil {
		return out, errors.New("write the time as HH:MM, such as 07:30")
	}
	out.Time = availability.TimeOfDay{Hour: t.Hour(), Minute: t.Minute()}
	zone := r.PostForm.Get("zone")
	z, err := time.LoadLocation(zone)
	if err != nil || zone == "" || zone == "Local" {
		return out, errors.New("choose a time zone")
	}
	out.Zone = z
	return out, nil
}

// dismiss dismisses one of the node's notices.
func (p *Panel) dismiss(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("notice"), 10, 64)
	if err != nil {
		http.Error(w, "That isn't one of the node's notices.", http.StatusBadRequest)
		return
	}
	p.deliver(w, r, func(ctx context.Context) (control.Reply, error) { return p.send.Dismiss(ctx, []int64{id}) })
}

// revision is the node's revision the form was read at.
func revision(r *http.Request) int64 {
	v, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	return v
}

// sendCommand sends the availability command e.
func (p *Panel) sendCommand(w http.ResponseWriter, r *http.Request, e availability.Event) {
	p.deliver(w, r, func(ctx context.Context) (control.Reply, error) { return p.send.Command(ctx, e) })
}

// deliver sends a command through do, unless the panel shows no node,
// and so no controls, because it can't read the agent or the machine
// isn't enrolled, and answers with the panel, saying what came of the
// command if it didn't apply.
func (p *Panel) deliver(w http.ResponseWriter, r *http.Request, do func(context.Context) (control.Reply, error)) {
	p.mu.Lock()
	ready, v := p.have && p.instead() == nil, wording.View{Name: p.name()}
	p.mu.Unlock()
	if ready {
		ctx, cancel := context.WithTimeout(r.Context(), sendFor)
		reply, err := do(ctx)
		cancel()
		p.mu.Lock()
		p.say(outcome(v, reply, err))
		p.mu.Unlock()
	}
	p.answer(w)
}

// say makes the panel say text, what came of its last command, for a
// while, or say nothing if text is empty. p.mu must be held.
func (p *Panel) say(text string) {
	p.said, p.saidUntil = text, p.now().Add(saidFor)
}

// outcome words what came of a command from the panel on the node v
// names: nothing if it applied, else why not.
func outcome(v wording.View, reply control.Reply, err error) string {
	var mismatch cli.VersionMismatch
	switch {
	case err == nil && reply.Refused == nil:
		return ""
	case err == nil:
		return v.TrayRefused(reply.Refused.Code, reply.Refused.Message)
	case errors.Is(err, cli.ErrNotConfirmed):
		return wording.TrayNotConfirmed
	case errors.As(err, &mismatch):
		return wording.TrayOtherVersion(mismatch.Tray, mismatch.Service)
	}
	return wording.TrayCouldntSend(err)
}

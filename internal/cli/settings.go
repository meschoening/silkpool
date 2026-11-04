package cli

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/store"
	"github.com/meschoening/silkpool/internal/wording"
)

// settingsJSON is a read of the settings as --json prints it: the
// revision a script passes back with --revision, and the settings
// snapshot, as a command's reply has them.
type settingsJSON struct {
	Revision int64              `json:"revision"`
	Settings store.Availability `json:"settings"`
}

// printSettings prints the settings r read, as JSON.
func (s *session) printSettings(r read) {
	a := r.status.Node.Availability
	s.printJSON(settingsJSON{Revision: a.Revision, Settings: a})
}

// autoResumeUsage is every form of auto-resume.
const autoResumeUsage = `silkpool auto-resume [--json]
       silkpool auto-resume on  [--at <hh:mm>] [--days <days>] [--zone <zone>] [--revision N] [--json]
       silkpool auto-resume set [--at <hh:mm>] [--days <days>] [--zone <zone>] [--revision N] [--json]
       silkpool auto-resume off [--json]`

// runAutoResume shows the auto-resume settings and what they mean now,
// or changes them: on turns auto-resume on, set changes its fields and
// leaves on or off alone, and off turns it off.
func runAutoResume(ctx context.Context, s *session, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "on", "set":
			return editAutoResume(ctx, s, args[0], args[1:])
		case "off":
			return turnOffAutoResume(ctx, s, args[1:])
		}
	}
	fs := s.flags("auto-resume", autoResumeUsage)
	r, code := s.begin(ctx, func() (int, bool) { _, code, ok := s.parse(fs, args, 0); return code, ok })
	if code != ExitOK {
		return code
	}
	if s.json {
		s.printSettings(r)
	} else {
		s.autoResume(r.view)
	}
	return ExitOK
}

// editAutoResume sends on's or set's edit. The fields it isn't given
// keep the values the CLI read, under the revision it read, or the one
// --revision gives.
func editAutoResume(ctx context.Context, s *session, form string, args []string) int {
	fs := s.flags("auto-resume "+form, fmt.Sprintf("silkpool auto-resume %s [--at <hh:mm>] [--days <days>] [--zone <zone>] [--revision N] [--json]", form))
	rev := revisionFlag(fs)
	atText := fs.String("at", "", "")
	daysText := fs.String("days", "", "")
	zoneText := fs.String("zone", "", "")
	var at availability.TimeOfDay
	var days availability.Weekdays
	var zone *time.Location
	set := map[string]bool{}
	r, code := s.begin(ctx, func() (int, bool) {
		if _, code, ok := s.parse(fs, args, 0); !ok {
			return code, false
		}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if form == "set" && !set["at"] && !set["days"] && !set["zone"] {
			return s.usage(fs, "Give at least one of --at, --days and --zone."), false
		}
		if set["at"] {
			var ok bool
			if at, ok = parseAt(*atText); !ok {
				return s.usage(fs, fmt.Sprintf("%q isn't a 24-hour time such as 07:30.", *atText)), false
			}
		}
		if set["days"] {
			var ok bool
			if days, ok = parseDays(*daysText); !ok {
				return s.usage(fs, fmt.Sprintf("%q isn't a set of days such as mon-fri or mon,wed,fri.", *daysText)), false
			}
		}
		if set["zone"] && !local(*zoneText) {
			var ok bool
			if zone, ok = loadZone(*zoneText); !ok {
				return s.usage(fs, fmt.Sprintf("%q isn't an IANA time zone such as America/Chicago, or local.", *zoneText)), false
			}
		}
		return ExitOK, true
	})
	if code != ExitOK {
		return code
	}
	if set["zone"] && local(*zoneText) {
		var ok bool
		if zone, ok = loadZone(r.status.Machine.Zone); !ok {
			return s.usage(fs, "This machine's own time zone has no IANA name, so name one, such as --zone America/Chicago.")
		}
	}
	to := r.node.Availability.AutoResume
	if form == "on" {
		to.On = true
	}
	if set["at"] {
		to.Time = at
	}
	if set["days"] {
		to.Days = days
	}
	if set["zone"] {
		to.Zone = zone
	}
	b, given := base(r, *rev)
	_, after, code := s.command(ctx, r, availability.EditAutoResume{Base: b, To: to}, given)
	if code != ExitOK || s.json {
		return code
	}
	s.autoResume(r.with(after))
	return ExitOK
}

// turnOffAutoResume sends the off-only edit, which always applies, and
// says so when it turns Paused into Paused indefinitely.
func turnOffAutoResume(ctx context.Context, s *session, args []string) int {
	fs := s.flags("auto-resume off", "silkpool auto-resume off [--json]")
	r, code := s.begin(ctx, func() (int, bool) { _, code, ok := s.parse(fs, args, 0); return code, ok })
	if code != ExitOK {
		return code
	}
	_, after, code := s.command(ctx, r, availability.TurnOffAutoResume{}, false)
	if code != ExitOK || s.json {
		return code
	}
	if r.node.Availability.Intent == availability.Paused && after.Intent == availability.PausedIndefinitely {
		fmt.Fprintln(s.env.Stdout, r.view.IntentNow(after))
	}
	s.autoResume(r.with(after))
	return ExitOK
}

// local reports whether --zone names the machine's own zone.
func local(zone string) bool { return strings.EqualFold(zone, "local") }

// loadZone loads a zone by its IANA name. Go would load "" as UTC and
// "Local" as whatever the machine's zone is at the time, which isn't a
// name auto-resume can keep.
func loadZone(name string) (*time.Location, bool) {
	if name == "" || local(name) {
		return nil, false
	}
	zone, err := time.LoadLocation(name)
	return zone, err == nil
}

// parseAt parses --at's 24-hour time, as 07:30 or 7:30.
func parseAt(text string) (availability.TimeOfDay, bool) {
	h, m, ok := strings.Cut(text, ":")
	if !ok || len(h) < 1 || len(h) > 2 || len(m) != 2 || !digits(h) || !digits(m) {
		return availability.TimeOfDay{}, false
	}
	hour, _ := strconv.Atoi(h)
	minute, _ := strconv.Atoi(m)
	if hour > 23 || minute > 59 {
		return availability.TimeOfDay{}, false
	}
	return availability.TimeOfDay{Hour: hour, Minute: minute}, true
}

// dayNames are the days as --days names them, Monday first.
var dayNames = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// parseDays parses --days: three-letter names, comma lists and ranges,
// as mon-fri or mon,wed,fri. A range runs Monday to Sunday, and one that
// passes Sunday wraps round to Monday, so fri-mon is a long weekend.
func parseDays(text string) (availability.Weekdays, bool) {
	var w availability.Weekdays
	for _, part := range strings.Split(text, ",") {
		from, to, isRange := strings.Cut(part, "-")
		first := slices.Index(dayNames, strings.ToLower(from))
		last := first
		if isRange {
			last = slices.Index(dayNames, strings.ToLower(to))
		}
		if first < 0 || last < 0 {
			return 0, false
		}
		for i := first; ; i = (i + 1) % 7 {
			w |= availability.DaysOf(time.Weekday((i + 1) % 7))
			if i == last {
				break
			}
		}
	}
	return w, true
}

// digits reports whether text is only ASCII digits.
func digits(text string) bool {
	for _, c := range text {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// runSlotLimit shows the slot limit, or sets it to <n>. A raise is based
// on the revision the CLI reads, or the one --revision gives; a lowering
// always applies.
func runSlotLimit(ctx context.Context, s *session, args []string) int {
	fs := s.flags("slot-limit", "silkpool slot-limit [--json]\n       silkpool slot-limit <n> [--revision N] [--json]")
	rev := revisionFlag(fs)
	var to int
	r, code := s.begin(ctx, func() (int, bool) {
		rest, code, ok := s.parseUpTo(fs, args, 0, 1)
		switch {
		case !ok:
			return code, false
		case len(rest) == 0 && *rev != 0:
			// --revision guards a change, and there's none to guard.
			return s.usage(fs, ""), false
		case len(rest) == 0:
			return ExitOK, true
		}
		n, err := strconv.Atoi(rest[0])
		if err != nil || n < 1 || n > availability.MaxSlotLimit {
			return s.usage(fs, fmt.Sprintf("The slot limit is a number from 1 to %d, and %q isn't one.", availability.MaxSlotLimit, rest[0])), false
		}
		to = n
		return ExitOK, true
	})
	if code != ExitOK {
		return code
	}
	if to == 0 {
		if s.json {
			s.printSettings(r)
		} else {
			fmt.Fprintln(s.env.Stdout, wording.SlotLimit(r.node.Availability.SlotLimit))
		}
		return ExitOK
	}
	b, given := base(r, *rev)
	_, after, code := s.command(ctx, r, availability.SetSlotLimit{Base: b, To: to}, given)
	if code != ExitOK || s.json {
		return code
	}
	fmt.Fprintln(s.env.Stdout, r.view.SlotLimitSet(after))
	return ExitOK
}

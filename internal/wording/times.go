package wording

import (
	"fmt"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
)

// Clock is the clock and its zone that a surface prints times in: the
// node's auto-resume zone. A time names its zone when that isn't the
// machine's own, by the zone's abbreviation.
type Clock struct {
	Now time.Time
	// Zone is the zone times print in.
	Zone *time.Location
	// Own is the machine's own zone, by its IANA name.
	Own string
}

func (c Clock) zone() *time.Location {
	if c.Zone == nil {
		return time.UTC
	}
	return c.Zone
}

// TimeOfDay is t's time of day, "7:30 AM".
func (c Clock) TimeOfDay(t time.Time) string {
	t = t.In(c.zone())
	s := t.Format("3:04 PM")
	if c.Zone != nil && c.Zone.String() != c.Own {
		s += " " + t.Format("MST")
	}
	return s
}

// day is how far t's day is from today: "" for today, "tomorrow",
// "yesterday", a weekday within the week either side, or a date.
func (c Clock) day(t time.Time) string {
	z := c.zone()
	ty, tm, td := t.In(z).Date()
	ny, nm, nd := c.Now.In(z).Date()
	days := int(time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC).Sub(time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	switch {
	case days == 0:
		return ""
	case days == 1:
		return "tomorrow"
	case days == -1:
		return "yesterday"
	case days > -7 && days < 7:
		return t.In(z).Format("Mon")
	}
	return t.In(z).Format("Mon 2 Jan")
}

// When is a time to come or gone by, as a sentence uses it after
// "until": "7:30 AM", "tomorrow 7:30 AM", "Fri 7:30 AM".
func (c Clock) When(t time.Time) string {
	if d := c.day(t); d != "" {
		return d + " " + c.TimeOfDay(t)
	}
	return c.TimeOfDay(t)
}

// At is when something happened, as a sentence ends with it: "at 4:42
// PM", "yesterday at 4:42 PM".
func (c Clock) At(t time.Time) string {
	if d := c.day(t); d != "" {
		return d + " at " + c.TimeOfDay(t)
	}
	return "at " + c.TimeOfDay(t)
}

// Duration is how long something has lasted: "under a minute", "12
// min", "2 h 5 min".
func Duration(d time.Duration) string {
	minutes := int((d + 30*time.Second) / time.Minute)
	switch {
	case minutes < 1:
		return "under a minute"
	case minutes < 60:
		return fmt.Sprintf("%d min", minutes)
	case minutes%60 == 0:
		return fmt.Sprintf("%d h", minutes/60)
	}
	return fmt.Sprintf("%d h %d min", minutes/60, minutes%60)
}

// Soon is how long until something comes: "in 40 s", "in 2 min".
func Soon(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("in %d s", max(0, int((d+time.Second-1)/time.Second)))
	}
	return "in " + Duration(d)
}

// weekdays are the days of the week, Monday first, as the words list
// them.
var weekdays = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}

// Days is a set of days as the words name it: "Every day", "Mon–Fri",
// "Weekends", or a list such as "Mon, Wed, Fri".
func Days(w availability.Weekdays) string {
	switch w {
	case availability.EveryDay:
		return "Every day"
	case availability.DaysOf(time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday):
		return "Mon–Fri"
	case availability.DaysOf(time.Saturday, time.Sunday):
		return "Weekends"
	}
	var names []string
	for _, d := range weekdays {
		if w.Has(d) {
			names = append(names, d.String()[:3])
		}
	}
	return strings.Join(names, ", ")
}

// Hour is a time of day as the words print it: "7:30 AM".
func Hour(t availability.TimeOfDay) string {
	return time.Date(2000, 1, 1, t.Hour, t.Minute, 0, 0, time.UTC).Format("3:04 PM")
}

// plural is n and noun, made plural unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// list joins words as a sentence lists them: "a", "a and b", "a, b and
// c".
func list(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// Sentence makes s a sentence: its first letter upper case, ending in a
// full stop. A first word with capitals of its own, such as macOS, keeps
// its case.
func Sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	first, _, _ := strings.Cut(s, " ")
	if strings.ToLower(first[1:]) == first[1:] {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}

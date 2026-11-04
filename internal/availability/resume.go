package availability

import "time"

// Next returns the first resume time strictly after after, whether or not
// auto-resume is on, or the zero Time if a selects no days. Each selected
// day has at most one resume time, at a's time of day in a's zone: the
// first occurrence of it when the clocks go back, and the first instant
// after the gap when they skip it. Next panics if a has no zone.
func (a AutoResume) Next(after time.Time) time.Time {
	y, m, d := after.In(a.Zone).Date()
	// Start the day before, in case a gap carries that day's resume time
	// past midnight.
	for i := -1; i <= 7; i++ {
		day := time.Date(y, m, d+i, 0, 0, 0, 0, time.UTC)
		if !a.Days.Has(day.Weekday()) {
			continue
		}
		wall := day.Add(time.Duration(a.Time.Hour)*time.Hour + time.Duration(a.Time.Minute)*time.Minute)
		if at := firstReading(a.Zone, wall); at.After(after) {
			return at
		}
	}
	return time.Time{}
}

// firstReading returns the first instant at which a clock in loc reads
// wall, a wall time given as if in UTC, or the first instant after the
// gap if the clock skips it. time.Date doesn't promise either, and
// ZoneBounds can't be trusted: with Go's own tz database it stops
// advancing at the end of leap years.
func firstReading(loc *time.Location, wall time.Time) time.Time {
	offset := func(t time.Time) time.Duration {
		_, seconds := t.In(loc).Zone()
		return time.Duration(seconds) * time.Second
	}
	// The clock can read wall only at wall minus an offset in force within
	// a day of it. The larger offset reads it first.
	hi, lo := offset(wall.Add(-24*time.Hour)), offset(wall.Add(24*time.Hour))
	if hi < lo {
		hi, lo = lo, hi
	}
	for _, off := range []time.Duration{hi, lo} {
		if at := wall.Add(-off); offset(at) == off {
			return at.In(loc)
		}
	}
	// The clock skips wall, jumping from lo to hi somewhere between the
	// two. Find the second it jumps.
	from, to := wall.Add(-hi).Unix(), wall.Add(-lo).Unix()
	for to-from > 1 {
		mid := from + (to-from)/2
		if offset(time.Unix(mid, 0)) == lo {
			from = mid
		} else {
			to = mid
		}
	}
	return time.Unix(to, 0).In(loc)
}

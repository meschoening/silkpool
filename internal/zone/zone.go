// Package zone is Silkpool's one default time zone, Central
// (America/Chicago), and the rule that falls back on it: wherever a
// machine's zone is missing or can't be found, Silkpool uses Central. A
// machine really set to UTC stays on UTC; only a missing or unfound zone
// becomes Central.
package zone

import (
	"sync"
	"time"

	// Windows has no IANA database of its own, and Default must load
	// everywhere.
	_ "time/tzdata"
)

// DefaultName is the zone Silkpool falls back on, by its IANA name.
const DefaultName = "America/Chicago"

// Default is DefaultName, loaded.
var Default = sync.OnceValue(func() *time.Location {
	z, err := time.LoadLocation(DefaultName)
	if err != nil {
		panic(err)
	}
	return z
})

// Load loads the zone named name, an IANA name, and reports whether it
// did. It doesn't for "", which Go would load as UTC, "Local", which
// follows the OS and isn't a name a schedule can keep, or a name that
// doesn't load.
func Load(name string) (*time.Location, bool) {
	if name == "" || name == "Local" {
		return nil, false
	}
	z, err := time.LoadLocation(name)
	return z, err == nil
}

// Own is a machine's own zone, from its IANA name as its agent read it
// from the OS, or Default if name names none: it's empty, "Local", or
// doesn't load.
func Own(name string) *time.Location {
	if z, ok := Load(name); ok {
		return z
	}
	return Default()
}

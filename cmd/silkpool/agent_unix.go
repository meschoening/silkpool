//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/zone"
)

// await waits for the agent or its control server to end, then ends the
// other and waits for it too, so neither outlives the store. It returns
// the first one's error.
func await(ran, served <-chan error, cancel func()) error {
	var err error
	select {
	case err = <-ran:
		cancel()
		<-served
	case err = <-served:
		cancel()
		<-ran
	}
	return err
}

// zoneName is the OS's time zone by its IANA name, or "" if it names
// none, which the agent enrolls in Central for.
func zoneName() string {
	return zoneFrom(os.Getenv("TZ"), "/etc/localtime", "/etc/timezone")
}

// zoneFrom is the zone the OS names by its IANA name: tz, TZ's value, if
// it names one that loads, else what the link localtime points to under
// zoneinfo/, else the name in the file timezone, or "" if none names one
// that loads. A machine really set to UTC names it, as TZ=UTC or a link
// to zoneinfo/UTC or zoneinfo/Etc/UTC, so "" is never taken for UTC.
func zoneFrom(tz, localtime, timezone string) string {
	loads := func(name string) bool { _, ok := zone.Load(name); return ok }
	if tz := strings.TrimPrefix(tz, ":"); !filepath.IsAbs(tz) && loads(tz) {
		return tz
	}
	if link, err := os.Readlink(localtime); err == nil {
		if _, name, ok := strings.Cut(link, "zoneinfo/"); ok && loads(name) {
			return name
		}
	}
	if b, err := os.ReadFile(timezone); err == nil {
		if name := strings.TrimSpace(string(b)); loads(name) {
			return name
		}
	}
	return ""
}

// freeDisk is the space free to root in dir's file system.
func freeDisk(dir string) (uint64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(dir, &s); err != nil {
		return 0, err
	}
	return s.Bfree * uint64(s.Bsize), nil
}

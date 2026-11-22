package linux

import (
	"strconv"
	"strings"
)

// CompareVersions compares two versions that CheckVersion takes
// by semver's precedence, returning -1, 0 or +1.
func CompareVersions(a, b string) int {
	aCore, aPre, _ := strings.Cut(a, "-")
	bCore, bPre, _ := strings.Cut(b, "-")
	if c := compareFields(strings.Split(aCore, "."), strings.Split(bCore, ".")); c != 0 {
		return c
	}
	switch {
	case aPre == bPre:
		return 0
	case aPre == "":
		return 1 // a release follows its prereleases
	case bPre == "":
		return -1
	}
	return compareFields(strings.Split(aPre, "."), strings.Split(bPre, "."))
}

// compareFields compares dot-separated identifiers as semver does:
// numbers numerically and below words, words in ASCII order, and a
// shorter list below a longer one it begins.
func compareFields(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		an, aErr := strconv.ParseUint(a[i], 10, 64)
		bn, bErr := strconv.ParseUint(b[i], 10, 64)
		switch {
		case aErr == nil && bErr == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aErr == nil:
			return -1
		case bErr == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

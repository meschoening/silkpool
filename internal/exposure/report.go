package exposure

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/meschoening/silkpool/internal/secret"
)

// A Request is what the agent asks of the helper (Run): the paths the
// read check probes, and the folders the survey walks, with those it
// skips.
type Request struct {
	Probe []string `json:"probe,omitempty"`
	Walk  []string `json:"walk,omitempty"`
	Skip  []string `json:"skip,omitempty"`
}

// A Report is what the helper found, as the account it ran as.
type Report struct {
	// UID is the account's, as the helper ran.
	UID int `json:"uid"`
	// Visible are the probed paths the account can see.
	Visible []string `json:"visible,omitempty"`
	// Places are the shared places: the folders the walk found that the
	// account can list and create entries in, and doesn't own.
	Places []Place `json:"places,omitempty"`
	// Unlisted are the folders the walk found that the account can enter
	// but not list. Nothing inside them was walked, and none is a place.
	Unlisted []string `json:"unlisted,omitempty"`
	// Listed counts the folders the walk listed.
	Listed int `json:"listed,omitempty"`
	// Error says why the helper couldn't do what it was asked.
	Error string `json:"error,omitempty"`
}

// A Place is a folder outside Silkpool's own that the account can create
// entries in, and doesn't own.
type Place struct {
	Path  string `json:"path"`
	Owner int    `json:"owner"`
	// Sticky reports that only an entry's owner may delete or rename it.
	Sticky bool `json:"sticky,omitempty"`
	// Dev and Ino are the folder's device and inode, as the walk listed
	// it, which the sweep checks: a folder put in its stead since, which
	// the account may not be able to list, isn't the place.
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// A Kind is what a hit is.
type Kind int

const (
	// Read is something outside the account's own that it can read.
	Read Kind = iota
	// Volume is a volume in /Volumes that the account can read.
	Volume
	// Change is a folder of the OS's software the account can change.
	Change
	// Unlisted is a folder of the OS's software the account can enter
	// but not list.
	Unlisted
)

// A Hit is something the account mustn't see or change.
type Hit struct {
	Path string
	Kind Kind
}

// Hits returns the hits of a check whose read check saw visible, beside
// survey, the latest survey: what it saw, in its order, then each shared
// place in the folders of the OS's software that isn't sticky, so that
// the account could replace what's in it, and each folder there that the
// account can enter but not list, by path.
func Hits(visible []string, survey Report) []Hit {
	var hits []Hit
	for _, p := range visible {
		kind := Read
		if under(p, "/Volumes") {
			kind = Volume
		}
		hits = append(hits, Hit{p, kind})
	}
	var changes []Hit
	changeable := func(p string) bool {
		return slices.ContainsFunc(ChangeRoots, func(root string) bool { return under(p, root) })
	}
	for _, p := range survey.Places {
		if !p.Sticky && changeable(p.Path) {
			changes = append(changes, Hit{p.Path, Change})
		}
	}
	for _, p := range survey.Unlisted {
		if changeable(p) && !slices.ContainsFunc(changes, func(h Hit) bool { return h.Path == p }) {
			changes = append(changes, Hit{p, Unlisted})
		}
	}
	slices.SortFunc(changes, func(a, b Hit) int { return strings.Compare(a.Path, b.Path) })
	return append(hits, changes...)
}

// Detail is the detail of a check that found hits: the first, with its
// fix, and how many more there are. The paths are the machine's, so
// they're scrubbed.
func Detail(hits []Hit) string {
	h := hits[0]
	p := quote(secret.Scrub(h.Path))
	var d string
	switch h.Kind {
	case Volume:
		d = fmt.Sprintf("%s can be read by Silkpool's service account: eject it, or take away other accounts' access to it", p)
	case Change:
		d = fmt.Sprintf("%s can be changed by Silkpool's service account: take away other accounts' write access with chmod o-w %s", p, p)
	case Unlisted:
		d = fmt.Sprintf("Silkpool's service account can enter %s but not list it, so Silkpool can't show that nothing in it can be changed: "+
			"take away other accounts' access with chmod o-x %s", p, p)
	default:
		d = fmt.Sprintf("%s can be read by Silkpool's service account: take away other accounts' access with chmod o-rwx %s", p, p)
	}
	if more := len(hits) - 1; more > 0 {
		d += fmt.Sprintf(" (and %d more)", more)
	}
	return d
}

// plain is a path a shell takes as it is.
var plain = regexp.MustCompile(`^[A-Za-z0-9_./+:@%,=-]+$`)

// quote quotes path for a shell, if it needs it.
func quote(path string) string {
	if plain.MatchString(path) {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

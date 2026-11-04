package node

import (
	"fmt"
	"strings"
)

// MaxNameLength is the longest a node's name may be: what's left of
// GitHub's 64 characters for a runner's name after the
// "-<slot>-<4 random>" the Hub adds, with a two-digit slot.
const MaxNameLength = 56

// InvalidName refuses a node name that breaks the rule: 1 to
// MaxNameLength ASCII letters, digits and hyphens, starting and ending
// with a letter or digit. Case is kept, and the Hub ignores it when it
// checks that a name is free. The rule keeps a name safe in unit names,
// paths and shells, and makes ignoring case unambiguous.
type InvalidName struct{ Name string }

func (e InvalidName) Error() string {
	return fmt.Sprintf("the name %q isn't a valid node name: use 1 to %d letters, digits and hyphens, starting and ending with a letter or digit", e.Name, MaxNameLength)
}

// CheckName returns the InvalidName that refuses name, or nil.
func CheckName(name string) error {
	if name == "" || len(name) > MaxNameLength || name[0] == '-' || name[len(name)-1] == '-' {
		return InvalidName{Name: name}
	}
	for i := range len(name) {
		if !nameChar(name[i]) {
			return InvalidName{Name: name}
		}
	}
	return nil
}

// NameFromHostname returns the name a machine offers when its
// administrator enrolls it without one: its hostname host, with every
// character but an ASCII letter, digit or hyphen turned into a hyphen,
// cut to MaxNameLength, and without hyphens at either end. The result
// is a valid name, or empty if host has no letter or digit to keep.
func NameFromHostname(host string) string {
	var b strings.Builder
	for _, r := range host {
		if r < 0x80 && nameChar(byte(r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.TrimLeft(b.String(), "-")
	return strings.TrimRight(name[:min(len(name), MaxNameLength)], "-")
}

// nameChar reports whether c may be in a name.
func nameChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-'
}

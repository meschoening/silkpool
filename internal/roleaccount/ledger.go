package roleaccount

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// guidForm is a GeneratedUID as newGUID writes it.
var guidForm = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)

// MarshalText writes the ledger as lines: "group <GUID> owned|pending"
// and "user <GUID> owned|pending" for each claim, "id <ID>", and
// "rebuild" while the account's folders are to be emptied.
func (l Ledger) MarshalText() ([]byte, error) {
	var b strings.Builder
	for _, c := range []struct {
		kind string
		c    Claim
	}{{"group", l.Group}, {"user", l.User}} {
		if c.c.GUID == "" {
			continue
		}
		if !guidForm.MatchString(c.c.GUID) {
			return nil, fmt.Errorf("the %s's GeneratedUID %q isn't one Silkpool makes", c.kind, c.c.GUID)
		}
		state := "pending"
		if c.c.Owned {
			state = "owned"
		}
		fmt.Fprintf(&b, "%s %s %s\n", c.kind, c.c.GUID, state)
	}
	if l.ID != 0 {
		fmt.Fprintf(&b, "id %d\n", l.ID)
	}
	if l.Rebuild {
		b.WriteString("rebuild\n")
	}
	return []byte(b.String()), nil
}

// UnmarshalText reads what MarshalText wrote, and fails on anything
// else: a ledger it can't read whole names nothing as Silkpool's.
func (l *Ledger) UnmarshalText(b []byte) error {
	var got Ledger
	seen := map[string]bool{}
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasSuffix(line, "\n") || seen[f[0]] {
			return fmt.Errorf("the ledger's line %q", line)
		}
		seen[f[0]] = true
		switch {
		case (f[0] == "group" || f[0] == "user") && len(f) == 3 && guidForm.MatchString(f[1]) && (f[2] == "owned" || f[2] == "pending"):
			c := Claim{GUID: f[1], Owned: f[2] == "owned"}
			if f[0] == "group" {
				got.Group = c
			} else {
				got.User = c
			}
		case f[0] == "rebuild" && len(f) == 1:
			got.Rebuild = true
		case f[0] == "id" && len(f) == 2:
			n, err := strconv.Atoi(f[1])
			if err != nil || strconv.Itoa(n) != f[1] || !inRange(n) {
				return fmt.Errorf("the ledger's line %q", line)
			}
			got.ID = n
		default:
			return fmt.Errorf("the ledger's line %q", line)
		}
	}
	*l = got
	return nil
}

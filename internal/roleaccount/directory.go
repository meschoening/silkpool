package roleaccount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// A Kind is a type of directory record: Users or Groups.
type Kind string

const (
	Users  Kind = "Users"
	Groups Kind = "Groups"
)

// ErrNotFound is a Directory's error for a record that isn't there.
var ErrNotFound = errors.New("eDSRecordNotFound")

// ErrPermission is a Directory's error for a change macOS refused. A
// process whose app doesn't have Full Disk Access gets it for deleting
// any user record, though not a group: a root LaunchDaemon, or sudo in
// a Terminal without it, as #192's probe found on macOS 26.5.1.
var ErrPermission = errors.New("eDSPermissionError")

// A Directory is the directory services a Mac's accounts are in, as dscl
// reaches them.
type Directory struct {
	// Run runs dscl with args and returns what it printed on its standard
	// output. Its error wraps ErrNotFound for a record that isn't there.
	Run func(ctx context.Context, args ...string) ([]byte, error)
}

// Local is the Mac's own directory, reached through /usr/bin/dscl. Only
// root may change it.
var Local = Directory{Run: dscl}

// dscl runs /usr/bin/dscl with args.
func dscl(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/dscl", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, dsclError(args, err, stderr.String())
	}
	return out, nil
}

// dsclError is the error of dscl run with args, which failed with err,
// printing stderr: ErrNotFound for a record that isn't there, and
// ErrPermission for a change macOS refused, as dscl names them.
func dsclError(args []string, err error, stderr string) error {
	cmd, msg := strings.Join(args, " "), strings.TrimSpace(stderr)
	switch {
	case strings.Contains(msg, "eDSRecordNotFound"):
		return fmt.Errorf("dscl %s: %w", cmd, ErrNotFound)
	case strings.Contains(msg, "eDSPermissionError"):
		return fmt.Errorf("dscl %s: %w: %w: %s", cmd, err, ErrPermission, msg)
	}
	return fmt.Errorf("dscl %s: %w: %s", cmd, err, msg)
}

// Read returns the local directory's record of kind named name, and
// false if there's none. It fails unless the record it read is that
// name's.
func (d Directory) Read(ctx context.Context, kind Kind, name string) (Record, bool, error) {
	if err := checkName(name); err != nil {
		return nil, false, err
	}
	out, err := d.Run(ctx, "-plist", ".", "-read", "/"+string(kind)+"/"+name)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	r, err := parseRecord(out)
	if err != nil {
		return nil, false, fmt.Errorf("the record of %s/%s: %w", kind, name, err)
	}
	if !slices.Contains(r["RecordName"], name) {
		return nil, false, fmt.Errorf("reading %s/%s read a record named %q", kind, name, r["RecordName"])
	}
	return r, true, nil
}

// All returns every record of kind that node holds (".", the local
// directory, or "/Search", every directory the search policy holds),
// with the attributes named, and RecordName. It fails unless the listing
// holds a record only the whole of it would: root's, for users and
// groups alike, so that a listing cut short can't pass for fewer records.
func (d Directory) All(ctx context.Context, node string, kind Kind, attrs ...string) ([]Record, error) {
	args := append([]string{"-plist", node, "-readall", "/" + string(kind), "RecordName"}, attrs...)
	out, err := d.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	rs, err := parseRecords(out)
	if err != nil {
		return nil, fmt.Errorf("the %s in %s: %w", kind, node, err)
	}
	root := false
	for _, r := range rs {
		if len(r["RecordName"]) == 0 {
			return nil, fmt.Errorf("the %s in %s: a record without a name", kind, node)
		}
		root = root || slices.Contains(r["RecordName"], "root") || slices.Contains(r["RecordName"], "wheel")
	}
	if !root {
		return nil, fmt.Errorf("the %s in %s don't include root's or wheel's record, so they can't have been read whole", kind, node)
	}
	return rs, nil
}

// set writes value as the record's attribute, replacing what it held,
// and makes the record if it isn't there.
func (d Directory) set(ctx context.Context, kind Kind, name, attr, value string) error {
	_, err := d.Run(ctx, ".", "-create", "/"+string(kind)+"/"+name, attr, value)
	return err
}

// unset deletes the record's attribute.
func (d Directory) unset(ctx context.Context, kind Kind, name, attr string) error {
	_, err := d.Run(ctx, ".", "-delete", "/"+string(kind)+"/"+name, attr)
	return err
}

// remove deletes the local directory's record of kind named name, whole.
func (d Directory) remove(ctx context.Context, kind Kind, name string) error {
	_, err := d.Run(ctx, ".", "-delete", "/"+string(kind)+"/"+name)
	return err
}

// checkName refuses a name dscl could read as something else: only
// letters, digits, and ._- after the first character, at most 64 bytes.
func checkName(name string) error {
	ok := name != "" && len(name) <= 64 && name[0] != '-' && name[0] != '.'
	for _, r := range name {
		ok = ok && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r))
	}
	if !ok {
		return fmt.Errorf("%q isn't a role account's name", name)
	}
	return nil
}

// id reads an attribute that holds one ID, and false if it doesn't hold
// exactly one decimal number.
func id(r Record, attr string) (int, bool) {
	v, ok := r.One(attr)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != v {
		return 0, false
	}
	return n, true
}

package slot

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// taskUsers returns the accounts a scheduled task's definition names as
// its principals' users, each as the file has it: a SID, a name, or a
// domain and a name. Windows keeps each task's definition as an XML
// file in System32\Tasks, in UTF-16 with a byte-order mark as Task
// Scheduler writes it, or in UTF-8. A principal that names a group, not
// a user, isn't one. A file that isn't a task's definition is an error:
// a task it can't read can't pass for one of nobody's.
func taskUsers(b []byte) ([]string, error) {
	text, err := utf8Text(b)
	if err != nil {
		return nil, err
	}
	d := xml.NewDecoder(bytes.NewReader(text))
	// The text is UTF-8 now, whatever its declaration says.
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	var task struct {
		XMLName    xml.Name
		Principals struct {
			Principal []struct {
				UserID string `xml:"UserId"`
			}
		}
	}
	if err := d.Decode(&task); err != nil {
		return nil, fmt.Errorf("isn't a task's definition: %w", err)
	}
	if task.XMLName.Local != "Task" {
		return nil, fmt.Errorf("isn't a task's definition: it's a %s", task.XMLName.Local)
	}
	var users []string
	for _, p := range task.Principals.Principal {
		if p.UserID != "" {
			users = append(users, p.UserID)
		}
	}
	return users, nil
}

// splitTask splits a task's path, such as \folder\name, into its folder
// with its trailing backslash, \folder\, and its name, name, as Task
// Scheduler's PowerShell takes them.
func splitTask(path string) (folder, name string) {
	i := strings.LastIndex(path, `\`)
	return path[:i+1], path[i+1:]
}

// utf8Text returns b as UTF-8: as it is, without a UTF-8 byte-order
// mark, or decoded from UTF-16 if it starts with UTF-16's mark.
func utf8Text(b []byte) ([]byte, error) {
	var big bool
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return b[3:], nil
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		big = true
	default:
		return b, nil
	}
	b = b[2:]
	if len(b)%2 != 0 {
		return nil, errors.New("isn't whole UTF-16")
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		if big {
			units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			units[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	return []byte(string(utf16.Decode(units))), nil
}

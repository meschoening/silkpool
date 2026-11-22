package slot

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/meschoening/silkpool/internal/secret"
)

// runnerName is what a runner's name may be: ASCII letters, digits,
// hyphens, underscores and dots, safe in a unit's name and a path.
var runnerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func checkRunner(runner string) error {
	if !runnerName.MatchString(runner) {
		return fmt.Errorf("%q isn't a runner name", runner)
	}
	return nil
}

// runnerFile is the file in a slot's folder that names the runner Start
// last started there, until the slot's reset has copied its logs.
const runnerFile = "runner-name"

// recordedRunner returns the runner slot n's record names, or "" if
// there's none.
func (s *Slots) recordedRunner(n int) string {
	b, err := os.ReadFile(filepath.Join(s.Folder(n), runnerFile))
	if runner := strings.TrimSpace(string(b)); err == nil && checkRunner(runner) == nil {
		return runner
	}
	return ""
}

// writeRecord writes value to a record file at path, readable by every
// account, through a new file renamed into place, so that a crash leaves
// the old record or the new one.
func writeRecord(path, value string) error {
	tmp := path + ".new"
	err := os.WriteFile(tmp, []byte(value+"\n"), 0o644)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// detail is err as a slot's failure detail: a sentence, with any secret
// in the text the commands it ran wrote scrubbed.
func detail(err error) string {
	d := secret.Scrub(err.Error())
	r, size := utf8.DecodeRuneInString(d)
	return string(unicode.ToUpper(r)) + d[size:]
}

// names returns the names in dir, or none if it doesn't exist.
func names(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	var list []string
	for _, e := range entries {
		list = append(list, e.Name())
	}
	return list, err
}

// recordedRunners returns the runners the slots' records name: those
// started and not yet reset.
func (s *Slots) recordedRunners() ([]string, error) {
	recorded, err := s.Recorded()
	if err != nil {
		return nil, err
	}
	var runners []string
	for _, n := range slices.Sorted(maps.Keys(recorded)) {
		runners = append(runners, recorded[n])
	}
	return runners, nil
}

// Recorded returns, by slot, the runner each slot's record names: the
// one Start last started there, until the slot's reset is done. A
// restarted agent reads what the dead ones' logs say, and watches the
// live ones again. With no slots' folder, there are none.
func (s *Slots) Recorded() (map[int]string, error) {
	slots, err := names(filepath.Join(s.Root, "slots"))
	if err != nil {
		return nil, err
	}
	recorded := map[int]string{}
	for _, name := range slots {
		if n, err := strconv.Atoi(name); err == nil && n > 0 {
			if runner := s.recordedRunner(n); runner != "" {
				recorded[n] = runner
			}
		}
	}
	return recorded, nil
}

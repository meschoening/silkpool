package slot

import (
	"fmt"
	"os"
	"path/filepath"
)

// folders makes the folders above slot n's runner root: Root/slots and
// the slot's folder, which root owns, open to every account so that the
// service account reaches the runner root. Root must be open to every
// account already.
func (s *Slots) folders(n int) error {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(s.Root); err != nil || fi.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("the service account can't reach the slots' folders through %s (%v): it must be open to every account", s.Root, err)
	}
	for _, dir := range []string{filepath.Dir(s.Folder(n)), s.Folder(n)} {
		if err := ownedDir(dir, 0, 0, 0o755); err != nil {
			return err
		}
	}
	return nil
}

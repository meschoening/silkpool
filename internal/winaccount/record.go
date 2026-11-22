package winaccount

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadRecord reads the SID recorded at path, or "" if there's no record.
func ReadRecord(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sid := strings.TrimSpace(string(b))
	if !strings.HasPrefix(sid, "S-1-") {
		return "", fmt.Errorf("%s doesn't hold a SID", path)
	}
	return sid, nil
}

// WriteRecord records sid at path, whole or not at all, in a folder the
// caller keeps for its own state.
func WriteRecord(path, sid string) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = f.WriteString(sid + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// RecordFile is the file in the agent's state folder that records the
// service account's SID.
const RecordFile = "service-account"

// Keep ensures the account spec names, as Ensure does, with the SID
// recorded at path, and records the SID Ensure returns if it's another.
func Keep(m Machine, spec Spec, path string) error {
	recorded, err := ReadRecord(path)
	if err != nil {
		return fmt.Errorf("can't read the record of the account %s: %w", spec.Name, err)
	}
	sid, err := Ensure(m, spec, recorded)
	if err != nil {
		return err
	}
	if sid != recorded {
		if err := WriteRecord(path, sid); err != nil {
			return fmt.Errorf("can't record the account %s: %w", spec.Name, err)
		}
	}
	return nil
}

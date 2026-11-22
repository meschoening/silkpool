package winslot

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Program is a slot host copy's program name, in its version's folder.
const Program = "silkpool.exe"

// Copies is the folder of the slot hosts' copies of silkpool.exe, one
// folder per version, C:\ProgramData\Silkpool\slot-host\<version>\ on
// an installed node (ADR 0006). Slot services never run the installed
// program, which Windows couldn't replace while a slot host runs from
// it. Tests use a folder of their own.
type Copies struct{ Dir string }

// Path is the copy of version's program.
func (c Copies) Path(version string) string {
	return filepath.Join(c.Dir, version, Program)
}

// Make copies program, the agent's own, as version's copy, and returns
// its path. A copy already there with the same contents is kept, since
// a slot host may run from it. One with other contents fails Make,
// which never replaces it: a version names one build.
func (c Copies) Make(version, program string) (string, error) {
	if err := checkVersion(version); err != nil {
		return "", err
	}
	want, err := digest(program)
	if err != nil {
		return "", fmt.Errorf("reading the program to copy: %w", err)
	}
	path := c.Path(version)
	switch got, err := digest(path); {
	case err == nil && bytes.Equal(got, want):
		return path, nil
	case err == nil:
		return "", fmt.Errorf("the slot host's copy for %s, %s, isn't this program", version, path)
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("reading the slot host's copy for %s: %w", version, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// The copy appears whole or not at all.
	tmp := path + ".new"
	if err := copyFile(program, tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("copying the program for the slot hosts: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("copying the program for the slot hosts: %w", err)
	}
	return path, nil
}

// Prune deletes every version's copy but keep's and those whose program
// is in use, as a slot service's program, and returns the versions it
// deleted. A copy Windows won't delete yet, because a slot host still
// runs from it, is left for a later Prune, and named in the error.
func (c Copies) Prune(keep string, inUse []string) ([]string, error) {
	entries, err := os.ReadDir(c.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var deleted []string
	var errs []error
	for _, e := range entries {
		v := e.Name()
		if !e.IsDir() || v == keep || slices.ContainsFunc(inUse, func(p string) bool { return samePath(p, c.Path(v)) }) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(c.Dir, v)); err != nil {
			errs = append(errs, fmt.Errorf("deleting the slot host's copy for %s: %w", v, err))
			continue
		}
		deleted = append(deleted, v)
	}
	return deleted, errors.Join(errs...)
}

// samePath reports whether a and b name the same path, as Windows
// compares them: ignoring case, and either separator.
func samePath(a, b string) bool {
	norm := func(p string) string { return strings.ReplaceAll(filepath.Clean(p), `/`, `\`) }
	return strings.EqualFold(norm(a), norm(b))
}

// checkVersion refuses a version that isn't one folder's name.
func checkVersion(v string) error {
	if v == "" || v == "." || v == ".." || strings.ContainsAny(v, `/\:`) || !filepath.IsLocal(v) {
		return fmt.Errorf("%q isn't a version the slot hosts' copies can be named by", v)
	}
	return nil
}

// digest is the SHA-256 of the file at path.
func digest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// copyFile copies src to a new file dst, and syncs it.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

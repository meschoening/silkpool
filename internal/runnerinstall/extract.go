package runnerinstall

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// extract extracts the tar.gz package at pkg into the folder dir. It
// refuses an entry that would land outside dir, and a link that points
// outside it, even through other links.
//
// Every file ends up readable by everyone and writable by its owner
// only, executable if the package says so, and owned by the agent,
// whoever the package names. Folders are 0755.
func extract(pkg, dir string) error {
	f, err := os.Open(pkg)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return damaged(err)
	}
	// The root also keeps every write inside dir, whatever links the
	// package has made by then.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	var symlinks []*tar.Header
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return damaged(err)
		}
		name := path.Clean(h.Name)
		switch {
		case name == ".":
			continue // the package's own folder
		case !filepath.IsLocal(name):
			return fmt.Errorf("its package holds %q, outside its folder", h.Name)
		case h.Typeflag == tar.TypeSymlink:
			symlinks = append(symlinks, h)
		}
		if err := extractEntry(root, tr, h, name); err != nil {
			return failure(err)
		}
	}
	// Now that every link is there, each must lead somewhere inside, or
	// nowhere at all.
	for _, h := range symlinks {
		if _, err := root.Stat(path.Clean(h.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return outside(h)
		}
	}
	return nil
}

func extractEntry(root *os.Root, tr *tar.Reader, h *tar.Header, name string) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return entryFailed(h, err)
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(name, 0o755); err != nil {
			return entryFailed(h, err)
		}
		return root.Chmod(name, 0o755)
	case tar.TypeReg:
		perm := fs.FileMode(0o644)
		if h.Mode&0o100 != 0 {
			perm = 0o755
		}
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return entryFailed(h, err)
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return entryFailed(h, err)
		}
		return root.Chmod(name, perm)
	case tar.TypeSymlink:
		if path.IsAbs(h.Linkname) || !filepath.IsLocal(path.Join(path.Dir(name), h.Linkname)) {
			return outside(h)
		}
		return entryFailed(h, root.Symlink(h.Linkname, name))
	case tar.TypeLink:
		if !filepath.IsLocal(path.Clean(h.Linkname)) {
			return outside(h)
		}
		// Only to a file: a hardlink to a symlink would be a symlink
		// somewhere else, where its target can lead outside.
		if info, err := root.Lstat(path.Clean(h.Linkname)); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("its package holds the hardlink %q -> %q, which isn't to a file in it", h.Name, h.Linkname)
		}
		return entryFailed(h, root.Link(path.Clean(h.Linkname), name))
	case tar.TypeXGlobalHeader:
		return nil
	}
	return fmt.Errorf("its package holds %q, which isn't a file, a folder or a link", h.Name)
}

// outside describes the link h, which points outside the package.
func outside(h *tar.Header) error {
	return fmt.Errorf("its package holds the link %q -> %q, which points outside its folder", h.Name, h.Linkname)
}

// entryFailed describes err, which extracting h met, or returns nil.
func entryFailed(h *tar.Header, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("couldn't extract %q from its package: %w", h.Name, err)
}

func damaged(err error) error { return fmt.Errorf("its package is damaged: %w", err) }

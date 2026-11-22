package runnerinstall

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

// unzip extracts the zip package at pkg into the folder dir. It refuses
// an entry whose name isn't a path inside dir as Windows reads it
// (filepath.IsLocal): one that leads out, through a slash or a
// backslash, is absolute, holds a colon, which names a drive or a
// stream, or names a device, such as NUL. It refuses any entry that
// isn't a file or a folder: a zip can hold symlinks, and the Windows
// packages have none. Folders a file's name implies are made, since the
// packages don't list them.
//
// Each file reaches the disk before the next is extracted.
func unzip(pkg, dir string) error {
	z, err := zip.OpenReader(pkg)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return damaged(err)
	}
	defer z.Close()
	// The root also keeps every write inside dir.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range z.File {
		name := path.Clean(f.Name)
		switch {
		case name == ".":
			continue // the package's own folder
		case !filepath.IsLocal(filepath.FromSlash(name)):
			return notInside(f)
		case f.Mode().IsDir():
			if err := root.MkdirAll(name, 0o755); err != nil {
				return entryFailed(f.Name, err)
			}
		case f.Mode().IsRegular():
			if err := unzipFile(root, f, name); err != nil {
				return entryFailed(f.Name, err)
			}
		default:
			return fmt.Errorf("its package holds %q, which isn't a file or a folder", f.Name)
		}
	}
	return nil
}

// notInside describes the entry f, whose name isn't a path inside the
// package's folder.
func notInside(f *zip.File) error {
	return fmt.Errorf("its package holds %q, which isn't a path inside its folder", f.Name)
}

// unzipFile extracts the file f as name in root.
func unzipFile(root *os.Root, f *zip.File, name string) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	if err == nil {
		err = w.Sync()
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

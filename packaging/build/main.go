// Command build builds what a Silkpool release publishes from this
// repository on Linux hosts, for the release wizard: the node's and the
// Hub's .deb and .rpm packages for linux/amd64 and linux/arm64, and
// install.sh, all stamped with the release's version, into one folder.
// Run it from the repository's root:
//
//	go run ./packaging/build -version 1.2.3 -out dist
//
// It names each package as its format does, such as
// silkpool_1.2.3_amd64.deb or silkpool-hub-1.2.3-1.aarch64.rpm, and
// prints each file it writes. It writes no SHA256SUMS: the release's
// covers every OS's files, so the wizard writes it last.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/meschoening/silkpool/packaging"
	"github.com/meschoening/silkpool/packaging/linux"
)

func main() {
	version := flag.String("version", "", "the release's version, such as 1.2.3, without the v")
	out := flag.String("out", "dist", "the folder to write into")
	flag.Parse()
	if flag.NArg() > 0 || *version == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := build(ctx, ".", *version, *out, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
}

// build builds the release at version from the module at root into out,
// and prints each file it writes.
func build(ctx context.Context, root, version, out string, stdout io.Writer) error {
	script, err := packaging.InstallScript(version)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	bins, err := os.MkdirTemp("", "silkpool-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bins)
	for _, arch := range linux.Arches {
		for _, b := range linux.Binaries {
			bin := filepath.Join(bins, arch, b.Package)
			if err := b.Build(ctx, root, arch, version, bin); err != nil {
				return err
			}
			for _, format := range linux.Formats {
				p := linux.Package{Name: b.Package, Format: format, Arch: arch, Version: version}
				if err := write(filepath.Join(out, p.FileName()), stdout, func(w io.Writer) error { return p.Write(w, bin) }); err != nil {
					return err
				}
			}
		}
	}
	return write(filepath.Join(out, "install.sh"), stdout, func(w io.Writer) error {
		_, err := w.Write(script)
		return err
	})
}

// write writes a file with f, and prints its path.
func write(path string, stdout io.Writer, f func(io.Writer) error) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f(file); err != nil {
		file.Close()
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	fmt.Fprintln(stdout, path)
	return nil
}

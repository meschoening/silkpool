// Command build builds what a Silkpool release publishes from this
// repository on any host, but for the macOS .pkg, as the release
// workflow and the release wizard's check of the export run it:
// the node's and the Hub's .deb and .rpm packages for linux/amd64 and
// linux/arm64, and install.sh, then the Windows node's zips for
// windows/amd64 and windows/arm64, and install.ps1, all stamped with the
// release's version, into one folder. Run it from the repository's
// root:
//
//	go run ./packaging/build -version 1.2.3 -out dist
//
// It names each package as its format does, such as
// silkpool_1.2.3_amd64.deb, silkpool-hub-1.2.3-1.aarch64.rpm or
// silkpool_1.2.3_windows_amd64.zip, and prints each file it writes. It
// writes no SHA256SUMS: the release's covers every OS's files, the
// .pkg that the workflow's macOS job builds on a Mac among them, so the
// workflow writes it last.
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
	"github.com/meschoening/silkpool/packaging/windows"
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
	if err := build(ctx, builders(), ".", *version, *out, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
}

// A builder builds the programs the release's files hold. The tests
// stand in for it, since building every program takes minutes.
type builder struct {
	// linux builds b for linux/arch at version into the file bin.
	linux func(ctx context.Context, b linux.Binary, root, arch, version, bin string) error
	// windows writes the Windows zip for arch at version to w.
	windows func(ctx context.Context, root, version, arch string, w io.Writer) error
}

// builders build the real programs.
func builders() builder {
	return builder{
		linux: func(ctx context.Context, b linux.Binary, root, arch, version, bin string) error {
			return b.Build(ctx, root, arch, version, bin)
		},
		windows: windows.Build,
	}
}

// build builds the release at version from the module at root into out,
// and prints each file it writes.
func build(ctx context.Context, b builder, root, version, out string, stdout io.Writer) error {
	script, err := packaging.InstallScript(version)
	if err != nil {
		return err
	}
	ps1, err := packaging.InstallPS1(version)
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
		for _, bin := range linux.Binaries {
			file := filepath.Join(bins, arch, bin.Package)
			if err := b.linux(ctx, bin, root, arch, version, file); err != nil {
				return err
			}
			for _, format := range linux.Formats {
				p := linux.Package{Name: bin.Package, Format: format, Arch: arch, Version: version}
				if err := write(filepath.Join(out, p.FileName()), stdout, func(w io.Writer) error { return p.Write(w, file) }); err != nil {
					return err
				}
			}
		}
	}
	if err := write(filepath.Join(out, "install.sh"), stdout, func(w io.Writer) error {
		_, err := w.Write(script)
		return err
	}); err != nil {
		return err
	}
	for _, arch := range windows.Arches {
		if err := write(filepath.Join(out, windows.FileName(version, arch)), stdout, func(w io.Writer) error {
			return b.windows(ctx, root, version, arch, w)
		}); err != nil {
			return err
		}
	}
	return write(filepath.Join(out, "install.ps1"), stdout, func(w io.Writer) error {
		_, err := w.Write(ps1)
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

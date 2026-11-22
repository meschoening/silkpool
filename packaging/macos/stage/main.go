// Command stage stages the macOS .pkg's payload and install scripts for
// the release workflow's macOS job, which then signs the code and builds,
// signs and notarizes the .pkg (ADR 0016). Run it on a Mac with Xcode,
// from the repository's root:
//
//	go run ./packaging/macos/stage -version 1.2.3 -out pkg
//
// It builds cmd/silkpool and the tray app for darwin/arm64, stamped with
// the version, and writes pkg/root, what the .pkg installs as ADR 0013
// lays it out, and pkg/scripts, its preinstall and postinstall. The out
// folder must not exist yet.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/meschoening/silkpool/packaging/macos"
)

func main() {
	version := flag.String("version", "", "the release's version, such as 1.2.3, without the v")
	out := flag.String("out", "pkg", "the folder to write, which must not exist yet")
	flag.Parse()
	if flag.NArg() > 0 || *version == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := stage(ctx, ".", *version, *out); err != nil {
		fmt.Fprintln(os.Stderr, "stage:", err)
		os.Exit(1)
	}
}

func stage(ctx context.Context, root, version, out string) error {
	bins, err := os.MkdirTemp("", "silkpool-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bins)
	in := macos.Inputs{Version: version, Silkpool: filepath.Join(bins, "silkpool"), Tray: filepath.Join(bins, "silkpool-tray")}
	if err := macos.BuildSilkpool(ctx, root, version, in.Silkpool); err != nil {
		return err
	}
	if err := macos.BuildTray(ctx, root, version, in.Tray); err != nil {
		return err
	}
	if err := macos.Stage(out, in); err != nil {
		return err
	}
	fmt.Println(filepath.Join(out, macos.RootDir))
	fmt.Println(filepath.Join(out, macos.ScriptsDir))
	return nil
}

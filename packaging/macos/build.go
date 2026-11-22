package macos

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// versionVariable is the variable a build's version is stamped into.
const versionVariable = "github.com/meschoening/silkpool/internal/version.Version"

// The commands the payload's binaries are built from, in the module at
// a checkout's root. The tray app's is a module of its own (ADR 0009).
const (
	silkpoolCommand = "./cmd/silkpool"
	trayModule      = "cmd/silkpool-tray"
)

// BuildSilkpool builds cmd/silkpool from the module at root for
// darwin/arm64, without cgo, stamped with version, into the file out.
// It cross-compiles from any host.
func BuildSilkpool(ctx context.Context, root, version, out string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("%q isn't a release's version, such as 1.2.3 or 1.2.3-rc.1", version)
	}
	return goBuild(ctx, root, []string{"CGO_ENABLED=0"}, "-ldflags", "-X "+versionVariable+"="+version, "-o", out, silkpoolCommand)
}

// BuildTray builds the tray app from its module at root for
// darwin/arm64, stamped with version, into the file out, as ADR 0009
// has it: with cgo, which Wails needs, -tags private_mac_apis, and
// SDKROOT set to Xcode's macOS SDK. It needs a Mac with Xcode.
func BuildTray(ctx context.Context, root, version, out string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("%q isn't a release's version, such as 1.2.3 or 1.2.3-rc.1", version)
	}
	sdk, err := exec.CommandContext(ctx, "xcrun", "--sdk", "macosx", "--show-sdk-path").Output()
	if err != nil {
		return fmt.Errorf("finding Xcode's macOS SDK, which the tray app builds against: %w", err)
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return err
	}
	env := []string{"CGO_ENABLED=1", "SDKROOT=" + strings.TrimSpace(string(sdk))}
	return goBuild(ctx, filepath.Join(root, trayModule), env, "-tags", "private_mac_apis", "-ldflags", "-X "+versionVariable+"="+version, "-o", out, ".")
}

// goBuild runs go build -trimpath with args in dir, for darwin/arm64,
// with env added.
func goBuild(ctx context.Context, dir string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "go", append([]string{"build", "-trimpath"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GOOS=darwin", "GOARCH="+Arch), env...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s in %s: %w\n%s", strings.Join(args, " "), dir, err, output.Bytes())
	}
	return nil
}

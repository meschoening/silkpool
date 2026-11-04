package linux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// versionVariable is the variable a build's version is stamped into.
const versionVariable = "github.com/meschoening/silkpool/internal/version.Version"

// Build builds b from the module at root, with the go command, for
// linux/arch, without cgo, and stamped with version, into the file out.
func (b Binary) Build(ctx context.Context, root, arch, version, out string) error {
	if err := CheckVersion(version); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-X "+versionVariable+"="+version, "-o", out, b.Command)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building %s for linux/%s: %w\n%s", b.Command, arch, err, output)
	}
	return nil
}

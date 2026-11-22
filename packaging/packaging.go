// Package packaging holds what a release publishes for installing
// Silkpool beside its packages: install.sh, and install.ps1 for Windows,
// which each release stamps with its own version, from "Define how each
// binary is packaged, installed, upgraded and signed per OS", recorded
// as ADR 0013. packaging/linux builds the Linux packages,
// packaging/macos lays out the macOS .pkg, and packaging/windows builds
// the Windows zips.
package packaging

import (
	_ "embed"
	"strings"

	"github.com/meschoening/silkpool/packaging/linux"
	"github.com/meschoening/silkpool/packaging/macos"
	"github.com/meschoening/silkpool/packaging/windows"
)

//go:embed install.sh
var installScript string

//go:embed install.ps1
var installPS1 string

// InstallScript is install.sh for the release at version: it downloads
// and installs that release's packages, or its .pkg on macOS.
func InstallScript(version string) ([]byte, error) {
	if err := linux.CheckVersion(version); err != nil {
		return nil, err
	}
	stamp := strings.NewReplacer(
		"@VERSION@", version,
		"@PKG@", macos.FileName(version),
	)
	return []byte(stamp.Replace(installScript)), nil
}

// InstallPS1 is install.ps1 for the release at version: it downloads
// that release's zip for the machine and installs it with silkpool
// install.
func InstallPS1(version string) ([]byte, error) {
	if err := linux.CheckVersion(version); err != nil {
		return nil, err
	}
	stamp := strings.NewReplacer(
		"@VERSION@", version,
		"@ZIP_AMD64@", windows.FileName(version, "amd64"),
		"@ZIP_ARM64@", windows.FileName(version, "arm64"),
	)
	return []byte(stamp.Replace(installPS1)), nil
}

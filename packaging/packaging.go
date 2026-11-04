// Package packaging holds what a release publishes for installing
// Silkpool beside its packages: install.sh, which each release stamps
// with its own version, from "Define how each binary is packaged,
// installed, upgraded and signed per OS",
// recorded as ADR 0013. packaging/linux builds the Linux packages.
package packaging

import (
	_ "embed"
	"strings"

	"github.com/meschoening/silkpool/packaging/linux"
)

//go:embed install.sh
var installScript string

// InstallScript is install.sh for the release at version: it downloads
// and installs that release's packages.
func InstallScript(version string) ([]byte, error) {
	if err := linux.CheckVersion(version); err != nil {
		return nil, err
	}
	stamp := strings.NewReplacer(
		"@VERSION@", version,
		"@DEB_VERSION@", linux.DebVersion(version),
		"@RPM_VERSION@", linux.RPMVersion(version),
	)
	return []byte(stamp.Replace(installScript)), nil
}

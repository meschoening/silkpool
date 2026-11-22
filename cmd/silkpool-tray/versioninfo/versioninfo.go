// Package versioninfo makes the version resource silkpool-tray.exe
// carries, as an object file the Go linker links into the Windows
// builds: rsrc_windows_<arch>.syso, beside the app's main package.
// Windows names the tray icon in Settings by the exe's file description,
// and by its file name without one, as "Test a Wails v3 tray app on
// macOS and Windows" found.
//
// The files are generated from the source's version and kept in the
// repository, so every build carries them. A test fails while they don't
// match what this package makes: rewrite them with
// go test ./versioninfo -update.
//
// It doesn't import Wails, so its tests run in the default lane.
package versioninfo

import (
	"bytes"
	"fmt"

	"github.com/tc-hib/winres"
	winversion "github.com/tc-hib/winres/version"

	"github.com/meschoening/silkpool/internal/version"
)

// Arches are the Windows architectures the tray app builds for.
var Arches = []string{"amd64", "arm64"}

// File is the name of goarch's object file.
func File(goarch string) string { return "rsrc_windows_" + goarch + ".syso" }

// The version resource's words. The file description is the name
// Windows gives the tray icon.
const (
	Description = "Silkpool"
	Product     = "Silkpool"
	exe         = "silkpool-tray.exe"
)

// Syso returns goarch's object file, holding the version resource of
// this source's version.
func Syso(goarch string) ([]byte, error) {
	arch, ok := map[string]winres.Arch{"amd64": winres.ArchAMD64, "arm64": winres.ArchARM64}[goarch]
	if !ok {
		return nil, fmt.Errorf("the tray app doesn't build for windows/%s", goarch)
	}
	var vi winversion.Info
	for key, value := range map[string]string{
		winversion.FileDescription:  Description,
		winversion.ProductName:      Product,
		winversion.InternalName:     "silkpool-tray",
		winversion.OriginalFilename: exe,
		winversion.FileVersion:      version.Version,
		winversion.ProductVersion:   version.Version,
	} {
		if err := vi.Set(winversion.LangDefault, key, value); err != nil {
			return nil, err
		}
	}
	// After the strings, so that they set the fixed part's numbers alone.
	vi.SetFileVersion(version.Version)
	vi.SetProductVersion(version.Version)
	var rs winres.ResourceSet
	rs.SetVersionInfo(vi)
	var b bytes.Buffer
	if err := rs.WriteObject(&b, arch); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

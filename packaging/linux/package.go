package linux

import (
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goreleaser/nfpm/v2"
	"github.com/goreleaser/nfpm/v2/deb"
	nfpmfiles "github.com/goreleaser/nfpm/v2/files"
	"github.com/goreleaser/nfpm/v2/rpm"
)

// The packages' maintainer scripts, one per package and hook, by dpkg's
// names. Each one also takes rpm's calls, which pass a count instead of
// dpkg's words.
//
//go:embed silkpool.preinst silkpool.postinst silkpool.prerm silkpool.postrm
//go:embed silkpool-hub.preinst silkpool-hub.postinst silkpool-hub.prerm silkpool-hub.postrm
var scripts embed.FS

// Formats are the formats each package is built in, and Arches the
// architectures, by their GOARCH names.
var (
	Formats = []string{"deb", "rpm"}
	Arches  = []string{"amd64", "arm64"}
)

// A Package is one package file: the node's or the Hub's package, in a
// format, for an architecture, at a release's version.
type Package struct {
	Name    string // Node or Hub
	Format  string // "deb" or "rpm"
	Arch    string // "amd64" or "arm64"
	Version string // such as 1.2.3 or 1.2.3-rc.1, which CheckVersion takes
}

// versionPattern is semver without a v, without build metadata, and with
// a prerelease of dot-separated letters and digits, which both formats
// carry after a "~", so that it sorts before its release.
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$`)

// CheckVersion returns an error unless version is one a release can
// carry, such as 1.2.3 or 1.2.3-rc.1.
func CheckVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("%q isn't a release's version, such as 1.2.3 or 1.2.3-rc.1", version)
	}
	return nil
}

// DebVersion and RPMVersion are version as the .deb and the .rpm carry
// it: 1.2.3-rc.1 becomes 1.2.3~rc.1, and the .rpm's release is always 1.
func DebVersion(version string) string { return strings.Replace(version, "-", "~", 1) }
func RPMVersion(version string) string { return DebVersion(version) + "-1" }

// packagers are nfpm's, by format.
var packagers = map[string]nfpm.Packager{"deb": deb.Default, "rpm": rpm.DefaultRPM}

// FileName is the package file's name by its format's convention, such
// as silkpool_1.2.3_amd64.deb or silkpool-1.2.3-1.x86_64.rpm. It panics
// for a format that isn't one of Formats.
func (p Package) FileName() string {
	packager, ok := packagers[p.Format]
	if !ok {
		panic(fmt.Sprintf("linux: %q isn't a package format", p.Format))
	}
	return packager.ConventionalFileName(nfpm.WithDefaults(&nfpm.Info{Name: p.Name, Arch: p.Arch, Version: p.Version}))
}

// Write writes the package to w. It holds the binary at the path binary,
// built for p.Arch and stamped with p.Version, beside the package's
// Files and its scripts, all root's.
func (p Package) Write(w io.Writer, binary string) error {
	if err := CheckVersion(p.Version); err != nil {
		return err
	}
	meta, ok := metadata[p.Name]
	if !ok {
		return fmt.Errorf("%q isn't a package", p.Name)
	}
	packager, ok := packagers[p.Format]
	if !ok {
		return fmt.Errorf("%q isn't a package format", p.Format)
	}
	dir, err := os.MkdirTemp("", "silkpool-package-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	var contents nfpmfiles.Contents
	add := func(src, dst string, mode os.FileMode) {
		contents = append(contents, &nfpmfiles.Content{Source: src, Destination: dst,
			FileInfo: &nfpmfiles.ContentFileInfo{Owner: "root", Group: "root", Mode: mode}})
	}
	for _, b := range Binaries {
		if b.Package == p.Name {
			add(binary, b.Path, 0o755)
		}
	}
	for _, f := range Files {
		if f.Package != p.Name {
			continue
		}
		src := filepath.Join(dir, f.Name)
		if err := os.WriteFile(src, f.Content(), 0o644); err != nil {
			return err
		}
		add(src, f.Path, f.Mode)
	}
	hooks, err := p.scripts(dir)
	if err != nil {
		return err
	}
	info := nfpm.WithDefaults(&nfpm.Info{
		Name:        p.Name,
		Arch:        p.Arch,
		Version:     p.Version,
		Section:     "admin",
		Priority:    "optional",
		Maintainer:  "Silkpool <" + homepage + ">",
		Vendor:      "Silkpool",
		Homepage:    homepage,
		Description: meta.summary + "\n" + meta.description,
		Overridables: nfpm.Overridables{
			Depends:  meta.depends[p.Format],
			Contents: contents,
			Scripts:  hooks,
			RPM:      nfpm.RPM{Summary: meta.summary},
		},
	})
	return packager.Package(info, w)
}

// homepage is where Silkpool's releases and operator docs are published.
const homepage = "https://github.com/meschoening/silkpool"

// metadata is what each package says about itself, and what it depends
// on in each format.
var metadata = map[string]struct {
	summary, description string
	depends              map[string][]string
}{
	Node: {
		summary:     "Silkpool's node: the agent service and the silkpool CLI",
		description: "Puts this machine on duty as self-hosted GitHub Actions runners for a Silkpool Hub's pool, and takes it off duty without interrupting builds.",
		// What GitHub's runner application needs and Ubuntu 26.04's
		// minimal install lacks (libicu78 there); the earlier ICU
		// versions are those of other Debian-based releases, which are
		// untested.
		depends: map[string][]string{
			"deb": {"ca-certificates", "libicu78 | libicu76 | libicu74 | libicu72", "libkrb5-3", "libgssapi-krb5-2"},
			"rpm": {"ca-certificates", "libicu", "krb5-libs"},
		},
	},
	Hub: {
		summary:     "Silkpool's Hub: its service and the silkpool-hub CLI",
		description: "Coordinates a pool of Silkpool nodes: it holds the pool's policy, shows each node's state, relays commands and brokers GitHub registration credentials.",
	},
}

// scripts writes p's maintainer scripts into dir, filled in for p, and
// returns where they are.
func (p Package) scripts(dir string) (nfpm.Scripts, error) {
	var sysusers string
	for _, f := range Files {
		if f.Package == p.Name && strings.HasPrefix(f.Path, "/usr/lib/sysusers.d/") {
			sysusers = strings.TrimSuffix(string(f.Content()), "\n")
		}
	}
	fill := strings.NewReplacer("@VERSION@", p.Version, "@DEB_VERSION@", DebVersion(p.Version),
		"@RPM_VERSION@", RPMVersion(p.Version), "@SYSUSERS@", sysusers)
	var paths [4]string
	for i, hook := range []string{"preinst", "postinst", "prerm", "postrm"} {
		body, err := scripts.ReadFile(p.Name + "." + hook)
		if err != nil {
			return nfpm.Scripts{}, err
		}
		paths[i] = filepath.Join(dir, hook)
		if err := os.WriteFile(paths[i], []byte(fill.Replace(string(body))), 0o755); err != nil {
			return nfpm.Scripts{}, err
		}
	}
	return nfpm.Scripts{PreInstall: paths[0], PostInstall: paths[1], PreRemove: paths[2], PostRemove: paths[3]}, nil
}

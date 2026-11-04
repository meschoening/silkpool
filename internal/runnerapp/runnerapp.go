// Package runnerapp is the pure core of the runner application's
// versions on the Hub: which version of GitHub's actions/runner each
// target runs, chosen from GitHub's answers, and when the Hub asks
// again. It also holds the platforms Silkpool fetches the runner
// application for, and the fixed template its packages' URLs follow,
// which the agent builds itself. The decision behind it is "Define how
// the agent installs, verifies and updates the runner application", recorded as
// ADR 0011.
//
// It's the Hub's half of that decision, beside package pool, which sends
// each change to the target's connected nodes, and package node, which
// installs it. Everything here is a function of (targets, event, now).
// The Hub applies an event for everything it learns from GitHub and its
// nodes, saves the Targets that come back, and only then carries out the
// effects. Nothing reads the clock, sleeps or does I/O.
package runnerapp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A Platform is an operating system and architecture the runner
// application is fetched for, named as GitHub names its packages.
type Platform string

// The platforms Silkpool supports, which match the gate's build targets.
// Intel Macs and 32-bit ARM Linux aren't among them.
const (
	LinuxX64   Platform = "linux-x64"
	LinuxARM64 Platform = "linux-arm64"
	WinX64     Platform = "win-x64"
	WinARM64   Platform = "win-arm64"
	OSXARM64   Platform = "osx-arm64"
)

// Platforms are the platforms Silkpool supports, in a fixed order.
var Platforms = []Platform{LinuxX64, LinuxARM64, WinX64, WinARM64, OSXARM64}

// A Version is a target's runner version: a release of the runner
// application, such as 2.338.2, and the SHA-256 of its package for each
// platform, in lower-case hex. The zero Version is none.
type Version struct {
	Version string
	SHA256  map[Platform]string
}

// Filename returns the name of the runner application's package of
// version for p.
func Filename(p Platform, version string) string {
	ext := "tar.gz"
	if strings.HasPrefix(string(p), "win-") {
		ext = "zip"
	}
	return fmt.Sprintf("actions-runner-%s-%s.%s", p, version, ext)
}

// URL returns where the agent downloads the runner application's
// package of version for p. The agent builds it itself and never takes
// a URL from the Hub, so a compromised Hub can choose only among real
// actions/runner releases.
func URL(p Platform, version string) string {
	return fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", version, Filename(p, version))
}

// A Download is one entry of the answer GitHub's downloads endpoint
// (GET /repos/{owner}/{repo}/actions/runners/downloads, or
// /orgs/{org}/...) gives for a target: its os, architecture, filename
// and sha256_checksum. Its download_url isn't kept, because the agent
// builds the URL itself.
type Download struct {
	OS, Arch string
	Filename string
	SHA256   string
}

// A Release is the latest release of actions/runner
// (GET /repos/actions/runner/releases/latest): its tag_name, such as
// v2.338.2, and its assets.
type Release struct {
	Tag    string
	Assets []Asset
}

// An Asset is one of a release's assets: its name, and its digest as
// GitHub gives it, "sha256:" followed by the hex digest.
type Asset struct{ Name, Digest string }

// served returns the version a downloads answer serves, and whether it
// serves one: every platform Silkpool supports, at one version, with a
// well-formed digest and the package name the agent will fetch. Other
// platforms are ignored. An empty answer serves none, and so does any
// that can't serve every node.
func served(list []Download) (Version, bool) {
	v := Version{SHA256: map[Platform]string{}}
	for _, d := range list {
		p := Platform(d.OS + "-" + d.Arch)
		if !supported(p) {
			continue
		}
		version, ok := versionOf(p, d.Filename)
		if !ok || v.Version != "" && version != v.Version || !hex64(d.SHA256) {
			return Version{}, false
		}
		v.Version, v.SHA256[p] = version, d.SHA256
	}
	return v, complete(v)
}

// latest returns the version a release carries, and whether it carries
// one: a well-formed tag, and an asset with a SHA-256 digest for every
// platform Silkpool supports.
func latest(r Release) (Version, bool) {
	version, ok := strings.CutPrefix(r.Tag, "v")
	if !ok || !Valid(version) {
		return Version{}, false
	}
	v := Version{Version: version, SHA256: map[Platform]string{}}
	for _, a := range r.Assets {
		for _, p := range Platforms {
			if sum, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && a.Name == Filename(p, version) && hex64(sum) {
				v.SHA256[p] = sum
			}
		}
	}
	return v, complete(v)
}

// complete reports whether v has a digest for every platform.
func complete(v Version) bool {
	for _, p := range Platforms {
		if v.SHA256[p] == "" {
			return false
		}
	}
	return v.Version != ""
}

func supported(p Platform) bool {
	for _, q := range Platforms {
		if p == q {
			return true
		}
	}
	return false
}

// versionOf returns the version in filename, if it's the name of p's
// package of that version.
func versionOf(p Platform, filename string) (string, bool) {
	rest, ok := strings.CutPrefix(filename, "actions-runner-"+string(p)+"-")
	if !ok {
		return "", false
	}
	for _, ext := range []string{".tar.gz", ".zip"} {
		if version, ok := strings.CutSuffix(rest, ext); ok && Valid(version) && Filename(p, version) == filename {
			return version, true
		}
	}
	return "", false
}

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	hexPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Valid reports whether version is a release version, such as 2.338.2.
func Valid(version string) bool { return versionPattern.MatchString(version) }

func hex64(s string) bool { return hexPattern.MatchString(s) }

// Compare compares two release versions, such as 2.338.2, number by
// number. It returns -1 if a is lower, 1 if it's higher, and 0 if
// they're equal. A version that isn't valid is lower than any that is.
func Compare(a, b string) int {
	pa, pb := parts(a), parts(b)
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

// parts returns version's three numbers, or -1s if it isn't valid.
func parts(version string) [3]int {
	m := versionPattern.FindStringSubmatch(version)
	if m == nil {
		return [3]int{-1, -1, -1}
	}
	var p [3]int
	for i := range p {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{-1, -1, -1}
		}
		p[i] = n
	}
	return p
}

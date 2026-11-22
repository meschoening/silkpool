// Package exposure holds what a Mac's exposure check and shared-places
// sweep decide, from "Define how the agent runs and launches slots on each
// OS", ADRs 0005 and 0017, and "Use one Silkpool service account per
// machine instead of per-slot accounts" (#214). Every slot runs its
// runners as the machine's one service account, so the check and the
// sweep are the machine's, not a slot's.
//
//   - The read check probes, as the account, what no job may see: what's
//     outside the OS folders at the top of the file system, each home in
//     /Users, each volume in /Volumes but macOS's own system volumes
//     (SystemVolume), and Silkpool's own folders (View).
//   - The survey walks the places on the Mac's writable volumes, as the
//     account, for the folders it can create entries in: the shared
//     places, which the sweep empties of what the account owns, and which
//     the check flags in the folders of the OS's software (Changes).
//   - Run is the helper that does both as the account, in a launchd job
//     the agent starts; the agent reads its Report.
//
// This file and the report's are portable, so the default lane tests them
// on Linux. Their paths are a Mac's, so they're joined with package path,
// never path/filepath, which would join them as Windows does there.
package exposure

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// OSFolders are a Mac's OS folders at the top of the file system, which
// the read check never probes: "Define how the agent runs and launches
// slots on each OS"'s, and every other entry a clean macOS 26 has there,
// on its sealed system volume or as a firmlink of /usr/share/firmlinks
// (ADR 0017). A stock entry missing here would fail every Mac closed.
// Users and Volumes hold what the check probes one by one.
var OSFolders = []string{
	".VolumeIcon.icns", ".file", ".nofollow", ".resolve", ".vol", "AppleInternal", "Applications", "Library",
	"System", "Users", "Volumes", "bin", "cores", "dev", "etc", "home", "nix", "opt", "pkg", "private", "sbin",
	"snap", "tmp", "usr", "var",
}

// sharedUsers are the entries in /Users that aren't anyone's home: the
// folder every account shares, and the file that names /Users in Finder.
var sharedUsers = []string{".localized", "Shared"}

// An Entry is a name in a folder, whether it's a symbolic link, and, in
// /Volumes, whether it's where one of macOS's own volumes that hold no
// account's files is mounted (SystemVolume).
type Entry struct {
	Name   string
	Link   bool
	System bool
}

// A View is what the agent reads, as root, of the folders whose entries
// the read check probes: the top of the file system, /Users and /Volumes.
type View struct {
	Top, Users, Volumes []Entry
}

// Probes returns the paths the read check probes, sorted: each entry at
// the top of the file system outside the OS folders, each home in /Users,
// each volume in /Volumes, which is a data volume however it's mounted,
// but macOS's own that hold no account's files, and private, Silkpool's
// own folders. A link counts for nothing: it leads to an OS folder, which
// every account may read, or to something probed on its own.
func (v View) Probes(private ...string) []string {
	paths := slices.Clone(private)
	add := func(dir string, entries []Entry, skip []string) {
		for _, e := range entries {
			if !e.Link && !e.System && !slices.Contains(skip, e.Name) {
				paths = append(paths, path.Join(dir, e.Name))
			}
		}
	}
	add("/", v.Top, OSFolders)
	add("/Users", v.Users, sharedUsers)
	add("/Volumes", v.Volumes, nil)
	slices.Sort(paths)
	return slices.Compact(paths)
}

// WalkRoots returns the folders the survey walks on a Mac whose
// /usr/share/firmlinks reads firmlinks. A firmlink leads from the sealed,
// read-only system volume to the data volume, the only one of the Mac's
// own a job can write to, so the survey walks each, but /Users, whose
// homes the read check probes one by one, and /Volumes, which holds
// data volumes, which are never walked. Instead it walks the folder of
// /Users every account shares, and /nix, which is a volume of its own
// where Nix is installed. It
// fails unless each line is a clean absolute path, a tab and the
// firmlink's target, and there's one at least.
func WalkRoots(firmlinks string) ([]string, error) {
	roots := []string{path.Join("/Users", "Shared"), "/nix"}
	found := false
	for line := range strings.Lines(firmlinks) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		link, target, ok := strings.Cut(line, "\t")
		if !ok || target == "" || !path.IsAbs(link) || path.Clean(link) != link {
			return nil, fmt.Errorf("/usr/share/firmlinks holds %q", line)
		}
		found = true
		if link != "/Users" && link != "/Volumes" {
			roots = append(roots, link)
		}
	}
	if !found {
		return nil, errors.New("/usr/share/firmlinks names no firmlink")
	}
	slices.Sort(roots)
	return slices.Compact(roots), nil
}

// ChangeRoots are the folders of the OS's software that a later job's
// tools come from: a folder in them the account can change fails the
// check (ADR 0005).
var ChangeRoots = []string{"/Applications", "/Library/Developer", "/nix", "/opt", "/usr"}

// under reports whether path is dir or inside it.
func under(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
}

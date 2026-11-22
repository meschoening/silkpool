// Package winexposure holds what a Windows machine's exposure check and
// shared-places sweep decide, and does them, from "Define how the agent
// runs and launches slots on each OS", ADRs 0005 and 0018, and "Use one
// Silkpool service account per machine instead of per-slot accounts"
// (#214). Every slot runs its runners as the machine's one service
// account, so the check and the sweep are the machine's, not a slot's.
//
//   - The read check looks, as the account, at what no job may see:
//     what's outside the OS folders at the top of the system drive, each
//     home in Users, what's at the top of each other fixed drive, and
//     Silkpool's own folders (View.Probes).
//   - The survey walks the system drive, and each other fixed drive's
//     root, for the folders the account can create entries in: the
//     shared places, which the sweep empties of what the account owns.
//     In the folders of the OS's software it guards each, denying the
//     account every write there (ADR 0019); one it can't, the check
//     flags (Hits).
//   - Survey, Look and Sweep do them, in the agent's own process, as
//     LocalSystem acting as the account through its S4U token (ADR 0018).
//
// This file is portable, so the default lane tests it on Linux. Its paths
// are Windows', joined with a backslash and compared ignoring case, as
// NTFS compares names, never with path/filepath, which would join them
// as Linux does there.
package winexposure

import (
	"fmt"
	"slices"
	"strings"

	"github.com/meschoening/silkpool/internal/secret"
)

// SystemFolders are the entries at the top of the system drive that the
// read check never looks at: the OS folders "Define how the agent runs
// and launches slots on each OS" names, the files Windows keeps there,
// and the folders a stock Windows 11 has there besides, each of which
// every account may read or none may (ADR 0018). A stock entry missing
// here would fail every Windows machine closed. Users holds the homes
// the check looks at one by one.
var SystemFolders = []string{
	"$Recycle.Bin", "$WinREAgent", "Documents and Settings", "PerfLogs", "Program Files",
	"Program Files (Arm)", "Program Files (x86)", "ProgramData", "Recovery", "System Volume Information",
	"Users", "Windows", "inetpub",
	"DumpStack.log", "DumpStack.log.tmp", "hiberfil.sys", "pagefile.sys", "swapfile.sys",
}

// DriveFolders are the entries at the top of another fixed drive that
// the read check never looks at: only the OS's own, with the page file,
// which Windows may keep on any fixed drive.
var DriveFolders = []string{"$Recycle.Bin", "System Volume Information", "pagefile.sys"}

// sharedUsers are the entries in Users that aren't anyone's home: the
// public profile and the default one, which count as OS folders, their
// junctions for older programs, and Users' own desktop.ini.
var sharedUsers = []string{"All Users", "Default", "Default User", "Public", "desktop.ini"}

// An Entry is a name in a folder, and whether it's a reparse point: a
// junction, a symbolic link, a mount point or any other.
type Entry struct {
	Name string
	Link bool
}

// A Drive is a fixed drive other than the system drive: its root, such
// as D:\, and the entries at its top.
type Drive struct {
	Root string
	Top  []Entry
}

// A View is what the agent reads, as LocalSystem, of the folders whose
// entries the read check looks at: the system drive's root, such as C:\,
// its top and its Users folder, and each other fixed drive. A network
// drive, a removable one or an optical one isn't the machine's own.
type View struct {
	System string
	Top    []Entry
	Users  []Entry
	Drives []Drive
}

// UsersFolder is the system drive's Users folder.
func (v View) UsersFolder() string { return Join(v.System, "Users") }

// Probes returns the paths the read check looks at, sorted: each entry
// at the top of the system drive outside the OS folders, each home in
// Users but own, the account's own profile, each entry at the top of
// another fixed drive but its OS folders, and private, Silkpool's own
// folders. A reparse point counts for nothing: it leads to an OS folder,
// which every account may read, to something probed on its own, or off
// the machine's drives, which the check doesn't cover.
func (v View) Probes(own string, private ...string) []string {
	paths := slices.Clone(private)
	add := func(dir string, entries []Entry, skip []string) {
		for _, e := range entries {
			p := Join(dir, e.Name)
			if !e.Link && !containsFold(skip, e.Name) && !strings.EqualFold(p, own) {
				paths = append(paths, p)
			}
		}
	}
	add(v.System, v.Top, SystemFolders)
	add(v.UsersFolder(), v.Users, sharedUsers)
	for _, d := range v.Drives {
		add(d.Root, d.Top, DriveFolders)
	}
	return sorted(paths)
}

// A Request is what a survey walks: each of Walk, through every folder
// below it on its own volume, and each of Shallow, by itself, but each
// folder in Skip and what's below it. A shared place in Guard, the
// folders of the OS's software, it guards, first telling Note, if it's
// set (ADR 0019).
type Request struct {
	Walk    []string          `json:"walk,omitempty"`
	Shallow []string          `json:"shallow,omitempty"`
	Skip    []string          `json:"skip,omitempty"`
	Guard   []string          `json:"guard,omitempty"`
	Note    func(Place) error `json:"-"`
}

// Request returns the survey of the machine v shows: the whole system
// drive, from its root, but the homes in Users, which the read check
// looks at one by one, own, the account's own profile, and skip,
// Silkpool's own folders; and each other fixed drive's root by itself,
// since operator data volumes are never walked (#19). It guards the
// shared places in the folders of the OS's software (ChangeRoots).
func (v View) Request(own string, skip ...string) Request {
	req := Request{Walk: []string{v.System}, Skip: slices.Clone(skip), Guard: ChangeRoots(v.System)}
	for _, e := range v.Users {
		if !e.Link && !containsFold(sharedUsers, e.Name) {
			req.Skip = append(req.Skip, Join(v.UsersFolder(), e.Name))
		}
	}
	if own != "" {
		req.Skip = append(req.Skip, own)
	}
	for _, d := range v.Drives {
		req.Shallow = append(req.Shallow, d.Root)
	}
	req.Skip = sorted(req.Skip)
	return req
}

// ChangeRoots are the folders of the OS's software on the system drive,
// system, that a later job's tools come from: a shared place in them
// fails the check (ADR 0005).
func ChangeRoots(system string) []string {
	return []string{Join(system, "Program Files"), Join(system, "Program Files (Arm)"), Join(system, "Program Files (x86)")}
}

// A Report is what a survey found, as the account.
type Report struct {
	// SID is the account's, whose token the survey acted as.
	SID string `json:"sid"`
	// Places are the shared places: the folders the walk found that the
	// account can create entries in, and doesn't own.
	Places []Place `json:"places,omitempty"`
	// Unwalked are the folders the walk couldn't look inside, even as
	// LocalSystem: nothing below them was walked.
	Unwalked []string `json:"unwalked,omitempty"`
	// Listed counts the folders the walk listed.
	Listed int `json:"listed,omitempty"`
	// Guarded counts the folders of the OS's software the account could
	// create entries in until the walk guarded them, and now can't.
	Guarded int `json:"guarded,omitempty"`
}

// A Place is a folder outside Silkpool's own that the account can create
// entries in, and doesn't own.
type Place struct {
	Path string `json:"path"`
	// Owner is its owner's SID, or "" if it has none, as on a drive with
	// no permissions.
	Owner string `json:"owner,omitempty"`
	// DeleteChild reports that the account may delete what others made
	// in it, as a folder without the Unix sticky bit lets it.
	DeleteChild bool `json:"deleteChild,omitempty"`
	// Volume and ID are the folder's volume serial number and file ID, as
	// the walk opened it, which the sweep checks: a folder put in its
	// stead since isn't the place.
	Volume uint64 `json:"volume"`
	ID     string `json:"id"`
}

// A Kind is what a hit is.
type Kind int

const (
	// Read is something outside the account's own that it can read.
	Read Kind = iota
	// Change is a folder of the OS's software the account can create
	// entries in.
	Change
	// Unseen is something the check or the survey couldn't look at, or
	// inside, so nothing shows the account can't read or change it.
	Unseen
)

// A Hit is something the account mustn't see or change, or that
// Silkpool can't show it can't.
type Hit struct {
	Path string
	Kind Kind
}

// Hits returns the hits of a check on the system drive system whose read
// check saw visible and couldn't look at unseen, beside survey, the
// latest survey: what it saw and couldn't look at, in the check's order,
// then, by path, each shared place in the folders of the OS's software,
// and each folder there the survey couldn't look inside. On Windows any
// shared place there counts, sticky or not: Windows has no sticky bit,
// and a job that can only add an entry beside a program can still plant
// a DLL it loads (ADR 0018).
func Hits(system string, visible, unseen []string, survey Report) []Hit {
	var hits []Hit
	for _, p := range visible {
		hits = append(hits, Hit{p, Read})
	}
	for _, p := range unseen {
		hits = append(hits, Hit{p, Unseen})
	}
	roots := ChangeRoots(system)
	changeable := func(p string) bool {
		return slices.ContainsFunc(roots, func(root string) bool { return Under(p, root) })
	}
	var changes []Hit
	for _, p := range survey.Places {
		if changeable(p.Path) {
			changes = append(changes, Hit{p.Path, Change})
		}
	}
	for _, p := range survey.Unwalked {
		if changeable(p) && !slices.ContainsFunc(changes, func(h Hit) bool { return strings.EqualFold(h.Path, p) }) {
			changes = append(changes, Hit{p, Unseen})
		}
	}
	slices.SortFunc(changes, func(a, b Hit) int { return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path)) })
	return append(hits, changes...)
}

// Exposed is a machine whose exposure check found something outside
// Silkpool's own that the service account can read, a folder of the OS's
// software it can change, or something it couldn't look at. Every slot
// fails closed with Slot can see files outside its own, until a check
// finds nothing.
type Exposed struct {
	// Detail names the first thing the check found, and its fix.
	Detail string
	Hits   []Hit
}

func (e *Exposed) Error() string { return e.Detail }

// Detail is the detail of a check that found hits: the first, with its
// fix, and how many more there are. The paths are the machine's, so
// they're scrubbed.
func Detail(hits []Hit) string {
	h := hits[0]
	p := secret.Scrub(h.Path)
	var d string
	switch h.Kind {
	case Change:
		d = fmt.Sprintf("%s can be changed by Silkpool's service account: take away other accounts' write access to it, under Security in its Properties", p)
	case Unseen:
		d = fmt.Sprintf("Silkpool couldn't look at everything in %s, so it can't show that its service account can't read or change it: "+
			"make sure only administrators and SYSTEM may change it, under Security in its Properties", p)
	default:
		d = fmt.Sprintf("%s can be read by Silkpool's service account: take away other accounts' access to it, under Security in its Properties", p)
	}
	if more := len(hits) - 1; more > 0 {
		d += fmt.Sprintf(" (and %d more)", more)
	}
	return d
}

// Join joins name to the folder dir, a Windows path.
func Join(dir, name string) string {
	if strings.HasSuffix(dir, `\`) {
		return dir + name
	}
	return dir + `\` + name
}

// Under reports whether path is dir or inside it, ignoring case.
func Under(path, dir string) bool {
	if strings.EqualFold(path, dir) {
		return true
	}
	dir = strings.TrimSuffix(dir, `\`) + `\`
	return len(path) > len(dir) && strings.EqualFold(path[:len(dir)], dir)
}

// containsFold reports whether names holds name, ignoring case.
func containsFold(names []string, name string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, name) })
}

// sorted returns paths sorted ignoring case, each once.
func sorted(paths []string) []string {
	slices.SortFunc(paths, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return slices.CompactFunc(paths, strings.EqualFold)
}

package winexposure

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"slices"
)

// A folder's rights, as winnt.h names them, which x/sys doesn't, or
// only on Windows.
const (
	fileReadData        = 0x1
	fileWriteData       = 0x2
	fileAddFile         = 0x2
	fileAppendData      = 0x4
	fileAddSubdirectory = 0x4
	fileWriteEA         = 0x10
	fileTraverse        = 0x20
	fileDeleteChild     = 0x40
	fileWriteAttributes = 0x100
	deleteRight         = 0x10000
	writeDAC            = 0x40000
	writeOwner          = 0x80000
)

// creates is what lets the account create an entry in a folder: adding a
// file or a folder, or changing who may, through its DACL or its owner.
const creates = fileAddFile | fileAddSubdirectory | writeDAC | writeOwner

// guardMask is what the guard denies the account in a folder of the OS's
// software, and in what's made in it: everything that writes, creates,
// deletes, or changes the DACL or the owner, and nothing that reads,
// lists, traverses or waits (ADR 0019).
const guardMask = fileWriteData | fileAppendData | fileWriteEA | fileDeleteChild | fileWriteAttributes |
	deleteRight | writeDAC | writeOwner

// readGuardMask is what the read guard denies the account in what the
// read check found it could read: all a file or folder's rights but
// reading its attributes and its DACL, and waiting on it. Jobs run
// without bypassing traverse checking, so nothing below a folder so
// guarded can be reached either (ADR 0020).
const readGuardMask = 0x1f01ff &^ (fileReadAttributes | readControl | synchronize)

// The rights the read guard leaves.
const (
	fileReadAttributes = 0x80
	readControl        = 0x20000
	synchronize        = 0x100000
)

// A guard is a kind of denial Silkpool puts first in a DACL: what it
// denies, and its inheritance flags.
type guard struct {
	mask  uint32
	flags byte
}

// The guards: the write guard of a shared place in the folders of the
// OS's software (ADR 0019), and the read guard of a folder or a file
// the read check found the account could read (ADR 0020).
var (
	writeGuard      = guard{guardMask, guardFlags}
	readGuardFolder = guard{readGuardMask, guardFlags}
	readGuardFile   = guard{readGuardMask, 0}
	guards          = []guard{writeGuard, readGuardFolder, readGuardFile}
)

// ACE types and flags, as winnt.h names them. A guard is inherited by
// the files and folders made in its folder; a copy one inherited has
// inheritedACE too.
const (
	aceAllowed       = 0
	aceDenied        = 1
	objectInherit    = 0x1
	containerInherit = 0x2
	inheritedACE     = 0x10
	guardFlags       = objectInherit | containerInherit
)

// ace is g's entry for the account whose SID is sid.
func (g guard) ace(sid []byte) []byte {
	b := []byte{aceDenied, g.flags, 0, 0}
	binary.LittleEndian.PutUint16(b[2:], uint16(8+len(sid)))
	b = binary.LittleEndian.AppendUint32(b, g.mask)
	return append(b, sid...)
}

// errBadACL is a DACL whose header doesn't hold its entries, or that
// the guard's entry wouldn't fit in. The guard leaves its folder.
var errBadACL = errors.New("its DACL can't take another entry")

// withGuard returns the DACL acl, as its bytes, with g's entry for sid
// first, before every other, which it keeps as they were: an explicit
// denial goes first in a DACL's canonical order. It reports whether it
// changed acl: one whose first entry is that guard already it leaves.
func withGuard(acl, sid []byte, kind guard) ([]byte, bool, error) {
	es, ok := aces(acl)
	if !ok {
		return nil, false, errBadACL
	}
	g := kind.ace(sid)
	if len(es) > 0 && bytes.Equal(es[0], g) {
		return acl, false, nil
	}
	out, ok := build(acl, append([][]byte{g}, es...))
	if !ok {
		return nil, false, errBadACL
	}
	return out, true, nil
}

// withoutGuard returns acl without each guard's entry for sid, of any
// kind, and whether it had any. Every other entry it keeps as it was.
func withoutGuard(acl, sid []byte) ([]byte, bool, error) {
	es, ok := aces(acl)
	if !ok {
		return nil, false, errBadACL
	}
	kept := slices.DeleteFunc(slices.Clone(es), func(e []byte) bool {
		return slices.ContainsFunc(guards, func(g guard) bool { return bytes.Equal(e, g.ace(sid)) })
	})
	if len(kept) == len(es) {
		return acl, false, nil
	}
	out, _ := build(acl, kept) // smaller than acl, so it fits
	return out, true, nil
}

// aces returns the entries acl's header counts, each as its bytes, and
// false if they don't fit in the size it gives.
func aces(acl []byte) ([][]byte, bool) {
	if len(acl) < 8 || aclSize(acl) < 8 || aclSize(acl) > len(acl) {
		return nil, false
	}
	rest := acl[8:aclSize(acl)]
	var es [][]byte
	for range binary.LittleEndian.Uint16(acl[4:]) {
		if len(rest) < 4 {
			return nil, false
		}
		size := int(binary.LittleEndian.Uint16(rest[2:]))
		if size < 4 || size > len(rest) {
			return nil, false
		}
		es, rest = append(es, rest[:size]), rest[size:]
	}
	return es, true
}

// build returns an ACL with old's header and the entries es, at
// revision 2 at least, and false if it's too big for an ACL.
func build(old []byte, es [][]byte) ([]byte, bool) {
	out := append([]byte{}, old[:8]...)
	for _, e := range es {
		out = append(out, e...)
	}
	if len(out) > 0xffff {
		return nil, false
	}
	out[0] = max(old[0], 2)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[4:], uint16(len(es)))
	return out, true
}

// aclSize is the size acl's header gives it.
func aclSize(acl []byte) int { return int(binary.LittleEndian.Uint16(acl[2:])) }

// SurveyFile is the file in the agent's state that keeps the latest
// survey, which the check and the sweep go by.
const SurveyFile = "shared-places"

// GuardFile is the file in the agent's state that records each folder a
// survey guarded, a line each (GuardLine), so the uninstall can take
// the guards away again (Unguard). A survey notes each before it guards
// it, so none is ever put without a record.
const GuardFile = "guarded"

// GuardLine is p's line in the guard ledger.
func GuardLine(p Place) []byte {
	b, _ := json.Marshal(Place{Path: p.Path, Volume: p.Volume, ID: p.ID}) // a Place always marshals
	return append(b, '\n')
}

// ParseGuarded reads the guard ledger b: each folder once, in the order
// first noted. A line that isn't a folder it leaves out: a survey writes
// each line, and syncs it, before it guards the folder, so a line an
// agent cut short never had a guard.
func ParseGuarded(b []byte) []Place {
	var places []Place
	seen := map[Place]bool{}
	for line := range bytes.Lines(b) {
		var p Place
		if json.Unmarshal(line, &p) != nil || p.Path == "" || !bytes.HasSuffix(line, []byte("\n")) || seen[p] {
			continue
		}
		seen[p] = true
		places = append(places, p)
	}
	return places
}

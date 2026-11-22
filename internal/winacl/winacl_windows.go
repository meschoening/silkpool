package winacl

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// An Entry is one allow entry of a DACL: whom it names, the rights it
// grants, and its inheritance flags.
type Entry struct {
	SID   *windows.SID
	Mask  windows.ACCESS_MASK
	Flags uint8
}

// A Mismatch says how a descriptor differs from the one wanted.
type Mismatch string

func (m Mismatch) Error() string { return string(m) }

// Check accepts sd only if owner owns it, and its DACL is present,
// protected, and exactly entries, in order. It returns a Mismatch saying
// how it differs otherwise.
func Check(sd *windows.SECURITY_DESCRIPTOR, owner *windows.SID, entries []Entry) error {
	o, _, err := sd.Owner()
	switch {
	case err != nil:
		return Mismatch("it has no owner")
	case o == nil || !o.Equals(owner):
		return Mismatch(fmt.Sprintf("it's owned by %s, not %s", name(o), name(owner)))
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 {
		return Mismatch("its DACL isn't present and protected")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return Mismatch("it has no DACL")
	}
	if int(dacl.AceCount) != len(entries) {
		return Mismatch(fmt.Sprintf("its DACL has %d entries, not %d", dacl.AceCount, len(entries)))
	}
	for i, e := range entries {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return err
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != e.Flags ||
			ace.Mask != e.Mask || !sid.Equals(e.SID) {
			return Mismatch(fmt.Sprintf("its DACL's entry %d isn't %s's 0x%x", i, name(e.SID), e.Mask))
		}
	}
	return nil
}

// WellKnown returns the SID of a well-known account or group.
func WellKnown(t windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	sid, err := windows.CreateWellKnownSid(t)
	if err != nil {
		// Each well-known SID Silkpool names exists on every Windows.
		panic(fmt.Sprintf("winacl: well-known SID %d: %v", t, err))
	}
	return sid
}

// name names a SID by its account, or by the SID itself.
func name(sid *windows.SID) string {
	if sid == nil {
		return "no one"
	}
	if n, _, _, err := sid.LookupAccount(""); err == nil && n != "" {
		return n
	}
	return sid.String()
}

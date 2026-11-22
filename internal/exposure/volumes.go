package exposure

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// macOS mounts some of its own APFS volumes in /Volumes, readable by
// every account by design, as an update left the boot container's
// Recovery volume mounted at /Volumes/Recovery on the reference Mac
// (#277). They hold what macOS boots and recovers from, never an
// account's files, so the read check doesn't probe them. What makes a
// volume one of them is the volume itself, never its name, which any
// disk image may take: it must have a system role, in an APFS container
// on the disk the Mac booted from (SystemVolume).

// SystemRoles are the APFS volume roles of macOS's own volumes that hold
// no account's files. Data, System and a volume with no role aren't
// among them: Data holds the homes, and System, mounted in /Volumes, is
// another install's, whose software may hold anything.
var SystemRoles = []string{"Hardware", "Preboot", "Recovery", "Update", "VM", "xART"}

// An APFSContainer is an APFS container, as diskutil apfs list -plist
// lists it: its own device, the devices of the physical stores it's on,
// and its volumes.
type APFSContainer struct {
	Reference string
	Stores    []string
	Volumes   []APFSVolume
}

// An APFSVolume is an APFS volume's device, such as disk3s3, and its
// roles.
type APFSVolume struct {
	Device string
	Roles  []string
}

// SystemVolume reports whether device, an APFS volume's device such as
// disk3s3, is one of macOS's own volumes that hold no account's files:
// a volume with one of SystemRoles, in a container whose physical stores
// are all on the disks the boot container's are on. boot
// is the device the root file system is mounted from, such as disk3s1s1,
// a snapshot of the boot container's System volume. A disk image or an
// account's external disk is never on the boot disk, whatever its
// volumes' roles or names, and adding a volume or a partition there takes
// an administrator, who could open any volume anyway.
func SystemVolume(device, boot string, containers []APFSContainer) bool {
	var bootDisks []string
	for _, c := range containers {
		if c.Reference != "" && strings.HasPrefix(boot, c.Reference+"s") {
			for _, s := range c.Stores {
				if d := wholeDisk(s); d != "" {
					bootDisks = append(bootDisks, d)
				}
			}
		}
	}
	if len(bootDisks) == 0 {
		return false
	}
	for _, c := range containers {
		for _, v := range c.Volumes {
			if v.Device != device {
				continue
			}
			if len(c.Stores) == 0 || !slices.ContainsFunc(v.Roles, func(r string) bool { return slices.Contains(SystemRoles, r) }) {
				return false
			}
			for _, s := range c.Stores {
				if !slices.Contains(bootDisks, wholeDisk(s)) {
					return false
				}
			}
			return true
		}
	}
	return false
}

var diskRE = regexp.MustCompile(`^(disk[0-9]+)(s[0-9]+)?$`)

// wholeDisk returns the whole disk that device, a disk or a partition of
// one such as disk0s2, is on, or "" if device names neither.
func wholeDisk(device string) string {
	m := diskRE.FindStringSubmatch(device)
	if m == nil {
		return ""
	}
	return m[1]
}

// ParseAPFSList reads the containers that diskutil apfs list -plist
// lists. It fails unless the output is a property list whose Containers
// is an array, and each container names its device.
func ParseAPFSList(b []byte) ([]APFSContainer, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	var root any
	for root == nil {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("diskutil apfs list's property list: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			if root, err = plistValue(d, se); err != nil {
				return nil, fmt.Errorf("diskutil apfs list's property list: %w", err)
			}
		}
	}
	top, _ := root.(map[string]any)
	list, ok := top["Containers"].([]any)
	if !ok {
		return nil, errors.New("diskutil apfs list's property list has no Containers")
	}
	var cs []APFSContainer
	for _, item := range list {
		m, _ := item.(map[string]any)
		c := APFSContainer{Reference: str(m["ContainerReference"])}
		if c.Reference == "" {
			return nil, errors.New("diskutil apfs list lists a container with no ContainerReference")
		}
		for _, s := range arr(m["PhysicalStores"]) {
			sm, _ := s.(map[string]any)
			c.Stores = append(c.Stores, str(sm["DeviceIdentifier"]))
		}
		for _, v := range arr(m["Volumes"]) {
			vm, _ := v.(map[string]any)
			vol := APFSVolume{Device: str(vm["DeviceIdentifier"])}
			for _, r := range arr(vm["Roles"]) {
				vol.Roles = append(vol.Roles, str(r))
			}
			c.Volumes = append(c.Volumes, vol)
		}
		cs = append(cs, c)
	}
	return cs, nil
}

func str(v any) string { s, _ := v.(string); return s }
func arr(v any) []any  { a, _ := v.([]any); return a }

// plistValue reads the value that se starts, through its end: a dict as
// a map, an array as a slice, true and false as bools, and any other
// element as its text.
func plistValue(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		key := ""
		haveKey := false
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				if haveKey {
					return nil, fmt.Errorf("the key %q has no value", key)
				}
				return m, nil
			case xml.StartElement:
				if t.Name.Local == "key" {
					if haveKey {
						return nil, fmt.Errorf("the key %q has no value", key)
					}
					if err := d.DecodeElement(&key, &t); err != nil {
						return nil, err
					}
					haveKey = true
					continue
				}
				if !haveKey {
					return nil, fmt.Errorf("a dict holds a %s with no key", t.Name.Local)
				}
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				m[key], haveKey = v, false
			}
		}
	case "array":
		var a []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				return a, nil
			case xml.StartElement:
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
		}
	case "true", "false":
		if err := d.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	default:
		var s string
		if err := d.DecodeElement(&s, &se); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return s, nil
	}
}

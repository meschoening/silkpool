package windows

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// A Shortcut is a shell link, a .lnk file, as the Start menu lists it:
// a program to start, the folder to start it in, and the words its
// tooltip shows. It's written in the format "[MS-SHLLINK]: Shell Link
// (.LNK) Binary File Format" sets out, so no COM is needed to make one.
// The shell finds the target by its item-ID list, as on the Windows VM a
// link naming it by its LinkInfo alone read as having none; the LinkInfo
// is there too, which Explorer uses to find a target that moved.
type Shortcut struct {
	Target, WorkingDir, Description string
}

// The format's values that Bytes uses.
const (
	linkHeaderSize = 0x4c
	// LinkFlags.
	hasIDList     = 0x01
	hasLinkInfo   = 0x02
	hasName       = 0x04
	hasWorkingDir = 0x10
	isUnicode     = 0x80
	// FileAttributes: FILE_ATTRIBUTE_NORMAL.
	fileAttributeNormal = 0x80
	// ShowCommand: SW_SHOWNORMAL.
	showNormal = 1
	// LinkInfoFlags: VolumeIDAndLocalBasePath.
	volumeIDAndLocalBasePath = 0x1
	// A LinkInfo header with the Unicode paths' offsets.
	linkInfoHeaderSize = 0x24
	// DriveType: DRIVE_FIXED.
	driveFixed = 3
	// A VolumeID with its label's offset, and an empty label.
	volumeIDSize = 0x11
)

// linkCLSID is the shell link's class ID,
// 00021401-0000-0000-C000-000000000046, as the header stores it.
var linkCLSID = [16]byte{0x01, 0x14, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}

// Bytes is the link as a .lnk file holds it: the header, a LinkInfo
// naming the target by its local path, in the system's code page where
// it's ASCII and in UTF-16 always, the description and working folder,
// and the terminal block.
func (s Shortcut) Bytes() ([]byte, error) {
	if len(s.Target) < 4 || s.Target[1] != ':' || s.Target[2] != '\\' {
		return nil, fmt.Errorf("%q isn't a local path, such as C:\\Program Files\\…", s.Target)
	}
	for _, part := range strings.Split(s.Target[3:], `\`) {
		if part == "" || strings.ContainsFunc(part, func(r rune) bool { return r < 0x20 || r >= 0x80 }) {
			return nil, fmt.Errorf("%q can't be named in a shell link's item IDs, which hold plain ASCII names", s.Target)
		}
	}
	for _, f := range []string{s.Target, s.WorkingDir, s.Description} {
		if strings.ContainsRune(f, 0) || len(utf16.Encode([]rune(f))) > 0xffff {
			return nil, fmt.Errorf("%q can't be in a shell link", f)
		}
	}
	var b bytes.Buffer
	le := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	// ShellLinkHeader.
	le(uint32(linkHeaderSize))
	b.Write(linkCLSID[:])
	le(uint32(hasIDList | hasLinkInfo | hasName | hasWorkingDir | isUnicode))
	le(uint32(fileAttributeNormal))
	le([3]uint64{}) // creation, access and write times: none
	le(uint32(0))   // FileSize
	le(int32(0))    // IconIndex
	le(uint32(showNormal))
	le(uint16(0))  // HotKey
	le([10]byte{}) // reserved
	b.Write(idList(s.Target))
	b.Write(linkInfo(s.Target))
	// StringData, each a count of UTF-16 units and the units, with no
	// NUL: NAME_STRING, then WORKING_DIR.
	for _, f := range []string{s.Description, s.WorkingDir} {
		u := utf16.Encode([]rune(f))
		le(uint16(len(u)))
		le(u)
	}
	le(uint32(0)) // TerminalBlock
	return b.Bytes(), nil
}

// myComputer is the shell item of This PC: its sort index, 0x50, and
// its class ID, 20D04FE0-3AEA-1069-A2D8-08002B30309D, as stored.
var myComputer = []byte{0x1f, 0x50, 0xe0, 0x4f, 0xd0, 0x20, 0xea, 0x3a, 0x69, 0x10, 0xa2, 0xd8, 0x08, 0x00, 0x2b, 0x30, 0x30, 0x9d}

// idList is the LinkTargetIDList naming target, a local path: This PC,
// its drive, then an item for each folder and the file, each its kind,
// its attributes and its name, with no size, times or extension blocks,
// which the shell fills in as it resolves the link.
func idList(target string) []byte {
	var items bytes.Buffer
	item := func(data []byte) {
		binary.Write(&items, binary.LittleEndian, uint16(len(data)+2))
		items.Write(data)
	}
	item(myComputer)
	item(append(append([]byte{0x2f}, target[:3]...), make([]byte, 19)...))
	parts := strings.Split(target[3:], `\`)
	for i, part := range parts {
		kind, attributes := byte(0x31), uint16(0x10) // a folder
		if i == len(parts)-1 {
			kind, attributes = 0x32, 0x20 // the file
		}
		data := []byte{kind, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(attributes), byte(attributes >> 8)}
		item(append(append(data, part...), 0))
	}
	items.Write([]byte{0, 0}) // TerminalID
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint16(items.Len()))
	b.Write(items.Bytes())
	return b.Bytes()
}

// linkInfo is the LinkInfo structure naming target, a local path on a
// fixed drive.
func linkInfo(target string) []byte {
	ansi := []byte(target)
	for i, c := range ansi {
		if c >= 0x80 {
			ansi[i] = '?' // the Unicode path below is the one Windows reads
		}
	}
	unicode := utf16.Encode([]rune(target))
	volumeIDOffset := uint32(linkInfoHeaderSize)
	localBasePathOffset := volumeIDOffset + volumeIDSize
	commonPathSuffixOffset := localBasePathOffset + uint32(len(ansi)) + 1
	localBasePathUnicodeOffset := commonPathSuffixOffset + 1
	commonPathSuffixUnicodeOffset := localBasePathUnicodeOffset + uint32(2*(len(unicode)+1))
	size := commonPathSuffixUnicodeOffset + 2

	var b bytes.Buffer
	le := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	le(size)
	le(uint32(linkInfoHeaderSize))
	le(uint32(volumeIDAndLocalBasePath))
	le(volumeIDOffset)
	le(localBasePathOffset)
	le(uint32(0)) // CommonNetworkRelativeLinkOffset: none
	le(commonPathSuffixOffset)
	le(localBasePathUnicodeOffset)
	le(commonPathSuffixUnicodeOffset)
	// VolumeID: its size, a fixed drive, no serial number, and an empty
	// label just after its own fields.
	le(uint32(volumeIDSize))
	le(uint32(driveFixed))
	le(uint32(0))
	le(uint32(0x10))
	b.WriteByte(0)
	b.Write(ansi)
	b.WriteByte(0)
	b.WriteByte(0) // CommonPathSuffix: empty
	le(unicode)
	le(uint16(0))
	le(uint16(0)) // CommonPathSuffixUnicode: empty
	return b.Bytes()
}

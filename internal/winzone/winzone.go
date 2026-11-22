// Package winzone names a Windows time zone by its IANA name, as an
// enroll tells the Hub the machine's zone. Windows keeps the zone in the
// registry under its own key name, such as "Central Standard Time";
// Silkpool's auto-resume needs the IANA name, such as "America/Chicago".
//
// The mapping is CLDR's windowsZones table, embedded as it's published:
// each Windows zone's "001" territory, its golden zone. The decision is
// in "Build the Windows agent service and its named-pipe control
// channel". windowsZones.xml is CLDR release 49's
// common/supplemental/windowsZones.xml, under the Unicode License; update
// it by copying a later release's file over it.
package winzone

import (
	_ "embed"
	"encoding/xml"
	"sync"
)

//go:embed windowsZones.xml
var table []byte

// zones maps each Windows zone's key name to its golden IANA zone.
var zones = sync.OnceValue(func() map[string]string {
	var doc struct {
		Zones []struct {
			Other     string `xml:"other,attr"`
			Territory string `xml:"territory,attr"`
			Type      string `xml:"type,attr"`
		} `xml:"windowsZones>mapTimezones>mapZone"`
	}
	// The table is embedded, so a table that doesn't parse fails every
	// test of this package rather than a node.
	if err := xml.Unmarshal(table, &doc); err != nil {
		panic("winzone: the embedded windowsZones table doesn't parse: " + err.Error())
	}
	m := make(map[string]string)
	for _, z := range doc.Zones {
		if z.Territory == "001" && z.Other != "" && z.Type != "" {
			m[z.Other] = z.Type
		}
	}
	return m
})

// IANA returns the IANA name of the Windows zone with key name key, and
// false if the table doesn't have it.
func IANA(key string) (string, bool) {
	zone, ok := zones()[key]
	return zone, ok
}

// Zones returns every Windows zone's key name with its IANA name.
func Zones() map[string]string {
	m := make(map[string]string, len(zones()))
	for k, v := range zones() {
		m[k] = v
	}
	return m
}

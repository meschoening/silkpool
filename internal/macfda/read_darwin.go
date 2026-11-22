package macfda

import "os"

// Read returns whether this process has Full Disk Access, by opening
// Probe for reading (FromOpen). It reads nothing from it.
func Read() (bool, error) {
	f, err := os.Open(Probe)
	if err == nil {
		f.Close()
	}
	return FromOpen(err)
}

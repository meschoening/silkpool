// Package linux builds the Linux packages, the node's silkpool and the
// Hub's silkpool-hub, as .deb and .rpm files with nfpm, from "Define how
// each binary is packaged, installed, upgraded and signed per OS",
// recorded as ADR 0013, and ADR 0014's slice. It holds what they install
// besides the binaries, and their maintainer scripts. The check gate's
// container lanes install the packages built from the commit, so that
// what they test is what an operator installs.
package linux

import (
	"embed"
	"io/fs"
)

//go:embed silkpool.service silkpool-hub.service silkpool.slice silkpool-hub.sysusers
var files embed.FS

// A File is one file a package installs: its name in this folder, the
// path it's installed at, its mode, and the package that holds it.
type File struct {
	Name, Path string
	Mode       fs.FileMode
	Package    string
}

// The packages: the node's and the Hub's.
const (
	Node = "silkpool"
	Hub  = "silkpool-hub"
)

// Files are the files the packages install, besides the binaries.
var Files = []File{
	{"silkpool.service", "/usr/lib/systemd/system/silkpool.service", 0o644, Node},
	{"silkpool.slice", "/usr/lib/systemd/system/silkpool.slice", 0o644, Node},
	{"silkpool-hub.service", "/usr/lib/systemd/system/silkpool-hub.service", 0o644, Hub},
	{"silkpool-hub.sysusers", "/usr/lib/sysusers.d/silkpool-hub.conf", 0o644, Hub},
}

// A Binary is one binary a package installs: the path it's installed
// at, and the command in the module it's built from.
type Binary struct {
	Path, Command string
	Package       string
}

// Binaries are the binaries the packages install.
var Binaries = []Binary{
	{"/usr/bin/silkpool", "./cmd/silkpool", Node},
	{"/usr/bin/silkpool-hub", "./cmd/silkpool-hub", Hub},
}

// Content returns the content of f.
func (f File) Content() []byte {
	b, err := files.ReadFile(f.Name)
	if err != nil {
		panic(err) // every File is embedded
	}
	return b
}

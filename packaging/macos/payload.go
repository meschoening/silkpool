package macos

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed io.github.meschoening.silkpool.agent.plist io.github.meschoening.silkpool.tray.plist Info.plist preinstall postinstall
var files embed.FS

// The folders Stage writes, in the folder it's given: what pkgbuild takes
// as --root and as --scripts, where the release workflow's macOS job
// signs and packs them.
const (
	RootDir    = "root"
	ScriptsDir = "scripts"
)

// Inputs are what a payload is made of besides this package's files: the
// release's version, and the paths of the two binaries, each built for
// darwin/arm64 and stamped with it.
type Inputs struct {
	Version  string // such as 1.2.3 or 1.2.3-rc.1
	Silkpool string // cmd/silkpool, without cgo
	Tray     string // cmd/silkpool-tray, with cgo, on a Mac
}

// An Entry is one file or folder Stage writes: its path under the folder
// it's given, its mode, and, for a file, where its bytes come from.
type Entry struct {
	Path string
	Mode fs.FileMode // with fs.ModeDir for a folder
	// From is the input file a binary is copied from, or empty.
	From string
	// Content is a file's bytes, when it isn't copied.
	Content []byte
}

// folder modes, as macOS has them: /Applications is the admin group's
// to write in too.
var folderModes = map[string]fs.FileMode{"Applications": 0o775}

// versionPattern is the versions a release carries, as packaging/linux
// takes them: semver without a v or build metadata.
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$`)

// Payload lists what Stage writes for in, folders first, each before
// what it holds.
func Payload(in Inputs) ([]Entry, error) {
	if !versionPattern.MatchString(in.Version) {
		return nil, fmt.Errorf("%q isn't a release's version, such as 1.2.3 or 1.2.3-rc.1", in.Version)
	}
	if in.Silkpool == "" || in.Tray == "" {
		return nil, fmt.Errorf("the payload needs both binaries")
	}
	bundleVersion, _, _ := strings.Cut(in.Version, "-")
	fill := strings.NewReplacer("@VERSION@", in.Version, "@BUNDLE_VERSION@", bundleVersion)
	read := func(name string) []byte {
		b, err := files.ReadFile(name)
		if err != nil {
			panic(err) // every name is embedded
		}
		return []byte(fill.Replace(string(b)))
	}
	installed := []Entry{
		{Path: Binary, Mode: 0o755, From: in.Silkpool},
		{Path: App + "/Contents/Info.plist", Mode: 0o644, Content: read("Info.plist")},
		{Path: AppBinary, Mode: 0o755, From: in.Tray},
		{Path: AgentPlist, Mode: 0o644, Content: read(AgentLabel + ".plist")},
		{Path: TrayPlist, Mode: 0o644, Content: read(TrayLabel + ".plist")},
	}
	var entries []Entry
	seen := map[string]bool{}
	var addFolders func(dir string)
	addFolders = func(dir string) {
		if dir == "." || seen[dir] {
			return
		}
		addFolders(path.Dir(dir))
		seen[dir] = true
		mode, ok := folderModes[strings.TrimPrefix(dir, RootDir+"/")]
		if !ok {
			mode = 0o755
		}
		entries = append(entries, Entry{Path: dir, Mode: fs.ModeDir | mode})
	}
	for _, e := range installed {
		e.Path = RootDir + e.Path
		addFolders(path.Dir(e.Path))
		entries = append(entries, e)
	}
	addFolders(ScriptsDir)
	for _, name := range []string{"preinstall", "postinstall"} {
		entries = append(entries, Entry{Path: ScriptsDir + "/" + name, Mode: 0o755, Content: read(name)})
	}
	return entries, nil
}

// Stage writes the payload for in into dir, which must not exist yet:
// pkgbuild's root and scripts, with each file's mode set as listed.
func Stage(dir string, in Inputs) error {
	entries, err := Payload(in)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		name := filepath.Join(dir, filepath.FromSlash(e.Path))
		if e.Mode.IsDir() {
			if err := os.Mkdir(name, e.Mode.Perm()); err != nil {
				return err
			}
		} else if err := writeEntry(name, e); err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		// Mkdir and OpenFile go through the umask.
		if err := os.Chmod(name, e.Mode.Perm()); err != nil {
			return err
		}
	}
	return nil
}

func writeEntry(name string, e Entry) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, e.Mode.Perm())
	if err != nil {
		return err
	}
	if e.From != "" {
		src, err := os.Open(e.From)
		if err != nil {
			f.Close()
			return err
		}
		_, err = io.Copy(f, src)
		src.Close()
		if err != nil {
			f.Close()
			return err
		}
	} else if _, err := f.Write(e.Content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

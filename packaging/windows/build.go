package windows

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tc-hib/winres"
	winversion "github.com/tc-hib/winres/version"

	"github.com/meschoening/silkpool/packaging/linux"
)

// versionVariable is the variable a build's version is stamped into.
const versionVariable = "github.com/meschoening/silkpool/internal/version.Version"

// The commands the programs are built from, in the module at a
// checkout's root. The tray app's is a module of its own (ADR 0009).
const (
	silkpoolCommand = "./cmd/silkpool"
	trayModule      = "cmd/silkpool-tray"
)

// Build builds the release's zip for version and arch from the module at
// root, and writes it to w. It cross-compiles both programs without cgo,
// from any host, and stamps each with a version resource of version.
func Build(ctx context.Context, root, version, arch string, w io.Writer) error {
	dir, err := os.MkdirTemp("", "silkpool-windows-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := BuildPrograms(ctx, root, version, arch, dir); err != nil {
		return err
	}
	return Zip(w, version, dir)
}

// BuildPrograms builds silkpool.exe and silkpool-tray.exe for arch from
// the module at root, stamped with version, into the folder dir.
func BuildPrograms(ctx context.Context, root, version, arch, dir string) error {
	if err := checkArch(arch); err != nil {
		return err
	}
	if err := linux.CheckVersion(version); err != nil {
		return err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := goBuild(ctx, root, arch, "-ldflags", "-X "+versionVariable+"="+version, "-o", filepath.Join(dir, Silkpool), silkpoolCommand); err != nil {
		return err
	}
	// The tray app is a GUI program, so starting it at sign-in or from the
	// Start menu opens no console window beside it.
	return goBuild(ctx, filepath.Join(root, trayModule), arch, "-ldflags", "-H=windowsgui -X "+versionVariable+"="+version, "-o", filepath.Join(dir, Tray), ".")
}

// goBuild runs go build -trimpath with args in dir, for windows/arch,
// without cgo.
func goBuild(ctx context.Context, dir, arch string, args ...string) error {
	cmd := exec.CommandContext(ctx, "go", append([]string{"build", "-trimpath"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH="+arch)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s in %s: %w\n%s", strings.Join(args, " "), dir, err, output.Bytes())
	}
	return nil
}

func checkArch(arch string) error {
	for _, a := range Arches {
		if a == arch {
			return nil
		}
	}
	return fmt.Errorf("there's no Silkpool for windows/%s", arch)
}

// zipTime is every entry's modification time, so the same programs make
// the same zip.
var zipTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Zip writes the release's zip for version to w, holding Programs from
// the folder dir, each stamped with a version resource of version
// (Stamp), at the top of the zip.
func Zip(w io.Writer, version, dir string) error {
	if err := linux.CheckVersion(version); err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	for _, name := range Programs {
		exe, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		stamped, err := Stamp(name, exe, version)
		if err != nil {
			return fmt.Errorf("stamping %s: %w", name, err)
		}
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: zipTime})
		if err != nil {
			return err
		}
		if _, err := f.Write(stamped); err != nil {
			return err
		}
	}
	return zw.Close()
}

// A Description is what a program's version resource says besides its
// version. Windows names the tray icon in Settings by the tray app's file
// description.
type Description struct {
	FileDescription, ProductName, InternalName, OriginalFilename string
}

// Descriptions are each program's. The tray app's is the one its own
// version resource carries (cmd/silkpool-tray/versioninfo), which Stamp
// checks.
var Descriptions = map[string]Description{
	Silkpool: {FileDescription: AppName, ProductName: AppName, InternalName: "silkpool", OriginalFilename: Silkpool},
	Tray:     {FileDescription: AppName, ProductName: AppName, InternalName: "silkpool-tray", OriginalFilename: Tray},
}

// Stamp returns the program name, the executable exe, with its version
// resource saying version, and every other resource it holds kept. A
// release's builds are stamped with its version only as they're linked,
// while the tray app's resource, linked from its module's object files,
// carries the source's. One already there must say what Descriptions
// says, besides its version, so the two can't drift apart.
func Stamp(name string, exe []byte, version string) ([]byte, error) {
	d, ok := Descriptions[name]
	if !ok {
		return nil, fmt.Errorf("%s isn't one of the zip's programs", name)
	}
	rs, err := winres.LoadFromEXE(bytes.NewReader(exe))
	if err == winres.ErrNoResources {
		rs, err = &winres.ResourceSet{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading its resources: %w", err)
	}
	if old := rs.Get(winres.RT_VERSION, winres.ID(1), winversion.LangDefault); old != nil {
		vi, err := winversion.FromBytes(old)
		if err != nil {
			return nil, fmt.Errorf("reading its version resource: %w", err)
		}
		if got := describe(vi); got != d {
			return nil, fmt.Errorf("its version resource says %+v, and packaging/windows says %+v", got, d)
		}
	}
	vi, err := VersionInfo(d, version)
	if err != nil {
		return nil, err
	}
	rs.SetVersionInfo(vi)
	var out bytes.Buffer
	if err := rs.WriteToEXE(&out, bytes.NewReader(exe)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// VersionInfo is the version resource of a program d describes, at
// version.
func VersionInfo(d Description, version string) (winversion.Info, error) {
	var vi winversion.Info
	for _, kv := range [][2]string{
		{winversion.FileDescription, d.FileDescription},
		{winversion.ProductName, d.ProductName},
		{winversion.InternalName, d.InternalName},
		{winversion.OriginalFilename, d.OriginalFilename},
		{winversion.FileVersion, version},
		{winversion.ProductVersion, version},
	} {
		if err := vi.Set(winversion.LangDefault, kv[0], kv[1]); err != nil {
			return vi, err
		}
	}
	// After the strings, so that they set the fixed part's numbers alone.
	vi.SetFileVersion(version)
	vi.SetProductVersion(version)
	return vi, nil
}

// ReadVersion returns the version a program's version resource says,
// as Stamp wrote it, and false if it has none.
func ReadVersion(exe io.ReadSeeker) (string, bool, error) {
	rs, err := winres.LoadFromEXESingleType(exe, winres.RT_VERSION)
	if err == winres.ErrNoResources {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	b := rs.Get(winres.RT_VERSION, winres.ID(1), winversion.LangDefault)
	if b == nil {
		return "", false, nil
	}
	vi, err := winversion.FromBytes(b)
	if err != nil {
		return "", false, err
	}
	v := vi.Table().GetMainTranslation()[winversion.ProductVersion]
	return v, v != "", nil
}

// describe is what vi says besides its version.
func describe(vi *winversion.Info) Description {
	t := vi.Table().GetMainTranslation()
	return Description{
		FileDescription:  t[winversion.FileDescription],
		ProductName:      t[winversion.ProductName],
		InternalName:     t[winversion.InternalName],
		OriginalFilename: t[winversion.OriginalFilename],
	}
}

package slot

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// systemBus is the system D-Bus socket. A slot that reached it could ask
// systemd and polkit for things on its own account.
const systemBus = "/run/dbus/system_bus_socket"

// osFolders are the Linux OS folders, from "Define how the agent runs and
// launches slots on each OS":
// the folders at the top of the file system that every account may read.
// Its /lib* is lib, lib32, lib64 and libx32, so that a data folder whose
// name starts with lib stays hidden. /home and /root are OS folders whose
// homes ProtectHome= hides.
var osFolders = []string{
	"bin", "boot", "dev", "etc", "home", "lib", "lib32", "lib64", "libx32",
	"lost+found", "nix", "opt", "proc", "root", "run", "sbin", "snap", "sys",
	"tmp", "usr", "var",
}

// A view is what a slot's unit would see, read just before it starts.
type view struct {
	// Top lists the entries at the top of the file system.
	Top []entry
	// Mounts lists every mount point.
	Mounts []string
	// Root and Slots list the names in the layout's Root and Root/slots.
	Root, Slots []string
	// Homes lists the entries in /home.
	Homes []entry
}

// An entry is a name in a folder, and whether it's a symbolic link.
type entry struct {
	name string
	link bool
}

// hidden returns the paths slot n's unit hides, sorted: the system D-Bus
// socket, the layout's Logs and Hidden, everything in Root but slot n's
// folder, and every entry or mount at the top of the file system outside
// the OS folders (outside).
func (l Layout) hidden(n int, v view) []string {
	paths := []string{systemBus, l.Logs}
	paths = append(paths, l.Hidden...)
	paths = append(paths, v.outside()...)
	for _, name := range v.Root {
		if name != "slots" {
			paths = append(paths, filepath.Join(l.Root, name))
		}
	}
	for _, name := range v.Slots {
		if name != strconv.Itoa(n) {
			paths = append(paths, filepath.Join(l.Root, "slots", name))
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// outside returns the entries and mounts at the top of the file system
// outside the OS folders, sorted, which no slot may see. A link at the
// top isn't one of them: it leads to an OS folder, which every slot may
// read, or to a place outside them through its own top-level entry, and
// hiding the link would hide what it leads to.
func (v view) outside() []string {
	var paths []string
	for _, e := range v.Top {
		if !e.link && !slices.Contains(osFolders, e.name) {
			paths = append(paths, "/"+e.name)
		}
	}
	for _, m := range v.Mounts {
		top, _, _ := strings.Cut(strings.TrimPrefix(m, "/"), "/")
		if top != "" && !slices.Contains(osFolders, top) {
			paths = append(paths, "/"+top)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// look reads what a slot's unit would see now.
func (l Layout) look() (view, error) {
	var v view
	top, err := os.ReadDir("/")
	if err != nil {
		return v, err
	}
	for _, e := range top {
		v.Top = append(v.Top, entry{e.Name(), e.Type()&os.ModeSymlink != 0})
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return v, err
	}
	defer f.Close()
	if v.Mounts, err = mountPoints(f); err != nil {
		return v, err
	}
	if v.Root, err = names(l.Root); err != nil {
		return v, err
	}
	if v.Slots, err = names(filepath.Join(l.Root, "slots")); err != nil {
		return v, err
	}
	homes, err := os.ReadDir("/home")
	if os.IsNotExist(err) {
		return v, nil
	}
	for _, e := range homes {
		v.Homes = append(v.Homes, entry{e.Name(), e.Type()&os.ModeSymlink != 0})
	}
	return v, err
}

// mountPoints returns the mount points mountinfo lists (mounts).
func mountPoints(mountinfo io.Reader) ([]string, error) {
	ms, err := mounts(mountinfo)
	points := make([]string, len(ms))
	for i, m := range ms {
		points[i] = m.point
	}
	return points, err
}

// A mount is a mount point, and whether it's mounted read-only.
type mount struct {
	point    string
	readOnly bool
}

// mounts returns the mounts mountinfo lists, in its order, which puts a
// mount after the one it's on: the fifth field of each line, with the
// octal escapes the kernel writes for spaces, tabs, newlines and
// backslashes undone, and the sixth, the mount's own options.
func mounts(mountinfo io.Reader) ([]mount, error) {
	var ms []mount
	s := bufio.NewScanner(mountinfo)
	for s.Scan() {
		if fields := strings.Fields(s.Text()); len(fields) > 5 {
			ro := slices.Contains(strings.Split(fields[5], ","), "ro")
			ms = append(ms, mount{unescape(fields[4]), ro})
		}
	}
	return ms, s.Err()
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if c, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(c))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// names returns the names in dir, or none if it doesn't exist.
func names(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	var list []string
	for _, e := range entries {
		list = append(list, e.Name())
	}
	return list, err
}

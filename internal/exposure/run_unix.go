//go:build unix

package exposure

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// Main is the helper, `silkpool check-exposure <request>`, which the
// agent runs as the service account in a launchd job with its slots'
// settings, so that what it finds is what a job could. It reads its
// Request as JSON, its one argument, carries it out (Run), and writes its
// Report to out as JSON. It exits 0 once it has done what it was asked,
// 1 if it couldn't, and 2 for a request it can't read.
func Main(args []string, out io.Writer) int {
	var req Request
	if len(args) != 1 || json.Unmarshal([]byte(args[0]), &req) != nil {
		fmt.Fprintln(os.Stderr, "check-exposure takes its request as JSON, its one argument")
		return 2
	}
	rep := Run(req)
	if err := json.NewEncoder(out).Encode(rep); err != nil || rep.Error != "" {
		return 1
	}
	return 0
}

// Run carries out req as the account it runs as: it probes each of
// req.Probe for the read check, and walks req.Walk, but req.Skip, for the
// shared places.
func Run(req Request) Report {
	rep := Report{UID: os.Getuid()}
	for _, p := range req.Probe {
		if visible(p) {
			rep.Visible = append(rep.Visible, p)
		}
	}
	if len(req.Walk) > 0 {
		var err error
		rep.Places, rep.Unlisted, rep.Listed, err = walk(req.Walk, req.Skip, rep.UID)
		if err != nil {
			rep.Error = err.Error()
		}
	}
	return rep
}

// visible reports whether the account it runs as can see path: list it,
// if it's a folder, open it, or connect to it, if it's a socket. A link
// counts for nothing.
func visible(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
		return false
	}
	switch {
	case fi.IsDir():
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		defer f.Close()
		_, err = f.Readdirnames(1)
		return err == nil || err == io.EOF
	case fi.Mode()&fs.ModeSocket != 0:
		c, err := net.Dial("unix", path)
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_NOFOLLOW, 0)
	if err == nil {
		f.Close()
	}
	return err == nil
}

// maxFound is the most shared places, or folders it can't list, a walk
// reports. A Mac with more has permissions no sweep should act on.
var maxFound = 10000

// walk walks each of roots, on its own volume, for the folders the
// account uid, which it runs as, can list and create entries in, and
// doesn't own: the shared places. It walks on through every folder it can
// list, but those in skip, links and folders uid owns, which the sweep
// deletes whole. A folder it can enter but not list it reports as
// unlisted, and it's no shared place, even if uid can write to it:
// nothing walks or sweeps inside it, since what's there can't be seen,
// and a count of such folders shows nothing (ADR 0017). A place is the
// very folder it listed, by the device, inode and owner of what it
// opened: a job may put another in its stead as it looks. A root that's
// missing or a link it skips, and so is a folder it can't look up: one
// whose lookup is refused, by its modes (EACCES) or by macOS's own
// protection (EPERM), as SIP and TCC refuse it for folders such as
// /Library/Caches/com.apple.aned. A job running as the account can't
// look such a folder up either, so it can't leave anything there. It
// fails on any other error.
func walk(roots, skip []string, uid int) (places []Place, unlisted []string, listed int, err error) {
	for _, root := range roots {
		root = filepath.Clean(root)
		var st unix.Stat_t
		if err := lstat(root, &st); err != nil {
			if unreachable(err) {
				continue
			}
			return nil, nil, 0, &os.PathError{Op: "lstat", Path: root, Err: err}
		}
		dev := st.Dev
		stack := []string{root}
		for len(stack) > 0 {
			dir := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if slices.ContainsFunc(skip, func(s string) bool { return under(dir, filepath.Clean(s)) }) {
				continue
			}
			if err := lstat(dir, &st); err != nil {
				if unreachable(err) {
					continue
				}
				return nil, nil, 0, &os.PathError{Op: "lstat", Path: dir, Err: err}
			}
			if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Dev != dev || int(st.Uid) == uid {
				continue
			}
			if beforeListing != nil {
				beforeListing(dir)
			}
			children, err := folders(dir, &st)
			switch {
			case err == nil && (st.Dev != dev || int(st.Uid) == uid):
			case err == nil:
				listed++
				if unix.Access(dir, unix.W_OK|unix.X_OK) == nil {
					places = append(places, Place{Path: dir, Owner: int(st.Uid), Sticky: st.Mode&unix.S_ISVTX != 0,
						Dev: uint64(st.Dev), Ino: uint64(st.Ino)})
				}
				for _, c := range children {
					stack = append(stack, filepath.Join(dir, c))
				}
			case errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM):
				if unix.Access(dir, unix.X_OK) == nil {
					unlisted = append(unlisted, dir)
				}
			case unreachable(err):
			default:
				return nil, nil, 0, err
			}
			if len(places) > maxFound || len(unlisted) > maxFound {
				return nil, nil, 0, fmt.Errorf("the walk found more than %d folders the account can create entries in, or can't list", maxFound)
			}
		}
	}
	return places, unlisted, listed, nil
}

// unreachable reports whether err is a path's that's gone, became a link
// or a file, or can't be reached: its lookup was refused, with EACCES,
// or with EPERM, which macOS returns for a folder SIP or TCC protects.
func unreachable(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) ||
		errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

// lstat looks a folder up for the walk. Tests set it to have a lookup
// fail as no folder an ordinary account makes can.
var lstat = unix.Lstat

// beforeListing, which tests set, runs just before the walk opens a
// folder to list it.
var beforeListing func(dir string)

// folders returns the names of the folders in dir, which it opens never
// through a link, and sets st to what it opened.
func folders(dir string, st *unix.Stat_t) ([]string, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	f := os.NewFile(uintptr(fd), dir)
	defer f.Close()
	if err := unix.Fstat(fd, st); err != nil {
		return nil, &os.PathError{Op: "fstat", Path: dir, Err: err}
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

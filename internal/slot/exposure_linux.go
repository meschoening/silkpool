package slot

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
)

// The exposure check is in "Define how the agent runs and launches slots
// on each OS"
// and ADR 0005.

// Exposed is a slot whose exposure check found something outside its own
// folders that it can read, or a folder of the OS's software that it can
// change. It fails closed with Slot can see files outside its own, until
// a check finds nothing.
type Exposed struct {
	// Detail names the first thing the check found, and its fix.
	Detail string
	hits   []hit
}

func (e *Exposed) Error() string { return e.Detail }

// Ready readies slot n for a runner, as PrepareSlot asks, and returns
// what the node applies: Prepared, or PrepareFailed with SlotCantStart or
// SlotExposed. It prepares the slot's account (Prepare), then runs the
// exposure check as it (Check). Any failure but a hit is SlotCantStart:
// the slot can't be vouched for.
func (s *Slots) Ready(ctx context.Context, n int) node.Event {
	err := s.Prepare(n)
	var cant *CantStart
	if err != nil && !errors.As(err, &cant) {
		err = fmt.Errorf("couldn't prepare slot %d's account: %w", n, err)
	}
	if err == nil {
		err = s.Check(ctx, n)
	}
	var exposed *Exposed
	switch {
	case err == nil:
		return node.Prepared{Slot: n}
	case errors.As(err, &exposed):
		return node.PrepareFailed{Slot: n, Reason: node.SlotExposed, Detail: exposed.Detail}
	case errors.As(err, &cant):
		return node.PrepareFailed{Slot: n, Reason: node.SlotCantStart, Detail: cant.Detail}
	}
	return node.PrepareFailed{Slot: n, Reason: node.SlotCantStart, Detail: detail(err)}
}

// checkTimeout is how long an exposure check may take.
const checkTimeout = 2 * time.Minute

// Check runs slot n's exposure check as its slot account, in a unit with
// its runners' sandbox, recomputed now, so that it sees what a job would.
// It returns nil if the slot can see nothing outside its own folders and
// the OS folders, and can change no folder of the OS's software; an
// *Exposed naming the first thing it found; or another error, such as a
// sandbox that can't be set up.
//
//   - The read check probes each entry and mount at the top of the file
//     system outside the OS folders, and each home in /home, and /root.
//     The agent lists them from outside the unit, so that the sandbox
//     can't keep them from the check.
//   - The write check walks /usr, /opt, /nix and /snap for a folder that
//     the slot can write to, that isn't sticky and isn't the slot's own.
//
// The Linux sandbox should leave a slot nothing to find, so the check
// proves that it does.
func (s *Slots) Check(ctx context.Context, n int) error {
	if _, err := user.Lookup(s.Account(n)); err != nil {
		return fmt.Errorf("can't find slot %d's account: %w", n, err)
	}
	v, err := s.look()
	if err != nil {
		return fmt.Errorf("can't work out what slot %d may see: %w", n, err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	unit := s.checkUnit(n, strings.ToLower(rand.Text()[:8]))
	props := slices.Concat([]property{
		{"Description", dbus.MakeVariant(fmt.Sprintf("Silkpool exposure check of slot %d", n))},
		{"Type", dbus.MakeVariant("exec")},
		{"ExecStart", dbus.MakeVariant([]execCommand{{s.Launcher, append([]string{s.Launcher, CheckMode}, v.exposable()...), false}})},
		{"StandardOutputFileDescriptor", dbus.MakeVariant(dbus.UnixFD(w.Fd()))},
		{"RuntimeMaxUSec", dbus.MakeVariant(uint64(checkTimeout / time.Microsecond))},
		{"AddRef", dbus.MakeVariant(true)},
		{"CollectMode", dbus.MakeVariant("inactive-or-failed")},
		// The runner root is made for each runner, after the check.
	}, s.sandbox(n, s.hidden(n, v), s.Home(n), "-"+s.RunnerRoot(n)))
	err = s.startUnit(ctx, unit, props, fmt.Sprintf("slot %d's exposure check", n))
	w.Close()
	if err != nil {
		return err
	}
	type result struct {
		report report
		err    error
	}
	got := make(chan result, 1)
	go func() {
		var rep report
		err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&rep)
		got <- result{rep, err}
	}()
	exit, err := s.waitUnit(ctx, unit)
	code := exit.Code
	if err != nil {
		s.manager(context.WithoutCancel(ctx), "KillUnit", unit, "all", int32(syscall.SIGKILL))
		return fmt.Errorf("slot %d's exposure check didn't finish: %w", n, err)
	}
	// systemd holds the pipe's other end until it unloads the unit, so
	// the report ends where its JSON does. The helper has written it all.
	r.SetReadDeadline(time.Now().Add(time.Second))
	res := <-got
	switch {
	case code != 0 || res.err != nil || res.report.Error != "":
		why := res.report.Error
		if why == "" && res.err != nil {
			why = res.err.Error()
		}
		return fmt.Errorf("slot %d's exposure check failed with exit code %d: %s", n, code, why)
	case len(res.report.Hits) > 0:
		return &Exposed{exposure(n, res.report), res.report.Hits}
	}
	return nil
}

// checkUnit is the name of slot n's exposure check unit, unique by id.
// It isn't a runner's, so Alive never names it.
func (l Layout) checkUnit(n int, id string) string {
	return fmt.Sprintf("%s-check-slot%d-%s.service", l.Name, n, id)
}

// exposable returns the paths the read check probes, sorted: what's
// outside the OS folders at the top of the file system, each home in
// /home, and /root. Homes that are links lead somewhere outside, or to
// another home.
func (v view) exposable() []string {
	paths := v.outside()
	for _, e := range v.Homes {
		if !e.link {
			paths = append(paths, "/home/"+e.name)
		}
	}
	paths = append(paths, "/root")
	slices.Sort(paths)
	return slices.Compact(paths)
}

// CheckMode is the hidden argument that runs silkpool as the exposure
// check, `silkpool check-exposure <path>...`, which the agent runs as a
// slot account in a unit with its runners' sandbox.
const CheckMode = "check-exposure"

// writeRoots are the folders of the OS's software that a later job's
// tools come from, which the write check walks.
var writeRoots = []string{"/usr", "/opt", "/nix", "/snap"}

// maxHits is how many folders the write check names at most.
const maxHits = 20

// A report is what the exposure check's helper writes.
type report struct {
	Hits []hit `json:"hits"`
	// More reports that the write check found more than maxHits.
	More bool `json:"more,omitempty"`
	// Error is why the check couldn't run.
	Error string `json:"error,omitempty"`
}

// A hit is something a slot can read, or change, that it mustn't.
type hit struct {
	Path   string `json:"path"`
	Change bool   `json:"change,omitempty"`
}

// checkExposure is the exposure check's helper. Running as the slot
// account in its unit, it probes paths for the read check, walks
// writeRoots for the write check, and writes its report to out as JSON.
func checkExposure(paths []string, out io.Writer) int {
	var rep report
	for _, p := range paths {
		if visible(p) {
			rep.Hits = append(rep.Hits, hit{Path: p})
		}
	}
	f, err := os.Open("/proc/self/mountinfo")
	var ms []mount
	if err == nil {
		ms, err = mounts(f)
		f.Close()
	}
	code := 0
	if err != nil {
		rep.Error, code = "can't read the mounts: "+err.Error(), 1
	} else {
		var changeable []string
		changeable, rep.More = writable(writeRoots, ms, os.Getuid())
		for _, p := range changeable {
			rep.Hits = append(rep.Hits, hit{Path: p, Change: true})
		}
	}
	if err := json.NewEncoder(out).Encode(rep); err != nil {
		return 1
	}
	return code
}

// visible reports whether the account it runs as can see path: list it,
// if it's a folder, open it, or connect to it, if it's a socket. A link
// counts for nothing, as it does for the sandbox (outside).
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

// writable walks roots for folders the account it runs as can write to,
// that aren't sticky and that uid doesn't own, and returns at most
// maxHits of them, and whether there were more. It skips what it can't
// list, and every part of a read-only mount, as ms has them, that no
// writable mount lies under: nothing in it can be written.
func writable(roots []string, ms []mount, uid int) (found []string, more bool) {
	for _, root := range roots {
		if p, err := filepath.EvalSymlinks(root); err == nil {
			root = p
		}
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if mountOf(ms, path).readOnly {
				if !writableBelow(ms, path) {
					return filepath.SkipDir
				}
				return nil
			}
			if changeable(path, uid) {
				if len(found) == maxHits {
					more = true
					return filepath.SkipAll
				}
				found = append(found, path)
			}
			return nil
		})
	}
	return found, more
}

// changeable reports whether the folder path lets the account it runs as
// create and delete entries, isn't sticky, so that it could replace
// other accounts' files, and isn't uid's own.
func changeable(path string, uid int) bool {
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil || st.Mode&unix.S_ISVTX != 0 || int(st.Uid) == uid {
		return false
	}
	return unix.Access(path, unix.W_OK) == nil
}

// mountOf returns the mount path is on: the last of ms whose point is
// path or a folder above it.
func mountOf(ms []mount, path string) mount {
	var on mount
	for _, m := range ms {
		if under(path, m.point) {
			on = m
		}
	}
	return on
}

// writableBelow reports whether a writable mount of ms lies below path.
func writableBelow(ms []mount, path string) bool {
	for _, m := range ms {
		if !m.readOnly && m.point != path && under(m.point, path) {
			return true
		}
	}
	return false
}

// under reports whether path is dir or inside it.
func under(path, dir string) bool {
	return path == dir || dir == "/" || strings.HasPrefix(path, dir+"/")
}

// exposure is the detail of a check that found the hits in r: the first,
// with its fix, and how many more it found. The path is the machine's,
// so it's scrubbed.
func exposure(n int, r report) string {
	h := r.Hits[0]
	p := quote(secret.Scrub(h.Path))
	d := fmt.Sprintf("%s can be read by slot %d's account: take away other accounts' access with chmod o-rwx %s", p, n, p)
	if h.Change {
		d = fmt.Sprintf("%s can be changed by slot %d's account: take away other accounts' write access with chmod o-w %s", p, n, p)
	}
	switch more := len(r.Hits) - 1; {
	case r.More:
		d += fmt.Sprintf(" (and at least %d more)", more+1)
	case more > 0:
		d += fmt.Sprintf(" (and %d more)", more)
	}
	return d
}

// plain is a path a shell takes as it is.
var plain = regexp.MustCompile(`^[A-Za-z0-9_./+:@%,=-]+$`)

// quote quotes path for a shell, if it needs it.
func quote(path string) string {
	if plain.MatchString(path) {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

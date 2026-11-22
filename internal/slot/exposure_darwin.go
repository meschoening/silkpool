package slot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/meschoening/silkpool/internal/exposure"
	"github.com/meschoening/silkpool/internal/launchd"
)

// The exposure check and the shared-places sweep are in "Define how the
// agent runs and launches slots on each OS", ADRs 0005 and 0017, and "Use
// one Silkpool service account per machine instead of per-slot accounts"
// (#214). What they decide is in internal/exposure.

// CheckMode is the hidden argument that runs silkpool as the exposure
// check's helper, `silkpool check-exposure <request>`, which the agent
// runs as the service account in a launchd job (exposure.Main).
const CheckMode = "check-exposure"

// A ServiceAccount is the Mac's one Silkpool service account, which every
// slot's runners run as (#214), as its caller has shown it's Silkpool's.
type ServiceAccount struct {
	Name     string
	UID, GID int
}

// Exposed is a Mac whose exposure check found something outside
// Silkpool's own that the service account can read, or a folder of the
// OS's software it can change, or can't show it can't. Every slot fails
// closed with Slot can see files outside its own, until a check finds
// nothing.
type Exposed struct {
	// Detail names the first thing the check found, and its fix.
	Detail string
	Hits   []exposure.Hit
}

func (e *Exposed) Error() string { return e.Detail }

const (
	// checkBound is how long the exposure check's job may take: it only
	// probes.
	checkBound = time.Minute
	// surveyBound is how long the survey's may: on the reference Mac, a
	// walk of its 244,000 folders as the macos lane's account took a
	// minute.
	surveyBound = 10 * time.Minute
	// maxReport is the most of a report the agent reads.
	maxReport = 4 << 20
)

// A survey is the latest survey, as Silkpool saves it.
type survey struct {
	Time time.Time `json:"time"`
	exposure.Report
}

// surveyFile is the file in the agent's state that keeps the latest
// survey.
func (l Layout) surveyFile() string { return filepath.Join(l.state(), "shared-places") }

// Survey works out the Mac's shared places: the folders outside
// Silkpool's own where a, the service account, can create entries. It
// walks, as a, in a launchd job with its slots' jobs' settings, the
// places on the data volume, the only one of the Mac's own a job can
// write to (exposure.WalkRoots), but Silkpool's own folders and
// /private/var/folders, whose account's folders the account-wide clean
// empties. It saves what it found, for Check, which counts the folders of
// the OS's software among them, and for the sweep. A survey that fails is
// saved as failed, so the check and the sweep fail closed until one
// succeeds.
//
// The agent surveys at enrollment, on start and daily (#19): it takes
// about a minute.
func (s *Slots) Survey(ctx context.Context, a ServiceAccount) error {
	rep, err := s.surveyAs(ctx, a)
	saved := survey{Time: time.Now().UTC(), Report: rep}
	if err != nil {
		saved.Report = exposure.Report{UID: a.UID, Error: err.Error()}
	}
	return errors.Join(err, s.saveSurvey(saved))
}

func (s *Slots) surveyAs(ctx context.Context, a ServiceAccount) (exposure.Report, error) {
	roots, err := s.walkRoots()
	if err != nil {
		return exposure.Report{}, fmt.Errorf("can't tell where the service account can write: %w", err)
	}
	skip := []string{"/private/var/folders"}
	for _, p := range []string{s.Root, s.Logs} {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		skip = append(skip, p)
	}
	return s.runHelper(ctx, a, exposure.Request{Walk: roots, Skip: skip}, s.surveyBound)
}

// saveSurvey saves sv as the latest survey.
func (s *Slots) saveSurvey(sv survey) error {
	b, err := json.Marshal(sv)
	if err == nil {
		err = os.MkdirAll(s.state(), 0o700)
	}
	if err == nil {
		err = writeRecord(s.surveyFile(), string(b))
	}
	if err != nil {
		return fmt.Errorf("can't save the shared places: %w", err)
	}
	return nil
}

// readSurvey reads the latest survey, which must have succeeded, as a.
func (s *Slots) readSurvey(a ServiceAccount) (survey, error) {
	var sv survey
	b, err := os.ReadFile(s.surveyFile())
	if os.IsNotExist(err) {
		return sv, errors.New("the shared places haven't been worked out yet, so nothing shows where the service account can write outside its own folders")
	}
	if err == nil {
		err = json.Unmarshal(b, &sv)
	}
	switch {
	case err != nil:
		return sv, fmt.Errorf("can't read the shared places: %w", err)
	case sv.Error != "":
		return sv, fmt.Errorf("the last survey couldn't work out where the service account can write outside its own folders: %s", sv.Error)
	case sv.UID != a.UID:
		return sv, fmt.Errorf("the shared places were worked out for UID %d, not the service account's %d", sv.UID, a.UID)
	}
	return sv, nil
}

// Check runs the Mac's exposure check as a, the service account, in a
// launchd job with its slots' jobs' settings, so that it sees what a job
// would. It probes, as a, each entry at the top of the file system
// outside the OS folders, each home in /Users, each volume in /Volumes,
// and Silkpool's state, logs, runner installs and jobs' output folder
// (exposure.View). a may enter the output folder, to write its jobs'
// output, but not list it: a check that can list it reports it. Beside
// what it sees, it counts the folders of the OS's software that the
// latest survey found a can change, or can enter but not list.
//
// It returns nil if a can see and change none of them; an *Exposed naming
// the first it found; or another error, when the survey hasn't
// succeeded, or was of another account, or the job can't run. Each fails
// every slot closed.
func (s *Slots) Check(ctx context.Context, a ServiceAccount) error {
	sv, err := s.readSurvey(a)
	if err != nil {
		return err
	}
	v, err := s.view()
	if err != nil {
		return fmt.Errorf("can't read what the service account may see: %w", err)
	}
	probes := v.Probes(s.state(), s.Installs(), s.Logs, s.output())
	rep, err := s.runHelper(ctx, a, exposure.Request{Probe: probes}, s.checkBound)
	if err != nil {
		return fmt.Errorf("the exposure check couldn't run: %w", err)
	}
	if hits := exposure.Hits(rep.Visible, sv.Report); len(hits) > 0 {
		return &Exposed{Detail: exposure.Detail(hits), Hits: hits}
	}
	return nil
}

// runHelper runs the helper with req as a, and returns its report. It
// runs in the check's launchd job, with the settings the slots' jobs
// have: UserName, so it runs as a with the groups launchd gives a, and
// SessionCreate. Its output goes to an output file of its own
// (newOutput), which the agent takes back once the job has ended, for
// what the helper said if it failed. Its report it sends to the agent
// over a socket, and the agent takes it only from the check's job's own
// process (acceptReport): every job of a's may write to the output file,
// since launchctl print shows its path, so a busy slot's job could plant
// a report there (#241). A job of the check's label that an earlier
// check left is unloaded first, if it's from Silkpool's own plist; one
// from another plist fails the check, and is left. bound limits the job:
// one still running then is killed. It fails unless the job ends with 0,
// and its report reads whole, as a's. Checks and surveys take turns, since
// they share the job: one waits for another's to end. The account-wide
// clean, which kills every process of a, waits for the job too.
func (s *Slots) runHelper(ctx context.Context, a ServiceAccount, req exposure.Request, bound time.Duration) (exposure.Report, error) {
	s.use.RLock()
	defer s.use.RUnlock()
	s.checking.Lock()
	defer s.checking.Unlock()
	var rep exposure.Report
	arg, err := json.Marshal(req)
	if err != nil {
		return rep, err
	}
	if err := s.jobsFolder(); err != nil {
		return rep, err
	}
	label := s.checkLabel()
	plist := filepath.Join(s.jobs(), label+".plist")
	if err := s.unload(ctx, label, plist); err != nil {
		return rep, fmt.Errorf("the check's job: %w", err)
	}
	out, err := s.newOutput(a.UID, a.GID)
	if err != nil {
		return rep, fmt.Errorf("can't make the check's output file: %w", err)
	}
	defer os.Remove(out)
	sock, ln, err := s.reportSocket()
	if err != nil {
		return rep, fmt.Errorf("can't make the check's socket: %w", err)
	}
	defer os.Remove(sock)
	defer ln.Close()
	reports := make(chan report, 1)
	go func() { reports <- s.acceptReport(ctx, ln, label) }()
	job := launchd.Job{
		Label:            label,
		Program:          s.Launcher,
		Args:             []string{CheckMode, string(arg), sock},
		UserName:         a.Name,
		SessionCreate:    true,
		SessionType:      s.sessionType,
		RunAtLoad:        true,
		WorkingDirectory: "/",
		Stdout:           out,
		Stderr:           out,
	}
	if err := writeFile(plist, job.Plist(), 0o644); err != nil {
		return rep, err
	}
	if err := s.ctl.Bootstrap(ctx, s.Domain, plist); err != nil {
		os.Remove(plist)
		return rep, fmt.Errorf("launchd didn't load the check's job: %w", err)
	}
	code, err := s.await(ctx, label, bound)
	if err = errors.Join(err, s.unload(context.WithoutCancel(ctx), label, plist)); err != nil {
		return rep, err
	}
	ln.Close()
	r := <-reports
	f, err := s.reclaim(out)
	if err != nil {
		return rep, fmt.Errorf("can't take back the check's output: %w", err)
	}
	defer f.Close()
	said, _ := readAtMost(f, 4096)
	err = r.err
	if err == nil {
		err = decodeOne(r.b, &rep)
	}
	switch {
	case code != 0 && err == nil && rep.Error != "":
		return rep, fmt.Errorf("the check's helper failed: %s", rep.Error)
	case code != 0:
		if hint := exConfigHint(code, int64(len(said))); hint != "" {
			return rep, fmt.Errorf("the check's job didn't start: %s", hint)
		}
		return rep, fmt.Errorf("the check's helper exited with %d: %s", code, bytes.TrimSpace(said))
	case err != nil:
		return rep, fmt.Errorf("can't read the check's report: %w", err)
	case rep.UID != a.UID:
		return rep, fmt.Errorf("the check ran as UID %d, not the service account's %d", rep.UID, a.UID)
	case rep.Error != "":
		return rep, errors.New(rep.Error)
	}
	return rep, nil
}

// A report is what the check's job sent the agent, or why it sent none.
type report struct {
	b   []byte
	err error
}

// reportSocket makes the socket the check's job sends its report to, in
// the output folder, which the service account may enter, and lets every
// account connect to it: the agent takes a report only from the check's
// job's process. It returns the socket's path.
func (s *Slots) reportSocket() (string, *net.UnixListener, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	path := filepath.Join(s.output(), hex.EncodeToString(b[:])+".sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return "", nil, err
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		os.Remove(path)
		return "", nil, err
	}
	return path, ln, nil
}

// acceptReport takes the report of the check's job, label, from the
// first connection to ln whose process is the job's, as launchd names
// it while it runs, and refuses every other, logging it. The helper
// keeps the connection open until the agent has read it, so it's still
// running then. It ends once ln is closed, with an error if no report
// came.
func (s *Slots) acceptReport(ctx context.Context, ln *net.UnixListener, label string) report {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return report{err: errors.New("the check's job sent no report")}
		}
		pid, err := peerPID(c)
		if err == nil {
			var st launchd.Status
			st, err = s.ctl.Print(ctx, s.Domain, label)
			if err == nil && (!st.Running() || st.PID != pid) {
				err = fmt.Errorf("process %d isn't the check's job, %+v", pid, st)
			}
		}
		if err != nil {
			c.Close()
			s.logger().Warn("Refused a report for the exposure check that didn't come from its job", "error", err)
			continue
		}
		c.SetReadDeadline(time.Now().Add(time.Minute))
		b, err := readAtMost(c, maxReport)
		c.Close()
		return report{b: b, err: err}
	}
}

// peerPID is the process that connected c.
func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	cerr := raw.Control(func(fd uintptr) { pid, err = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID) })
	return pid, errors.Join(cerr, err)
}

// await waits until launchd says the job label has run and ended, and
// returns how it ended, as Wait reads a runner's job. One still running
// after bound, or when ctx ends, it kills, and fails.
func (s *Slots) await(ctx context.Context, label string, bound time.Duration) (int, error) {
	deadline := time.Now().Add(bound)
	tick := time.NewTicker(s.poll)
	defer tick.Stop()
	for {
		st, err := s.ctl.Print(ctx, s.Domain, label)
		switch {
		case err != nil:
			return 0, err
		case !st.Loaded:
			return 0, errors.New("the check's job is gone before it ended")
		case st.Exited && st.State == "not running":
			return st.Code, nil
		}
		late := time.Now().After(deadline)
		if late || ctx.Err() != nil {
			s.ctl.Kill(context.WithoutCancel(ctx), "SIGKILL", s.Domain, label)
			if late {
				return 0, fmt.Errorf("the check's job didn't end within %v, so it was killed", bound)
			}
			return 0, ctx.Err()
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
		}
	}
}

// readAtMost reads r, and fails if it's longer than limit, returning
// its first limit bytes.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(b)) > limit {
		return b[:limit], fmt.Errorf("it's longer than %d bytes", limit)
	}
	return b, err
}

// decodeOne decodes b, which must hold one JSON value and nothing else.
func decodeOne(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("more follows the report")
	}
	return nil
}

// sweep empties the Mac's shared places, as the latest survey of a, the
// service account, found them, of what a owns. Every slot's jobs run as
// a, so what a owns there may be any slot's: sweep runs in the
// account-wide clean, once every slot is idle (#214), and fails while
// any slot still records a runner.
//
// It deletes each entry a owns at the top of a place, whole, but never
// follows a link: it opens each place through the folders above it,
// none of them a link, and a place that's gone or became a link holds
// nothing of a's there. It sweeps only the very folder the survey listed
// as a, by its device and inode: one put in its stead since, by a job
// renaming folders in a folder that isn't sticky, a may not be able to
// list, so it's left (ADR 0017). Inside a folder a owns, it deletes only what a
// owns too: an entry a doesn't own, another account may have made, or a
// job may have linked in, so it's left, and fails the sweep, naming it
// (ADR 0017). Entries a doesn't own at the top of a place are other
// accounts', and left. After a failure it sweeps the other places, and
// returns every failure.
func (s *Slots) sweep(ctx context.Context, a ServiceAccount) error {
	recorded, err := s.Recorded()
	if err != nil {
		return fmt.Errorf("can't tell whether every slot is idle: %w", err)
	}
	if len(recorded) > 0 {
		n := slices.Min(slices.Collect(maps.Keys(recorded)))
		return fmt.Errorf("slot %d still holds runner %s, and the shared places are swept only once every slot is idle", n, recorded[n])
	}
	sv, err := s.readSurvey(a)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range sv.Places {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := sweepPlace(p, a.UID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sweepPlace deletes what uid owns at the top of the folder p, if the
// folder at its path is still the one the survey listed. It skips an
// entry that's gone, and one whose stat macOS refuses even root with
// EPERM, as SIP and TCC refuse /Library/Caches/com.apple.aned: whose
// such an entry is can't be established, so the sweep leaves it, as it
// acts only on what it can show is the account's (ADR 0017). Any other
// error fails the sweep.
func sweepPlace(p exposure.Place, uid int) error {
	path := p.Path
	f, err := openPlace(path)
	if f == nil {
		return err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if uint64(st.Dev) != p.Dev || uint64(st.Ino) != p.Ino {
		return nil
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}
	r := remover{uid: uid, ownedOnly: true}
	var errs []error
	for _, name := range names {
		var st unix.Stat_t
		err := fstatat(int(f.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, unix.ENOENT), errors.Is(err, unix.EPERM):
		case err != nil:
			errs = append(errs, &os.PathError{Op: "stat", Path: filepath.Join(path, name), Err: err})
		case int(st.Uid) == uid:
			if err := r.removeAt(int(f.Fd()), name, filepath.Join(path, name), false); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// fstatat stats an entry of a shared place for the sweep. Tests set it
// to have a stat fail as no entry an ordinary account makes can.
var fstatat = unix.Fstatat

// openPlace opens the folder at path, an absolute path, through each
// folder above it, none of which may be a link. It returns nil, and no
// error, if the folder is gone, or it or a folder above it is a link or
// a file.
func openPlace(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%q isn't a clean absolute path", path)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: "/", Err: err}
	}
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		switch {
		case errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP):
			return nil, nil
		case err != nil:
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

// readView reads the folders whose entries the read check probes: the
// top of the file system, /Users and /Volumes, where it marks macOS's
// own system volumes (markSystemVolumes).
func readView() (exposure.View, error) {
	var v exposure.View
	var err error
	for _, read := range []struct {
		dir  string
		into *[]exposure.Entry
	}{{"/", &v.Top}, {"/Users", &v.Users}, {"/Volumes", &v.Volumes}} {
		if *read.into, err = entries(read.dir); err != nil {
			return v, err
		}
	}
	return v, markSystemVolumes("/Volumes", v.Volumes, apfsList)
}

// markSystemVolumes marks each entry of dir, /Volumes, vols, where one of
// macOS's own system volumes is mounted (exposure.SystemVolume): an entry
// that isn't a link, and that statfs finds is the mount point of an APFS
// volume, whose role and container list, which it reads only then,
// shows is one of them. Any other entry it leaves, so the read check
// probes it: one statfs can't read, one that isn't a mount point, or
// whatever's mounted there.
func markSystemVolumes(dir string, vols []exposure.Entry, list func() ([]byte, error)) error {
	devices := map[int]string{}
	for i, e := range vols {
		if e.Link {
			continue
		}
		p := filepath.Join(dir, e.Name)
		var st unix.Statfs_t
		if unix.Statfs(p, &st) != nil || unix.ByteSliceToString(st.Mntonname[:]) != p ||
			unix.ByteSliceToString(st.Fstypename[:]) != "apfs" {
			continue
		}
		if dev, ok := strings.CutPrefix(unix.ByteSliceToString(st.Mntfromname[:]), "/dev/"); ok {
			devices[i] = dev
		}
	}
	if len(devices) == 0 {
		return nil
	}
	var root unix.Statfs_t
	if err := unix.Statfs("/", &root); err != nil {
		return &os.PathError{Op: "statfs", Path: "/", Err: err}
	}
	boot, _ := strings.CutPrefix(unix.ByteSliceToString(root.Mntfromname[:]), "/dev/")
	b, err := list()
	if err != nil {
		return err
	}
	containers, err := exposure.ParseAPFSList(b)
	if err != nil {
		return err
	}
	for i, dev := range devices {
		vols[i].System = exposure.SystemVolume(dev, boot, containers)
	}
	return nil
}

// apfsList returns what diskutil apfs list -plist prints: the Mac's APFS
// containers and their volumes.
func apfsList() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandBound)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/sbin/diskutil", "apfs", "list", "-plist")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("diskutil apfs list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// entries returns the entries in dir, each with whether it's a link.
func entries(dir string) ([]exposure.Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var list []exposure.Entry
	for _, e := range des {
		list = append(list, exposure.Entry{Name: e.Name(), Link: e.Type()&os.ModeSymlink != 0})
	}
	return list, nil
}

// macWalkRoots returns the folders the survey walks, from the Mac's
// /usr/share/firmlinks.
func macWalkRoots() ([]string, error) {
	b, err := os.ReadFile("/usr/share/firmlinks")
	if err != nil {
		return nil, err
	}
	return exposure.WalkRoots(string(b))
}

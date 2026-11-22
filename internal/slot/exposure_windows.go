package slot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/meschoening/silkpool/internal/winaccount"
	"github.com/meschoening/silkpool/internal/winexposure"
	"github.com/meschoening/silkpool/internal/winslot"
)

// The exposure check and the shared-places sweep are in "Define how the
// agent runs and launches slots on each OS", ADRs 0005 and 0018, and "Use
// one Silkpool service account per machine instead of per-slot accounts"
// (#214). What they decide, and how they look, is in
// internal/winexposure.

// SurveyEvery is how often the shared places are worked out again: the
// agent surveys as it starts and daily (#19).
const SurveyEvery = 24 * time.Hour

// A failed survey is tried again once firstRetry has passed, and each
// time it fails again in a row, twice as long, up to lastRetry. A survey
// walks the whole system drive, so a machine where it keeps failing
// isn't walked again at every slot's preparation (#284). The agent's
// own survey, as it starts and every SurveyEvery, runs regardless.
const (
	firstRetry = 5 * time.Minute
	lastRetry  = 6 * time.Hour
)

// retryAfter is how long after the latest of failures surveys that
// failed in a row the next is due.
func retryAfter(failures int) time.Duration {
	wait := firstRetry
	for i := 1; i < failures && wait < lastRetry; i++ {
		wait *= 2
	}
	return min(wait, lastRetry)
}

// A survey is the latest survey, as Silkpool saves it: when it ran, the
// system drive it walked, what it found, or why it failed, and how many
// surveys in a row have failed with it.
type survey struct {
	Time     time.Time `json:"time"`
	System   string    `json:"system,omitempty"`
	Error    string    `json:"error,omitempty"`
	Failures int       `json:"failures,omitempty"`
	winexposure.Report
}

// surveyFile is the file in the agent's state that keeps the latest
// survey, which no slot may open.
func (l Layout) surveyFile() string { return filepath.Join(l.state(), winexposure.SurveyFile) }

// guardFile is the file in the agent's state that records each folder a
// survey guarded (winexposure.GuardFile), which the uninstall reads to
// take the guards away.
func (l Layout) guardFile() string { return filepath.Join(l.state(), winexposure.GuardFile) }

// Survey works out the machine's shared places: the folders outside
// Silkpool's own where the service account can create entries. It logs
// on as the account, as a slot host does for its listener (S4U, Batch,
// winslot.LogonToken), and walks, in the agent's own process, as
// LocalSystem acting as the account through that token, the whole
// system drive, but the homes in Users, the account's own profile and
// Silkpool's own folders, and each other fixed drive's root by itself
// (winexposure.Survey). It guards the shared places in the folders of
// the OS's software, recording each in the agent's state first (ADR
// 0019). It saves what it found, for Check, which counts those it
// couldn't guard, and for the sweep. A survey that fails is saved as
// failed, so the check and the sweep fail closed until one succeeds.
//
// It holds the slots' lock for reading, as a runner does, since the
// account-wide clean disables the account, which refuses its logon.
// Surveys take turns. The agent surveys as it starts and every
// SurveyEvery after; on the VM, #19's walk of the system drive took 80 s.
func (s *Slots) Survey(ctx context.Context) error {
	s.surveying.Lock()
	defer s.surveying.Unlock()
	return s.survey(ctx)
}

// survey is Survey, with the surveys' turn taken.
func (s *Slots) survey(ctx context.Context) error {
	s.use.RLock()
	defer s.use.RUnlock()
	sid, err := s.serviceSID()
	saved := survey{Time: s.now().UTC()}
	if err == nil {
		saved.System, saved.Report, err = s.surveyAs(ctx, sid)
	}
	if err != nil {
		// A failure's wait runs from when it failed: a survey may take
		// minutes.
		saved = survey{Time: s.now().UTC(), Error: err.Error(), Failures: s.failedSurveys() + 1, Report: winexposure.Report{SID: sid}}
	}
	return errors.Join(err, s.saveSurvey(saved))
}

func (s *Slots) surveyAs(ctx context.Context, sid string) (string, winexposure.Report, error) {
	v, own, a, done, err := s.lookAs(sid)
	if err != nil {
		return "", winexposure.Report{}, err
	}
	defer done()
	req := v.Request(own, s.Root, s.Logs)
	if s.scope != nil {
		req = winexposure.Request{Walk: s.scope, Skip: req.Skip}
	}
	note, closeLedger, err := s.ledger()
	if err != nil {
		return "", winexposure.Report{}, err
	}
	defer closeLedger()
	req.Note = note
	rep, err := winexposure.Survey(ctx, a, req)
	if err != nil {
		return "", winexposure.Report{}, fmt.Errorf("can't work out where the service account can write: %w", err)
	}
	return v.System, rep, nil
}

// ledger opens the guard ledger to append to it, and returns a Note
// that records a place in it, which the caller calls close after. Each
// line is on the disk before the place is guarded, so no guard
// outlives its record.
func (s *Slots) ledger() (note func(winexposure.Place) error, close func() error, err error) {
	f, err := openLedger(s.guardFile())
	if err != nil {
		return nil, nil, fmt.Errorf("can't open the record of what Silkpool guards: %w", err)
	}
	note = func(p winexposure.Place) error {
		if _, err := f.Write(winexposure.GuardLine(p)); err != nil {
			return fmt.Errorf("can't record what Silkpool guards: %w", err)
		}
		if err := f.Sync(); err != nil {
			return fmt.Errorf("can't record what Silkpool guards: %w", err)
		}
		return nil
	}
	return note, f.Close, nil
}

// openLedger opens the guard ledger at path to append to it. A last line
// an agent cut short it ends first, so the next line is whole.
func openLedger(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err = f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			_, err = f.Write([]byte("\n"))
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// lookAs reads what the check and the survey look at, and logs on as
// the account sid, for them to act as. done closes its token.
func (s *Slots) lookAs(sid string) (v winexposure.View, own string, a winexposure.Account, done func(), err error) {
	if v, err = s.readView(); err != nil {
		return v, "", a, nil, fmt.Errorf("can't read what the service account may see: %w", err)
	}
	if own, err = s.profileOf(sid); err != nil {
		return v, "", a, nil, fmt.Errorf("can't read where the service account's profile is: %w", err)
	}
	if a.SID, err = windows.StringToSid(sid); err != nil {
		return v, "", a, nil, fmt.Errorf("the service account's SID %q: %w", sid, err)
	}
	logon, err := s.logOn(s.Account.Name, sid)
	if err != nil {
		return v, "", a, nil, fmt.Errorf("can't log on as the service account: %w", err)
	}
	// It acts as the token a job runs as: the account's, without
	// bypassing traverse checking (ADR 0020).
	a.Token, err = winslot.WithoutTraverseBypass(logon)
	logon.Close()
	if err != nil {
		return v, "", a, nil, fmt.Errorf("can't act as the service account: %w", err)
	}
	return v, own, a, func() { a.Token.Close() }, nil
}

// serviceSID reads the service account's SID, as the agent recorded it
// (winaccount.Keep).
func (s *Slots) serviceSID() (string, error) {
	sid, err := winaccount.ReadRecord(s.accountRecord())
	if err == nil && sid == "" {
		err = errors.New("no SID is recorded for it")
	}
	if err != nil {
		return "", fmt.Errorf("can't read the service account: %w", err)
	}
	return sid, nil
}

// failedSurveys is how many surveys in a row the latest survey saved
// ends, or 0 if it succeeded or can't be read.
func (s *Slots) failedSurveys() int {
	var sv survey
	b, err := os.ReadFile(s.surveyFile())
	if err != nil || json.Unmarshal(b, &sv) != nil || sv.Error == "" {
		return 0
	}
	return max(sv.Failures, 1)
}

// saveSurvey saves sv as the latest survey.
func (s *Slots) saveSurvey(sv survey) error {
	b, err := json.Marshal(sv)
	if err == nil {
		err = writeRecord(s.surveyFile(), string(b))
	}
	if err != nil {
		return fmt.Errorf("can't save the shared places: %w", err)
	}
	return nil
}

// errNoSurvey is a machine whose shared places haven't been worked out.
var errNoSurvey = errors.New("the shared places haven't been worked out yet, so nothing shows where the service account can write outside its own folders")

// readSurvey reads the latest survey, which must have succeeded, of the
// account sid.
func (s *Slots) readSurvey(sid string) (survey, error) {
	var sv survey
	b, err := os.ReadFile(s.surveyFile())
	if errors.Is(err, fs.ErrNotExist) {
		return sv, errNoSurvey
	}
	if err == nil {
		err = json.Unmarshal(b, &sv)
	}
	switch {
	case err != nil:
		return sv, fmt.Errorf("can't read the shared places: %w", err)
	case sv.Error != "":
		return sv, fmt.Errorf("the last survey couldn't work out where the service account can write outside its own folders: %s", sv.Error)
	case sv.SID != sid:
		return sv, fmt.Errorf("the shared places were worked out for %s, not the service account, %s", sv.SID, sid)
	}
	return sv, nil
}

// Check runs the machine's exposure check as the service account, in
// the runner's context: it logs on as the account as a slot host does,
// and looks, in the agent's own process, as LocalSystem acting as the
// account through that token, at each entry at the top of the system
// drive outside the OS folders, each home in Users but the account's
// own profile, each entry at the top of another fixed drive but its OS
// folders, and Silkpool's state and logs (winexposure.View). What the
// account can read, but Silkpool's own, it guards first, and the
// survey's places in the folders of the OS's software it guards again
// (guard, ADR 0020); then it counts what the account can still read, and
// the folders of the OS's software it can still create entries in, or
// that the survey couldn't look inside.
//
// It returns nil if the account can see and change none of them; a
// *winexposure.Exposed naming the first it found; or another error, when
// the survey hasn't succeeded, was of another account, or the check
// can't run, as when the account can't log on. Each fails every slot
// closed. It holds the slots' lock for reading, as Survey does.
func (s *Slots) Check(ctx context.Context) error {
	s.use.RLock()
	defer s.use.RUnlock()
	sid, err := s.serviceSID()
	if err != nil {
		return err
	}
	sv, err := s.readSurvey(sid)
	if err != nil {
		return err
	}
	v, own, a, done, err := s.lookAs(sid)
	if err != nil {
		return fmt.Errorf("the exposure check couldn't run: %w", err)
	}
	defer done()
	visible, unseen, err := winexposure.Look(ctx, a, v.Probes(own, s.state(), s.Logs))
	if err != nil {
		return fmt.Errorf("the exposure check couldn't run: %w", err)
	}
	rep, err := s.guard(ctx, a, v.System, own, visible, sv.Report)
	if err != nil {
		return fmt.Errorf("the exposure check couldn't run: %w", err)
	}
	if len(visible) > 0 {
		// It looks again at what's there now, so an entry made while it
		// guarded is looked at too.
		if v, err = s.readView(); err == nil {
			visible, unseen, err = winexposure.Look(ctx, a, v.Probes(own, s.state(), s.Logs))
		}
		if err != nil {
			return fmt.Errorf("the exposure check couldn't run: %w", err)
		}
	}
	if hits := winexposure.Hits(v.System, visible, unseen, rep); len(hits) > 0 {
		return &winexposure.Exposed{Detail: winexposure.Detail(hits), Hits: hits}
	}
	return nil
}

// guard denies the account a, recording each first in the guard ledger,
// what the check found it could read (winexposure.GuardReads), but
// what holds Silkpool's own folders or the account's profile, own, which
// it needs, and which stays a hit; and, again, each shared place in the
// folders of the OS's software that the latest survey, rep, found, or
// that a survey guarded, topmost, as the ledger records them, since a
// program may have set its DACL again (winexposure.GuardPlaces). It
// returns rep with the places that are guarded now left out (ADR 0020).
func (s *Slots) guard(ctx context.Context, a winexposure.Account, system, own string, visible []string, rep winexposure.Report) (winexposure.Report, error) {
	needed := []string{s.Root, s.state(), s.Logs, own}
	reads := slices.DeleteFunc(slices.Clone(visible), func(p string) bool {
		return slices.ContainsFunc(needed, func(n string) bool { return n != "" && winexposure.Under(canonical(n), canonical(p)) })
	})
	inChange := func(p winexposure.Place) bool {
		return slices.ContainsFunc(winexposure.ChangeRoots(system), func(r string) bool { return winexposure.Under(p.Path, r) })
	}
	var changes, others []winexposure.Place
	for _, p := range rep.Places {
		if inChange(p) {
			changes = append(changes, p)
		} else {
			others = append(others, p)
		}
	}
	tops, err := s.guardedTops()
	if err != nil {
		return rep, err
	}
	var once []winexposure.Place
	for _, p := range tops {
		if inChange(p) && !slices.ContainsFunc(changes, func(c winexposure.Place) bool { return c.Volume == p.Volume && c.ID == p.ID }) {
			once = append(once, p)
		}
	}
	if len(reads) == 0 && len(changes) == 0 && len(once) == 0 {
		return rep, nil
	}
	note, closeLedger, err := s.ledger()
	if err != nil {
		return rep, err
	}
	defer closeLedger()
	if err := winexposure.GuardReads(ctx, a, reads, note); err != nil {
		return rep, err
	}
	still, err := winexposure.GuardPlaces(ctx, a, changes, false, note)
	if err != nil {
		return rep, err
	}
	again, err := winexposure.GuardPlaces(ctx, a, once, true, note)
	if err != nil {
		return rep, err
	}
	rep.Places = slices.Concat(others, still, again)
	return rep, nil
}

// canonical is path as the read check names it: without the \\?\
// prefix that takes a path as it is, cleaned, and with each short name
// in it long, as Windows names a folder it lists, if it's there.
func canonical(path string) string {
	path = filepath.Clean(strings.TrimPrefix(path, `\\?\`))
	p, err := windows.UTF16FromString(`\\?\` + path)
	if err != nil {
		return path
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if n, err := windows.GetLongPathName(&p[0], &buf[0], uint32(len(buf))); err == nil && n > 0 && int(n) < len(buf) {
		return filepath.Clean(strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`))
	}
	return path
}

// guardedTops returns the folders the guard ledger records that no other
// it records is above: those a program may set the DACL of again, which
// the folders below keep their own guards through.
func (s *Slots) guardedTops() ([]winexposure.Place, error) {
	b, err := os.ReadFile(s.guardFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("can't read the record of what Silkpool guards: %w", err)
	}
	all := winexposure.ParseGuarded(b)
	return slices.DeleteFunc(slices.Clone(all), func(p winexposure.Place) bool {
		return slices.ContainsFunc(all, func(q winexposure.Place) bool {
			return !strings.EqualFold(q.Path, p.Path) && winexposure.Under(p.Path, q.Path)
		})
	}), nil
}

// Exposure is the exposure check a slot's preparation runs (Check). A
// survey takes a minute or more, so it never runs one itself: without a
// survey that succeeded, as just after the account is made, it starts
// one in the background, and fails closed until it's done. With one
// older than SurveyEvery, as if the agent's daily survey stopped, it
// starts one too, and checks against the latest meanwhile. After a
// survey that failed, it starts one once its wait is over (retryAfter),
// and fails closed, saying when.
func (s *Slots) Exposure(ctx context.Context) error {
	sid, err := s.serviceSID()
	if err != nil {
		return err
	}
	sv, err := s.readSurvey(sid)
	if sv.Error != "" && !s.due(sv, err) {
		return fmt.Errorf("%w; Silkpool works them out again at %s", err, sv.Time.Add(retryAfter(sv.Failures)).Local().Format("15:04"))
	}
	if s.due(sv, err) {
		s.surveyLater()
	}
	switch {
	case errors.Is(err, errNoSurvey):
		return errors.New("the shared places, where the service account can write outside its own folders, are being worked out, which takes a few minutes, and a slot can start once they are")
	case err != nil:
		return fmt.Errorf("%w; Silkpool is working them out again", err)
	}
	return s.Check(ctx)
}

// due reports whether a check should start a survey, given the latest,
// sv, as readSurvey read it with err: when none has succeeded, but a
// survey that failed only once its wait is over (retryAfter), or when
// the latest is SurveyEvery old.
func (s *Slots) due(sv survey, err error) bool {
	switch {
	case sv.Error != "":
		return !s.now().Before(sv.Time.Add(retryAfter(sv.Failures)))
	case err != nil:
		return true
	}
	return s.now().Sub(sv.Time) >= SurveyEvery
}

// surveyLater starts a survey in the background, unless one started so
// is still running. Once it's its turn, it surveys only if a survey is
// still due: the agent's own may have run meanwhile, and failed.
func (s *Slots) surveyLater() {
	if !s.resurveying.CompareAndSwap(false, true) {
		return
	}
	s.surveys.Go(func() {
		defer s.resurveying.Store(false)
		s.surveying.Lock()
		defer s.surveying.Unlock()
		if sid, err := s.serviceSID(); err == nil {
			if sv, err := s.readSurvey(sid); !s.due(sv, err) {
				return
			}
		}
		// A failure is saved, and the next check names it.
		s.survey(context.Background())
	})
}

// sweep empties the shared places, as the latest survey of the account
// sid found them, of what the account owns (winexposure.Sweep). It runs
// in the account-wide clean, once every slot is idle and nothing of the
// account runs, so what it deletes is no running job's. Without a survey
// that succeeded, of this account, it fails, so the clean fails closed.
func (s *Slots) sweep(ctx context.Context, sid string) error {
	sv, err := s.readSurvey(sid)
	if err != nil {
		return err
	}
	return s.machine.Sweep(ctx, sid, sv.Places)
}

// machineView reads the folders whose entries the read check looks at:
// the system drive's top and its Users folder, and each other fixed
// drive's top. A fixed drive that isn't ready, as one BitLocker keeps
// locked, has nothing anyone can read.
func machineView() (winexposure.View, error) {
	win, err := windows.GetWindowsDirectory()
	if err != nil {
		return winexposure.View{}, err
	}
	v := winexposure.View{System: filepath.VolumeName(win) + `\`}
	if v.Top, err = entries(v.System); err != nil {
		return v, err
	}
	if v.Users, err = entries(v.UsersFolder()); err != nil {
		return v, err
	}
	roots, err := fixedDrives()
	if err != nil {
		return v, err
	}
	for _, root := range roots {
		if root == v.System {
			continue
		}
		top, err := entries(root)
		if errors.Is(err, windows.ERROR_NOT_READY) {
			continue
		}
		if err != nil {
			return v, err
		}
		v.Drives = append(v.Drives, winexposure.Drive{Root: root, Top: top})
	}
	return v, nil
}

// fixedDrives returns the roots of the machine's fixed drives, such as
// C:\ and D:\.
func fixedDrives() ([]string, error) {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	var roots []string
	for i := range 26 {
		if drives&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			return nil, err
		}
		if windows.GetDriveType(p) == windows.DRIVE_FIXED {
			roots = append(roots, root)
		}
	}
	return roots, nil
}

// entries returns the entries in dir, each with whether it's a reparse
// point.
func entries(dir string) ([]winexposure.Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var list []winexposure.Entry
	for _, e := range des {
		list = append(list, winexposure.Entry{Name: e.Name(), Link: e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0})
	}
	return list, nil
}

// profileList is the registry key whose subkeys record each profile, by
// its account's SID.
const profileList = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`

// profileFolder returns where sid's profile is, as Windows records it,
// or "" if it has none.
func profileFolder(sid string) (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileList+`\`+sid, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer k.Close()
	p, _, err := k.GetStringValue("ProfileImagePath")
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return registry.ExpandString(p)
}

// Sweep empties the shared places of what sid owns (winexposure.Sweep).
func (local) Sweep(ctx context.Context, sid string, places []winexposure.Place) error {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return err
	}
	return winexposure.Sweep(ctx, s, places)
}

// readView reads what the read check looks at: the machine's
// (machineView), unless a test gave its own.
func (s *Slots) readView() (winexposure.View, error) {
	if s.view != nil {
		return s.view()
	}
	return machineView()
}

// logOn logs on as the service account, as a slot host does
// (winslot.LogonToken), unless a test gave its own logon.
func (s *Slots) logOn(account, sid string) (windows.Token, error) {
	if s.logon != nil {
		return s.logon(account, sid)
	}
	return winslot.LogonToken(account, sid)
}

// profileOf returns where sid's profile is (profileFolder), unless a
// test said where.
func (s *Slots) profileOf(sid string) (string, error) {
	if s.profile != nil {
		return s.profile(sid)
	}
	return profileFolder(sid)
}

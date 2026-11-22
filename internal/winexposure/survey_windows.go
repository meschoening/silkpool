package winexposure

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/secret"
)

// maxFound is the most shared places, or folders it can't look inside, a
// survey reports. The account's token may write only where the account
// itself, or its logon session, is granted it (ADR 0019), so a machine
// with more has been given to the account wholesale, which no sweep
// should act on; and every place is kept in the agent's memory and its
// state. A survey that finds more walks on, counting, to say where they
// are, and fails.
var maxFound = 20000

// Survey works out the shared places as a, in the agent's own process:
// the folders the account can create entries in, and doesn't own. It
// walks each of req.Walk, through every folder below it on its volume,
// and looks at each of req.Shallow alone, but each folder in req.Skip
// and what's below it.
//
// It lists and opens each folder as LocalSystem, with SeBackupPrivilege
// enabled on its own thread, so it reaches every folder, the account's
// or not, and never follows a reparse point: it opens each folder by its
// name in its parent's handle, and leaves a junction, a link or a mount
// point there as it is. What the account may do to each folder it asks
// Windows, opening that very folder again as the account. A folder the
// account owns it leaves, and doesn't walk: the sweep deletes it whole,
// from the shared place it's in. A folder even LocalSystem can't look
// inside it reports as unwalked (ADR 0018). A place in one of req.Guard
// it guards, telling req.Note first: it denies the account's token
// every write there, and asks Windows again (ADR 0019). It fails on an
// error that isn't a folder that's gone or can't be opened, or, once it
// has walked everything, if it found more than maxFound of either,
// naming the folders that hold the most.
func Survey(ctx context.Context, a Account, req Request) (Report, error) {
	rep := Report{SID: a.SID.String()}
	var user *windows.SID
	if len(req.Guard) > 0 {
		if req.Note == nil {
			return rep, errors.New("a survey that guards must record what it guards")
		}
		var err error
		if user, err = tokenUser(a.Token); err != nil {
			return rep, err
		}
	}
	err := onThread(a.Token, func(t *thread) error {
		w := walker{ctx: ctx, t: t, a: a, req: req, rep: &rep, user: user}
		for _, root := range req.Walk {
			if err := w.root(root, true); err != nil {
				return err
			}
		}
		for _, root := range req.Shallow {
			if err := w.root(root, false); err != nil {
				return err
			}
		}
		return errors.Join(w.shared.tooMany("folders the account can create entries in", "a sweep may act on"),
			w.unseen.tooMany("folders Silkpool can't look inside", "it may leave unwalked"))
	})
	if err != nil {
		return Report{SID: rep.SID}, err
	}
	return rep, nil
}

type walker struct {
	ctx context.Context
	t   *thread
	a   Account
	req Request
	rep *Report
	// user is the SID of the account's token, whom a guard denies.
	user *windows.SID
	// volume is the volume of the root being walked, and top its path.
	volume uint64
	top    string
	// shared and unseen count the places and unwalked folders it found.
	shared, unseen tally
}

// A tally counts what a walk found, by where: the folder two below the
// root it walked, or the root itself.
type tally struct {
	n     int
	where map[string]int
}

// add counts path, found below top, and reports whether it's still
// within maxFound.
func (c *tally) add(top, path string) bool {
	c.n++
	if c.where == nil {
		c.where = map[string]int{}
	}
	c.where[within(top, path, 2)]++
	return c.n <= maxFound
}

// tooMany is nil if the walk found no more than maxFound, or says how
// many what it found, and where most are, more than the most it may.
func (c *tally) tooMany(what, may string) error {
	if c.n <= maxFound {
		return nil
	}
	where := slices.Collect(maps.Keys(c.where))
	slices.SortFunc(where, func(a, b string) int {
		return cmp.Or(cmp.Compare(c.where[b], c.where[a]), strings.Compare(strings.ToLower(a), strings.ToLower(b)))
	})
	var most []string
	for _, p := range where[:min(3, len(where))] {
		most = append(most, fmt.Sprintf("%s (%d)", secret.Scrub(p), c.where[p]))
	}
	return fmt.Errorf("the walk found %d %s, more than the %d %s; most are in %s", c.n, what, maxFound, may, strings.Join(most, ", "))
}

// within returns the folder levels below top that path is in, or path
// itself if it's no deeper, or not below top.
func within(top, path string, levels int) string {
	if top == "" || !Under(path, top) {
		return path
	}
	rest := strings.TrimPrefix(path[len(top):], `\`)
	if rest == "" {
		return path
	}
	parts := strings.SplitN(rest, `\`, levels+1)
	return Join(top, strings.Join(parts[:min(levels, len(parts))], `\`))
}

// root walks the folder path, through what's below it if deep.
func (w *walker) root(path string, deep bool) error {
	if w.skipped(path) {
		return nil
	}
	h, err := openPath(path, windows.FILE_READ_ATTRIBUTES)
	switch {
	case gone(err):
		return nil
	case blocked(err):
		return w.unwalked(path)
	case err != nil:
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	id, err := identify(h)
	if err != nil {
		return &os.PathError{Op: "read", Path: path, Err: err}
	}
	if !id.folder() {
		return nil
	}
	w.volume, w.top = id.volume, path
	return w.folder(h, id, path, deep)
}

// folder looks at the folder h is open on, at path, and walks what's
// below it if deep.
func (w *walker) folder(h windows.Handle, id identity, path string, deep bool) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	granted, err := w.t.granted(h)
	switch {
	case gone(err):
		return nil
	case blocked(err):
		return w.unwalked(path)
	case err != nil:
		return &os.PathError{Op: "check the account's access to", Path: path, Err: err}
	}
	l, err := reopen(h, windows.FILE_LIST_DIRECTORY|windows.READ_CONTROL,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT)
	switch {
	case gone(err):
		return nil
	case blocked(err):
		return w.unwalked(path)
	case err != nil:
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(l), path)
	defer f.Close()
	owner, err := ownerOf(l)
	if err != nil {
		return &os.PathError{Op: "read the owner of", Path: path, Err: err}
	}
	if owner != nil && owner.Equals(w.a.SID) {
		return nil
	}
	if granted&creates != 0 && w.guards(path) {
		if granted, err = w.guard(h, l, id, path, granted); err != nil {
			return err
		}
	}
	if granted&creates != 0 {
		p := Place{Path: path, DeleteChild: granted&fileDeleteChild != 0, Volume: id.volume, ID: id.id}
		if owner != nil {
			p.Owner = owner.String()
		}
		if w.shared.add(w.top, path) {
			w.rep.Places = append(w.rep.Places, p)
		}
	}
	if !deep {
		return nil
	}
	entries, err := f.ReadDir(-1)
	switch {
	case gone(err):
		return nil
	case blocked(err):
		return w.unwalked(path)
	case err != nil:
		return &os.PathError{Op: "list", Path: path, Err: err}
	}
	w.rep.Listed++
	for _, e := range entries {
		// A reparse point Go reports as such isn't looked at; one it
		// reports as a folder is caught below, by its attributes.
		if !e.IsDir() {
			continue
		}
		if err := w.child(l, Join(path, e.Name()), e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// child walks the folder name in the folder dir, at path.
func (w *walker) child(dir windows.Handle, path, name string) error {
	if w.skipped(path) {
		return nil
	}
	h, err := openAt(dir, name, windows.FILE_READ_ATTRIBUTES, windows.FILE_DIRECTORY_FILE)
	switch {
	case gone(err):
		return nil
	case blocked(err):
		return w.unwalked(path)
	case err != nil:
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	id, err := identify(h)
	if err != nil {
		return &os.PathError{Op: "read", Path: path, Err: err}
	}
	if !id.folder() || id.volume != w.volume {
		return nil
	}
	return w.folder(h, id, path, true)
}

// guards reports whether path is in a folder the request guards.
func (w *walker) guards(path string) bool {
	return slices.ContainsFunc(w.req.Guard, func(g string) bool { return Under(path, g) })
}

// guard guards the shared place at path, open as h and l, which the
// account may do granted to, and returns what it may do then. It tells
// the request's Note first, so a guard is never put without a record of
// it. A folder that it can't guard, as one that has no DACL, it leaves,
// as a place, and so a hit.
func (w *walker) guard(h, l windows.Handle, id identity, path string, granted uint32) (uint32, error) {
	if w.req.Note != nil {
		if err := w.req.Note(Place{Path: path, Volume: id.volume, ID: id.id}); err != nil {
			return 0, err
		}
	}
	g, err := reopen(l, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_DIRECTORY_FILE)
	switch {
	case gone(err), blocked(err):
		return granted, nil
	case err != nil:
		return 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	err = put(g, w.user, writeGuard)
	windows.CloseHandle(g)
	switch {
	case errors.Is(err, errNoDACL), errors.Is(err, errBadACL):
		return granted, nil
	case err != nil:
		return 0, &os.PathError{Op: "deny the account changes to", Path: path, Err: err}
	}
	now, err := w.t.granted(h)
	switch {
	case gone(err), blocked(err):
		return granted, nil
	case err != nil:
		return 0, &os.PathError{Op: "check the account's access to", Path: path, Err: err}
	}
	if now&creates == 0 {
		w.rep.Guarded++
	}
	return now, nil
}

// skipped reports whether path is in a folder the request skips.
func (w *walker) skipped(path string) bool {
	return slices.ContainsFunc(w.req.Skip, func(s string) bool { return Under(path, s) })
}

// unwalked notes that the walk couldn't look inside path.
func (w *walker) unwalked(path string) error {
	if w.unseen.add(w.top, path) {
		w.rep.Unwalked = append(w.rep.Unwalked, path)
	}
	return nil
}

// Look runs the read check as a, in the agent's own process: for each of
// paths, it opens what's there as LocalSystem, never following a
// reparse point at its end, and asks Windows whether the account may
// read it, opening that very file or folder again as the account. It
// returns those the account may read or list, and those it couldn't look
// at, such as a file held open with no sharing, whose open as the
// account fails as a sharing violation. A path that's missing, or a
// reparse point, the account sees nothing of.
func Look(ctx context.Context, a Account, paths []string) (visible, unseen []string, err error) {
	err = onThread(a.Token, func(t *thread) error {
		for _, p := range paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			h, err := openPath(p, windows.FILE_READ_ATTRIBUTES)
			switch {
			case gone(err):
				continue
			case blocked(err):
				unseen = append(unseen, p)
				continue
			case err != nil:
				return &os.PathError{Op: "open", Path: p, Err: err}
			}
			see, err := readable(t, h)
			windows.CloseHandle(h)
			switch {
			case gone(err):
			case blocked(err):
				unseen = append(unseen, p)
			case err != nil:
				return &os.PathError{Op: "check the account's access to", Path: p, Err: err}
			case see:
				visible = append(visible, p)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return visible, unseen, nil
}

// readable reports whether the account may read, or list, what h is open
// on, unless it's a reparse point.
func readable(t *thread, h windows.Handle) (bool, error) {
	id, err := identify(h)
	if err != nil || id.reparse() {
		return false, err
	}
	granted, err := t.granted(h)
	return granted&windows.FILE_READ_DATA != 0, err
}

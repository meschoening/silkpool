package roleaccount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// A Machine is what holds IDs on a Mac: its directories, its processes,
// and the folders accounts leave behind them.
type Machine struct {
	Directory
	// Processes lists every running process. It fails unless the list
	// holds this process, so a listing cut short can't pass for fewer.
	Processes func(ctx context.Context) ([]Process, error)
	// Leftovers lists what an account deleted earlier may have left with
	// its IDs: on the Mac, the /var/folders folders SIP keeps for it, and
	// the folders in /Users. It fails unless it read them whole.
	Leftovers func() ([]Owned, error)
}

// A Process is a running process: its PID, and every UID and GID it
// holds as its effective, real or saved one, any of which it can switch
// back to without root.
type Process struct {
	PID  int
	UIDs []int
	GIDs []int
}

// An Owned is a path, with its owner and group.
type Owned struct {
	Path     string
	UID, GID int
}

// A Use is what holds an ID: a user or a group, by its name, a process,
// by its PID, or a folder, by its path.
type Use struct {
	What, Name string
}

func (u Use) String() string { return u.What + " " + u.Name }

// Taken returns what holds each ID in use as a UID or a GID: a record in
// the local directory or any the search policy holds, as a user's
// UniqueID or PrimaryGroupID or a group's PrimaryGroupID; a process; or
// something an account deleted earlier left behind. Role accounts take
// the same number for both, so a number is taken if anything holds it as
// either.
func (m Machine) Taken(ctx context.Context) (map[int][]Use, error) {
	taken := map[int][]Use{}
	add := func(id int, u Use) {
		if !slices.Contains(taken[id], u) {
			taken[id] = append(taken[id], u)
		}
	}
	for _, node := range []string{"/Search", "."} {
		users, err := m.All(ctx, node, Users, "UniqueID", "PrimaryGroupID")
		if err != nil {
			return nil, err
		}
		groups, err := m.All(ctx, node, Groups, "PrimaryGroupID")
		if err != nil {
			return nil, err
		}
		for kind, rs := range map[string][]Record{"user": users, "group": groups} {
			for _, r := range rs {
				for _, attr := range []string{"UniqueID", "PrimaryGroupID"} {
					for _, v := range r[attr] {
						n, err := strconv.Atoi(v)
						if err != nil {
							return nil, fmt.Errorf("%s %s's %s is %q", kind, r["RecordName"][0], attr, v)
						}
						add(n, Use{kind, r["RecordName"][0]})
					}
				}
			}
		}
	}
	procs, err := m.Processes(ctx)
	if err != nil {
		return nil, err
	}
	self := false
	for _, p := range procs {
		self = self || p.PID == os.Getpid()
		for _, n := range slices.Concat(p.UIDs, p.GIDs) {
			add(n, Use{"process", strconv.Itoa(p.PID)})
		}
	}
	if !self {
		return nil, fmt.Errorf("the processes listed don't include this one, %d, so they can't have been listed whole", os.Getpid())
	}
	left, err := m.Leftovers()
	if err != nil {
		return nil, err
	}
	if len(left) == 0 {
		return nil, errors.New("no folder an account keeps was found, so they can't have been read whole")
	}
	for _, o := range left {
		add(o.UID, Use{"folder", o.Path})
		add(o.GID, Use{"folder", o.Path})
	}
	return taken, nil
}

// parseProcesses reads ps's lines of n numbers: three UIDs, three GIDs
// and the PID.
func parseProcesses(out string, n int) ([]Process, error) {
	var procs []Process
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != n {
			return nil, fmt.Errorf("ps -A printed %q", line)
		}
		ids := make([]int, n)
		for i, v := range f {
			id, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("ps -A printed %q", line)
			}
			ids[i] = id
		}
		procs = append(procs, Process{PID: ids[6], UIDs: ids[0:3], GIDs: ids[3:6]})
	}
	return procs, nil
}

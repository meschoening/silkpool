package hubops

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/github"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/node"
)

// Targets are written as "Name the CLI and define its commands" has it:
// owner/repo for a repository, acme for an organization's Default runner
// group, and acme:builders for another of its groups. GitHub account
// names can't hold a colon, so the forms never collide. The Hub keys
// each target by its written form, as GitHub names it, and keeps a
// repository's ID and a group's, so a rename on GitHub doesn't lose it.

// defaultGroup is the ID of every organization's Default runner group.
const defaultGroup = 1

// A TargetKind is what a target is on GitHub.
type TargetKind string

// The kinds of target.
const (
	Repository   TargetKind = "repository"
	Organization TargetKind = "organization"
)

// A Target is a target as target list shows it.
type Target struct {
	// Target is the target as it's written, which other commands take.
	Target     string     `json:"target"`
	Kind       TargetKind `json:"kind"`
	Connection string     `json:"connection"`
	// Default reports that new nodes get it.
	Default bool      `json:"default,omitempty"`
	Added   time.Time `json:"added"`
	// Lost is when it became a lost target, or zero while the Hub can see
	// it on GitHub.
	Lost time.Time `json:"lost,omitzero"`
	// Nodes are the names of the nodes that use it, and RunsOn a runs-on
	// line for each OS and architecture among them.
	Nodes  []string `json:"nodes,omitempty"`
	RunsOn []string `json:"runs_on,omitempty"`
	// Group is an organization target's runner group, as GitHub reads it
	// now, or nil, with why in GroupFailed, if GitHub didn't answer.
	Group       *Group `json:"group,omitempty"`
	GroupFailed string `json:"group_failed,omitempty"`
}

// A Group is a runner group and what it serves: "all" of the
// organization's repositories, all its "private" ones, or the "selected"
// Repositories.
type Group struct {
	Name         string   `json:"name"`
	Visibility   string   `json:"visibility"`
	Repositories []string `json:"repositories,omitempty"`
}

// TargetList is target list's reply.
type TargetList struct {
	Targets   []Target `json:"targets"`
	PoolLabel string   `json:"pool_label"`
}

// A TargetChange is a target after a change, and whether it changed.
type TargetChange struct {
	Target  Target `json:"target"`
	Changed bool   `json:"changed"`
}

// A Candidate is a target a connection offers: a repository of a
// personal connection's installation, or a runner group of an
// organization's.
type Candidate struct {
	Target     string     `json:"target"`
	Kind       TargetKind `json:"kind"`
	Connection string     `json:"connection"`
	// Refused is the code of the rule that keeps it from being a target,
	// and Reason the rule in words, or both are empty.
	Refused string `json:"refused,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Added reports that it's a target already.
	Added bool   `json:"added,omitempty"`
	Group *Group `json:"group,omitempty"`
}

// Available is target available's reply: every connection's candidates,
// and the connections GitHub didn't list them for.
type Available struct {
	Candidates []Candidate `json:"candidates"`
	Failed     []Failed    `json:"failed,omitempty"`
}

// Failed is a connection whose candidates GitHub didn't list, and why.
type Failed struct {
	Connection string `json:"connection"`
	Message    string `json:"message"`
}

// Targets lists the Hub's targets, in the order they were added. An
// organization target's group is read from GitHub.
func (o *Ops) Targets(ctx context.Context) (TargetList, error) {
	h, err := o.state(ctx)
	if err != nil {
		return TargetList{}, err
	}
	out := TargetList{Targets: []Target{}, PoolLabel: h.Settings.PoolLabel}
	for _, t := range h.Targets {
		tg := targetOf(h, t)
		if tg.Kind == Organization {
			c, _ := connection(h, t.Connection)
			if g, err := o.c.GitHub.Group(ctx, c, t.GitHub); err != nil {
				tg.GroupFailed = fmt.Sprintf("GitHub didn't read the runner group: %v", err)
			} else {
				tg.Group = groupOf(g)
			}
		}
		out.Targets = append(out.Targets, tg)
	}
	return out, nil
}

// Available lists what every connection offers as a target, each with
// the rule that refuses it, if one does: a personal connection's
// installed repositories by name, and an organization's runner groups.
func (o *Ops) Available(ctx context.Context) (Available, error) {
	h, err := o.state(ctx)
	if err != nil {
		return Available{}, err
	}
	out := Available{Candidates: []Candidate{}}
	for _, c := range h.Connections {
		cands, err := o.candidates(ctx, h, c)
		if err != nil {
			out.Failed = append(out.Failed, Failed{Connection: c.Account, Message: err.Error()})
			continue
		}
		out.Candidates = append(out.Candidates, cands...)
	}
	return out, nil
}

// candidates returns what c offers.
func (o *Ops) candidates(ctx context.Context, h hubstore.Hub, c hubstore.Connection) ([]Candidate, error) {
	var out []Candidate
	add := func(cand Candidate, refusal *Refusal) {
		_, err := findTarget(h, cand.Target)
		cand.Added = err == nil
		if refusal != nil {
			cand.Refused, cand.Reason = refusal.Code, refusal.Message
		}
		out = append(out, cand)
	}
	if c.Kind == hubstore.Personal {
		repos, err := o.c.GitHub.Repositories(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("GitHub didn't list the repositories of %s's installation: %w", c.Account, err)
		}
		slices.SortFunc(repos, func(a, b github.Repository) int {
			return cmp.Compare(strings.ToLower(a.FullName()), strings.ToLower(b.FullName()))
		})
		for _, r := range repos {
			add(Candidate{Target: r.FullName(), Kind: Repository, Connection: c.Account}, refusePublic(r))
		}
		return out, nil
	}
	groups, err := o.c.GitHub.RunnerGroups(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("GitHub didn't list %s's runner groups: %w", c.Account, err)
	}
	for _, g := range groups {
		add(Candidate{Target: groupKey(c, g), Kind: Organization, Connection: c.Account, Group: groupOf(g)}, refusePublicGroup(c, g))
	}
	return out, nil
}

// AddTarget adds the target written as written, once GitHub shows that
// Silkpool may serve it: a private repository in its connection's
// installation, or a runner group that doesn't allow public
// repositories. The first target added is the default.
func (o *Ops) AddTarget(ctx context.Context, from node.Origin, written string) (TargetChange, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return TargetChange{}, err
	}
	t, err := o.resolve(ctx, h, written)
	if err != nil {
		return TargetChange{}, err
	}
	// Adding a target finishes setup's target step, if it's next.
	if held, err := findTarget(h, t.Key); err == nil {
		_, err := o.changeSettings(ctx, from, advance(hubstore.StepEnable))
		return TargetChange{Target: targetOf(h, held)}, err
	}
	t.Added = o.c.Now().UTC()
	if err := o.apply(ctx, hubstore.PutTarget{Target: t, Origin: from}); err != nil {
		return TargetChange{}, err
	}
	if _, err := o.changeSettings(ctx, from, advance(hubstore.StepEnable)); err != nil {
		return TargetChange{}, err
	}
	return o.targetChange(ctx, t.Key)
}

// RemoveTarget removes the target written as written. It's refused while
// any node uses it.
func (o *Ops) RemoveTarget(ctx context.Context, from node.Origin, written string) (TargetChange, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return TargetChange{}, err
	}
	t, err := findTarget(h, written)
	if err != nil {
		return TargetChange{}, err
	}
	if err := o.apply(ctx, hubstore.DropTarget{Key: t.Key, Origin: from}); err != nil {
		return TargetChange{}, err
	}
	return TargetChange{Target: targetOf(h, t), Changed: true}, nil
}

// SetDefaultTarget makes the target written as written the one new
// nodes get.
func (o *Ops) SetDefaultTarget(ctx context.Context, from node.Origin, written string) (TargetChange, error) {
	o.changing.Lock()
	defer o.changing.Unlock()
	h, err := o.state(ctx)
	if err != nil {
		return TargetChange{}, err
	}
	t, err := findTarget(h, written)
	if err != nil {
		return TargetChange{}, err
	}
	changed, err := o.changeSettings(ctx, from, func(s *hubstore.Settings) error {
		s.DefaultTarget = t.Key
		return nil
	})
	if err != nil {
		return TargetChange{}, err
	}
	c, err := o.targetChange(ctx, t.Key)
	c.Changed = changed
	return c, err
}

// targetChange returns the target key names, as it is now, changed.
func (o *Ops) targetChange(ctx context.Context, key string) (TargetChange, error) {
	h, err := o.state(ctx)
	if err != nil {
		return TargetChange{}, err
	}
	t, err := findTarget(h, key)
	if err != nil {
		return TargetChange{}, err
	}
	return TargetChange{Target: targetOf(h, t), Changed: true}, nil
}

// resolve returns the target written as written, as the Hub would keep
// it, or the rule that refuses it.
func (o *Ops) resolve(ctx context.Context, h hubstore.Hub, written string) (hubstore.Target, error) {
	written = strings.TrimSpace(written)
	owner, repo, isRepo := strings.Cut(written, "/")
	owner, group, isGroup := strings.Cut(owner, ":")
	switch {
	case written == "" || owner == "" || isRepo && (isGroup || repo == "" || strings.Contains(repo, "/")) || isGroup && group == "":
		return hubstore.Target{}, refuse("invalid-target", "%q isn't a target: write owner/repository, an organization, or organization:group", written)
	}
	c, ok := connection(h, owner)
	if !ok {
		return hubstore.Target{}, refuse("no-connection", "the Hub has no GitHub connection for %s: add one in the dashboard, under Settings", owner)
	}
	switch {
	case isRepo && c.Kind != hubstore.Personal:
		return hubstore.Target{}, refuse("org-repository", "the account %s is an organization, so its targets are its runner groups: write %s for its Default group, or %s:<group>", c.Account, c.Account, c.Account)
	case !isRepo && c.Kind == hubstore.Personal:
		return hubstore.Target{}, refuse("personal-group", "the account %s is a personal one, so its targets are its repositories: write %s/<repository>", c.Account, c.Account)
	case isRepo:
		return o.resolveRepo(ctx, c, owner+"/"+repo)
	}
	return o.resolveGroup(ctx, c, group)
}

// resolveRepo returns the target of the repository full, which must be
// private and in c's installation.
func (o *Ops) resolveRepo(ctx context.Context, c hubstore.Connection, full string) (hubstore.Target, error) {
	repos, err := o.c.GitHub.Repositories(ctx, c)
	if err != nil {
		return hubstore.Target{}, fmt.Errorf("GitHub didn't list the repositories of %s's installation: %w", c.Account, err)
	}
	i := slices.IndexFunc(repos, func(r github.Repository) bool { return strings.EqualFold(r.FullName(), full) })
	if i < 0 {
		return hubstore.Target{}, refuse("not-installed", "the repository %s isn't in the App's installation on %s: add it to the installation on GitHub first", full, c.Account)
	}
	r := repos[i]
	if refusal := refusePublic(r); refusal != nil {
		return hubstore.Target{}, refusal
	}
	return hubstore.Target{Key: r.FullName(), Connection: c.Account, GitHub: github.Target{Owner: r.Owner, Repo: r.Name, RepoID: r.ID}}, nil
}

// resolveGroup returns the target of c's runner group named name, or of
// its Default group if name is empty. The group mustn't allow public
// repositories.
func (o *Ops) resolveGroup(ctx context.Context, c hubstore.Connection, name string) (hubstore.Target, error) {
	var g github.Group
	if name == "" {
		var err error
		if g, err = o.c.GitHub.Group(ctx, c, github.OrgTarget(c.Account, defaultGroup)); err != nil {
			return hubstore.Target{}, fmt.Errorf("GitHub didn't read %s's Default runner group: %w", c.Account, err)
		}
	} else {
		groups, err := o.c.GitHub.RunnerGroups(ctx, c)
		if err != nil {
			return hubstore.Target{}, fmt.Errorf("GitHub didn't list %s's runner groups: %w", c.Account, err)
		}
		i := slices.IndexFunc(groups, func(g github.Group) bool { return strings.EqualFold(g.Name, name) })
		if i < 0 {
			return hubstore.Target{}, refuse("unknown-group", "the organization %s has no runner group named %s: Silkpool never creates runner groups", c.Account, name)
		}
		g = groups[i]
	}
	if refusal := refusePublicGroup(c, g); refusal != nil {
		return hubstore.Target{}, refusal
	}
	return hubstore.Target{Key: groupKey(c, g), Connection: c.Account, GitHub: github.OrgTarget(c.Account, g.ID)}, nil
}

// refusePublic refuses a public repository.
func refusePublic(r github.Repository) *Refusal {
	if r.Private {
		return nil
	}
	return refuse("public", "the repository %s is public: Silkpool never serves public repositories", r.FullName())
}

// refusePublicGroup refuses a runner group that allows public
// repositories.
func refusePublicGroup(c hubstore.Connection, g github.Group) *Refusal {
	if !g.AllowsPublic {
		return nil
	}
	return refuse("public-group", "the %s runner group of %s allows public repositories, so it can't be a target", g.Name, c.Account)
}

// groupKey returns how c's group g is written.
func groupKey(c hubstore.Connection, g github.Group) string {
	if g.ID == defaultGroup {
		return c.Account
	}
	return c.Account + ":" + g.Name
}

// groupOf returns what g serves.
func groupOf(g github.Group) *Group {
	out := &Group{Name: g.Name, Visibility: g.Visibility}
	for _, r := range g.Repositories {
		out.Repositories = append(out.Repositories, r.FullName())
	}
	return out
}

// connection returns the connection of account, ignoring case.
func connection(h hubstore.Hub, account string) (hubstore.Connection, bool) {
	i := slices.IndexFunc(h.Connections, func(c hubstore.Connection) bool { return strings.EqualFold(c.Account, account) })
	if i < 0 {
		return hubstore.Connection{}, false
	}
	return h.Connections[i], true
}

// targetOf returns t as target list shows it, without its group.
func targetOf(h hubstore.Hub, t hubstore.Target) Target {
	out := Target{Target: t.Key, Kind: Repository, Connection: t.Connection, Default: h.Settings.DefaultTarget == t.Key, Added: t.Added.UTC()}
	if t.GitHub.IsOrganization() {
		out.Kind = Organization
	}
	if lost := h.Pool.Targets[t.Key].Lost; !lost.IsZero() {
		out.Lost = lost.UTC()
	}
	var platforms []string
	for _, n := range h.Pool.Nodes {
		if n.Target != t.Key || !n.Removed.IsZero() {
			continue
		}
		out.Nodes = append(out.Nodes, n.Name)
		if osName, arch := osArch(n.Platform); osName != "" && !slices.Contains(platforms, osName+" "+arch) {
			platforms = append(platforms, osName+" "+arch)
			out.RunsOn = append(out.RunsOn, fmt.Sprintf("runs-on: [%s, %s, %s]", h.Settings.PoolLabel, osName, arch))
		}
	}
	return out
}

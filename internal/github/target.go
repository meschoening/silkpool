package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/meschoening/silkpool/internal/secret"
)

// A Target is where a node's runners register: one repository, from a
// personal connection, or one runner group of an organization, from an
// organization connection.
type Target struct {
	// Owner is the repository's owner, or the organization.
	Owner string
	// Repo is the repository's name, or empty for an organization
	// target.
	Repo string
	// RepoID is the repository's GitHub ID, which outlives a rename, or
	// 0 if it isn't known yet. Lost matches by it when it's set.
	RepoID int64
	// Group is the organization's runner group, or 0 for a repository
	// target.
	Group int64
}

// RepoTarget returns the repository target owner/name.
func RepoTarget(owner, name string) Target { return Target{Owner: owner, Repo: name} }

// OrgTarget returns the target of org's runner group.
func OrgTarget(org string, group int64) Target { return Target{Owner: org, Group: group} }

// IsOrganization reports whether t is an organization target.
func (t Target) IsOrganization() bool { return t.Repo == "" }

func (t Target) String() string {
	if t.IsOrganization() {
		return fmt.Sprintf("%s (runner group %d)", t.Owner, t.Group)
	}
	return t.Owner + "/" + t.Repo
}

// Permissions are the product's permissions for a connection to an
// organization, or to a personal account, which its App's manifest asks
// for and its installation tokens are narrowed to.
func Permissions(organization bool) map[string]string {
	if organization {
		return map[string]string{
			"organization_self_hosted_runners": "write",
			"actions":                          "read",
			"metadata":                         "read",
		}
	}
	return map[string]string{
		"administration": "write",
		"actions":        "read",
		"metadata":       "read",
	}
}

// scope is the token scope for acting on t: narrowed to its repository,
// with exactly the product's permissions for its kind of connection.
func (t Target) scope() scope {
	if t.IsOrganization() {
		return scope{perms: Permissions(true)}
	}
	return scope{repos: []string{t.Repo}, perms: Permissions(false)}
}

// runners is the path of t's runners endpoints.
func (t Target) runners() string {
	if t.IsOrganization() {
		return "/orgs/" + url.PathEscape(t.Owner) + "/actions/runners"
	}
	return t.repo() + "/actions/runners"
}

// group is the path of an organization target's runner group.
func (t Target) group() string {
	return fmt.Sprintf("/orgs/%s/actions/runner-groups/%d", url.PathEscape(t.Owner), t.Group)
}

// repo is the path of a repository target's repository.
func (t Target) repo() string {
	return "/repos/" + url.PathEscape(t.Owner) + "/" + url.PathEscape(t.Repo)
}

// unreachable reports whether err is GitHub refusing a token narrowed
// to t's repository (422) because the installation doesn't reach it.
// GitHub's own calls answer 404 for such a repository, so the client
// reports it as 404 too.
func (t Target) unreachable(err error) bool {
	e, ok := tokenFailed(err)
	return ok && !t.IsOrganization() && e.Status == http.StatusUnprocessableEntity
}

// ErrPublic is a target Silkpool refuses because it may serve a public
// repository: a public repository, or a runner group that allows public
// repositories.
var ErrPublic = errors.New("silkpool never serves public repositories")

// CheckTarget checks t before a mint: a repository target must be
// private, and an organization target's runner group must not allow
// public repositories. A refusal wraps ErrPublic, and reads as status 403
// when Mint reports it.
func (c *Client) CheckTarget(ctx context.Context, t Target) error {
	if t.IsOrganization() {
		g, err := c.Group(ctx, t)
		if err != nil {
			return err
		}
		if g.AllowsPublic {
			// The group's name is what GitHub holds, which the
			// organization wrote, so it's scrubbed as it's copied in.
			return fmt.Errorf("runner group %q of %s allows public repositories: %w", secret.Scrub(g.Name), t.Owner, ErrPublic)
		}
		return nil
	}
	var repo struct {
		Private bool `json:"private"`
	}
	if _, err := c.do(ctx, t.scope(), http.MethodGet, t.repo(), nil, &repo); err != nil {
		return err
	}
	if !repo.Private {
		return fmt.Errorf("%s is public: %w", t, ErrPublic)
	}
	return nil
}

// A Group is an organization's runner group.
type Group struct {
	ID   int64
	Name string
	// Visibility is which of the organization's repositories the group
	// serves: "all", "private" or "selected".
	Visibility string
	// AllowsPublic reports that the group serves public repositories
	// too, which Silkpool refuses.
	AllowsPublic bool
	// Repositories are the repositories it serves when Visibility is
	// "selected", and nil otherwise.
	Repositories []Repository
}

// group is a runner group as GitHub's JSON has it.
type group struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Visibility   string `json:"visibility"`
	AllowsPublic bool   `json:"allows_public_repositories"`
}

// Group reads an organization target's runner group, and the
// repositories it serves when it serves only selected ones.
func (c *Client) Group(ctx context.Context, t Target) (Group, error) {
	var g group
	if _, err := c.do(ctx, t.scope(), http.MethodGet, t.group(), nil, &g); err != nil {
		return Group{}, err
	}
	return c.served(ctx, t, g)
}

// RunnerGroups lists org's runner groups, each with the repositories it
// serves when it serves only selected ones, for the operator to choose a
// target from.
func (c *Client) RunnerGroups(ctx context.Context, org string) ([]Group, error) {
	t := OrgTarget(org, 0)
	groups, err := list[group](ctx, c, t.scope(), "/orgs/"+url.PathEscape(org)+"/actions/runner-groups?per_page=100", "runner_groups")
	if err != nil {
		return nil, err
	}
	out := []Group{}
	for _, g := range groups {
		t.Group = g.ID
		full, err := c.served(ctx, t, g)
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

// served returns g, the runner group of t, with the repositories it
// serves when it serves only selected ones.
func (c *Client) served(ctx context.Context, t Target, g group) (Group, error) {
	out := Group{ID: g.ID, Name: g.Name, Visibility: g.Visibility, AllowsPublic: g.AllowsPublic}
	if g.Visibility == "selected" {
		repos, err := list[repository](ctx, c, t.scope(), t.group()+"/repositories?per_page=100", "repositories")
		if err != nil {
			return Group{}, err
		}
		out.Repositories = []Repository{}
		for _, r := range repos {
			out.Repositories = append(out.Repositories, r.public())
		}
	}
	return out, nil
}

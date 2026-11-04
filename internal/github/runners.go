package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/meschoening/silkpool/internal/registration"
	"github.com/meschoening/silkpool/internal/secret"
)

// RunnerLabels returns a runner's labels, as ADR 0003 sets them: the
// pool label, its OS (linux, windows or macos), its architecture (x64 or
// arm64), then the node's custom labels, all lowercase and each once.
// goos and goarch are the agent's GOOS and GOARCH. It never adds
// self-hosted: only a custom label can.
func RunnerLabels(pool, goos, goarch string, custom []string) ([]string, error) {
	osLabel, ok := map[string]string{"linux": "linux", "windows": "windows", "darwin": "macos"}[goos]
	if !ok {
		return nil, fmt.Errorf("silkpool doesn't run on %s", goos)
	}
	archLabel, ok := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if !ok {
		return nil, fmt.Errorf("silkpool doesn't run on %s", goarch)
	}
	if strings.TrimSpace(pool) == "" {
		return nil, errors.New("the pool label is empty")
	}
	var labels []string
	for _, l := range append([]string{pool, osLabel, archLabel}, custom...) {
		l = strings.ToLower(strings.TrimSpace(l))
		if l != "" && !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	return labels, nil
}

// repoRunnerGroup is the runner group every repository-level JIT runner
// must name: GitHub refuses the request without it (422), and answers
// any other ID with 404.
const repoRunnerGroup = 1

// Mint carries out a GenerateJITConfig effect: it checks t (CheckTarget),
// then has GitHub generate a JIT config for a runner named name with
// exactly labels, in runner group 1 for a repository target or t's group
// for an organization target. It returns the event to apply and, after
// Generated, the encoded JIT config, which the Hub keeps for the node
// (the zero secret.Value otherwise):
//
//   - Generated, with the runner's ID;
//   - NameTaken, with random, when GitHub answers 409;
//   - GenerateFailed for anything else, with the status of whichever
//     call failed: 429 for any rate limit, 0 with no answer, 404 for a
//     repository the installation doesn't reach, and 403 for a target
//     CheckTarget refuses. Its Detail is that call's error, in which
//     what GitHub and the network said is already scrubbed (see Error),
//     so the Hub can log it and send it to the node as it is.
func (c *Client) Mint(ctx context.Context, t Target, name string, labels []string, random uint32) (registration.Event, secret.Value) {
	fail := func(err error) (registration.Event, secret.Value) {
		status := Status(err)
		switch {
		case errors.Is(err, ErrPublic):
			status = http.StatusForbidden
		case t.unreachable(err):
			status = http.StatusNotFound
		}
		return registration.GenerateFailed{Runner: name, Status: status, Detail: err.Error()}, secret.Value{}
	}
	if err := c.CheckTarget(ctx, t); err != nil {
		return fail(err)
	}
	group := int64(repoRunnerGroup)
	if t.IsOrganization() {
		group = t.Group
	}
	body := map[string]any{
		"name":            name,
		"runner_group_id": group,
		"labels":          labels,
		"work_folder":     "_work",
	}
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Config string `json:"encoded_jit_config"`
	}
	path := t.runners() + "/generate-jitconfig"
	if _, err := c.do(ctx, t.scope(), http.MethodPost, path, body, &out); err != nil {
		if Status(err) == http.StatusConflict {
			return registration.NameTaken{Runner: name, Random: random}, secret.Value{}
		}
		return fail(err)
	}
	if out.Runner.ID == 0 || out.Config == "" {
		// The registration may exist all the same, as with no answer.
		return fail(&Error{Op: "POST " + path, Message: "GitHub's answer carries no runner ID or JIT config"})
	}
	return registration.Generated{Runner: name, ID: out.Runner.ID}, secret.New(out.Config)
}

// Delete carries out a DeleteRunner effect: it DELETEs runner id's
// registration from t, and returns the result as Deleted for runner
// name. Status is the DELETE's: 204 when it's gone, 422 while it holds a
// job, or 404 with TargetVisible reporting whether the Hub can still see
// t. A call that fails before the DELETE reads as that call's status
// (429 for any rate limit, 0 with no answer) with TargetVisible false,
// and as 404 for a repository the installation doesn't reach. Only the
// DELETE itself ever reads as 422.
func (c *Client) Delete(ctx context.Context, t Target, name string, id int64) registration.Deleted {
	path := fmt.Sprintf("%s/%d", t.runners(), id)
	_, err := c.do(ctx, t.scope(), http.MethodDelete, path, nil, nil)
	d := registration.Deleted{Runner: name, Status: http.StatusNoContent, TargetVisible: true}
	if err == nil {
		return d
	}
	d.Status = Status(err)
	_, beforeDelete := tokenFailed(err)
	switch {
	case beforeDelete:
		d.TargetVisible = false
		if t.unreachable(err) {
			d.Status = http.StatusNotFound
		} else if d.Status == http.StatusUnprocessableEntity {
			d.Status = http.StatusForbidden
		}
	case d.Status == http.StatusNotFound:
		d.TargetVisible = c.visible(ctx, t)
	case d.Status != http.StatusUnprocessableEntity:
		d.TargetVisible = false
	}
	return d
}

// visible reports whether the Hub can see t right now: its repository,
// or its runner group.
func (c *Client) visible(ctx context.Context, t Target) bool {
	path := t.group()
	if !t.IsOrganization() {
		path = t.repo()
	}
	_, err := c.do(ctx, t.scope(), http.MethodGet, path, nil, nil)
	return err == nil
}

// A Runner is one runner registered in a target, as GitHub lists it.
type Runner struct {
	ID   int64
	Name string
	// Status is "online" or "offline".
	Status string
	// Busy reports that GitHub counts a job as assigned to it.
	Busy bool
	// Labels are its labels, in GitHub's order.
	Labels []string
}

// Runners lists the runners registered in t: a repository's, or those in
// an organization target's runner group.
func (c *Client) Runners(ctx context.Context, t Target) ([]Runner, error) {
	path := t.runners()
	if t.IsOrganization() {
		path = t.group() + "/runners"
	}
	type label struct {
		Name string `json:"name"`
	}
	type runner struct {
		ID     int64   `json:"id"`
		Name   string  `json:"name"`
		Status string  `json:"status"`
		Busy   bool    `json:"busy"`
		Labels []label `json:"labels"`
	}
	got, err := list[runner](ctx, c, t.scope(), path+"?per_page=100", "runners")
	if err != nil {
		return nil, err
	}
	runners := []Runner{}
	for _, r := range got {
		out := Runner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy, Labels: []string{}}
		for _, l := range r.Labels {
			out.Labels = append(out.Labels, l.Name)
		}
		runners = append(runners, out)
	}
	return runners, nil
}

// Listed returns runners as the Listed event for target, the Hub's key
// for the target they were listed in, for the periodic comparison.
func Listed(target string, runners []Runner) registration.Listed {
	l := registration.Listed{Target: target, Runners: []registration.Listing{}}
	for _, r := range runners {
		l.Runners = append(l.Runners, registration.Listing{ID: r.ID, Name: r.Name})
	}
	return l
}

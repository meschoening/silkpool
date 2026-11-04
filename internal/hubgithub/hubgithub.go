// Package hubgithub carries out the Hub's GitHub work through package
// github's client: the registration records' mints and DELETEs, the
// runner version's checks, and the polls the Hub's loop makes (package
// hub). The decisions behind it are in "Define the registration scope
// model and labels",
// "Build the Hub's registration records core"
// and ADR 0011.
//
// It keeps one github.Client for each of the Hub's GitHub connections,
// made from the connection's App and its private key file in the Hub's
// state folder, so each connection's installation tokens live in its
// client's memory, are replaced before they expire, and are never
// logged. A client whose rate limit is spent fails every call at once
// until GitHub's reset time, so a limited connection's work fails as a
// rate limit (429) without reaching GitHub.
package hubgithub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/meschoening/silkpool/internal/github"
	"github.com/meschoening/silkpool/internal/hub"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/pool"
	"github.com/meschoening/silkpool/internal/registration"
	"github.com/meschoening/silkpool/internal/runnerapp"
	"github.com/meschoening/silkpool/internal/secret"
)

// Config is what a GitHub needs.
type Config struct {
	// Dir is the Hub's state folder, which holds each connection's
	// private key file.
	Dir string
	// BaseURL is GitHub's REST API root: github.API when empty, or a
	// fake's URL. Web is github.com's: github.Web when empty, or a
	// fake's URL.
	BaseURL string
	Web     string
	// HTTP makes the requests, or nil for the client's own.
	HTTP *http.Client
	// Log is where it logs, or nil for slog.Default.
	Log *slog.Logger
}

// GitHub carries out the Hub's GitHub work. It's the hub.GitHub the
// Hub service runs with, and it's safe for concurrent use.
type GitHub struct {
	c   Config
	log *slog.Logger

	mu      sync.Mutex
	clients map[string]client // by connection account
}

// A client is the client for one connection, as the connection was when
// it was made.
type client struct {
	conn   hubstore.Connection
	client *github.Client
}

var _ hub.GitHub = (*GitHub)(nil)

// New returns a GitHub for c.
func New(c Config) *GitHub {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	return &GitHub{c: c, log: log, clients: map[string]client{}}
}

// Carry carries out work with the Hub's state h, and returns the events
// that come of it. Once ctx has ended, the Hub drops them, so nothing
// is logged of them either.
func (g *GitHub) Carry(ctx context.Context, h hubstore.Hub, work hubstore.Effect) []hubstore.Event {
	switch w := work.(type) {
	case registration.GenerateJITConfig:
		return []hubstore.Event{g.mint(ctx, h, w)}
	case registration.DeleteRunner:
		return []hubstore.Event{g.delete(ctx, h, w)}
	case runnerapp.CheckDownloads:
		return []hubstore.Event{g.checkDownloads(ctx, h, w)}
	case runnerapp.FetchLatest:
		return []hubstore.Event{g.fetchLatest(ctx, h)}
	case hub.ListRunners:
		return g.list(ctx, h, w)
	case hub.CheckTargets:
		return g.checkTargets(ctx, h, w)
	}
	g.log.Error("GitHub work the Hub doesn't carry out", "work", fmt.Sprintf("%T", work))
	return nil
}

// logIf logs, unless ctx has ended: the Hub drops what comes of work
// it's no longer waiting for.
func (g *GitHub) logIf(ctx context.Context, level slog.Level, msg string, args ...any) {
	if ctx.Err() == nil {
		g.log.Log(ctx, level, msg, args...)
	}
}

// mint has GitHub generate a JIT config for a node's runner, with the
// node's labels, after checking the target. One the Hub can't ask for,
// because it no longer holds the node, the target or its connection, or
// the node's platform is unknown, fails as GitHub refusing it (404), so
// its record is dropped; one it can't ask for because the connection's
// key can't be read fails as one with no answer.
//
// The GenerateFailed's detail goes to the node, and is logged, as it's
// made: Mint's comes with what GitHub said already scrubbed, and the
// Hub's own reason is scrubbed here, since it copies in what the machine
// said. It never names the platform a node stated, which shows only as
// unknown.
func (g *GitHub) mint(ctx context.Context, h hubstore.Hub, w registration.GenerateJITConfig) hubstore.Event {
	t, cl, err := g.target(h, w.Target)
	if err == nil {
		var labels []string
		if labels, err = runnerLabels(h, w.Node); err == nil {
			ev, config := cl.Mint(ctx, t.GitHub, w.Runner, labels, mrand.Uint32())
			switch ev := ev.(type) {
			case registration.Generated:
				g.logIf(ctx, slog.LevelInfo, "GitHub minted a runner", "node", w.Node, "runner", w.Runner, "id", ev.ID, "target", w.Target, "labels", labels)
				return hub.Minted{Generated: ev, JITConfig: config}
			case registration.NameTaken:
				g.logIf(ctx, slog.LevelInfo, "a runner's name is taken on GitHub, so the Hub picks another", "runner", w.Runner, "target", w.Target)
			case registration.GenerateFailed:
				g.logIf(ctx, slog.LevelWarn, "GitHub didn't mint a runner", "node", w.Node, "runner", w.Runner, "target", w.Target, "status", ev.Status, "err", ev.Detail)
			}
			return ev
		}
	}
	failed := registration.GenerateFailed{Runner: w.Runner, Detail: secret.Scrub(err.Error())}
	g.logIf(ctx, slog.LevelError, "couldn't ask GitHub for a runner", "node", w.Node, "runner", w.Runner, "target", w.Target, "err", failed.Detail)
	if errors.Is(err, errMissing) {
		failed.Status = http.StatusNotFound
	}
	return failed
}

// delete DELETEs a runner's registration. Without a client for its
// target, the DELETE reads as one with no answer.
func (g *GitHub) delete(ctx context.Context, h hubstore.Hub, w registration.DeleteRunner) hubstore.Event {
	t, cl, err := g.target(h, w.Target)
	if err != nil {
		g.logIf(ctx, slog.LevelError, "couldn't delete a runner's registration", "runner", w.Runner, "target", w.Target, "err", err)
		return registration.Deleted{Runner: w.Runner}
	}
	d := cl.Delete(ctx, t.GitHub, w.Runner, w.ID)
	g.logIf(ctx, slog.LevelInfo, "deleted a runner's registration", "runner", w.Runner, "id", w.ID, "target", w.Target, "status", d.Status, "target_visible", d.TargetVisible)
	return d
}

// checkDownloads reads the runner application GitHub serves a target.
func (g *GitHub) checkDownloads(ctx context.Context, h hubstore.Hub, w runnerapp.CheckDownloads) hubstore.Event {
	t, cl, err := g.target(h, w.Target)
	var got []github.Download
	if err == nil {
		got, err = cl.Downloads(ctx, t.GitHub)
	}
	if err != nil {
		g.logIf(ctx, slog.LevelWarn, "couldn't read the runner version GitHub serves a target", "target", w.Target, "err", err)
		return runnerapp.CheckFailed(w)
	}
	served := runnerapp.Served{Target: w.Target, Downloads: []runnerapp.Download{}}
	for _, d := range got {
		served.Downloads = append(served.Downloads, runnerapp.Download{OS: d.OS, Arch: d.Arch, Filename: d.Filename, SHA256: d.SHA256})
	}
	return served
}

// fetchLatest reads actions/runner's latest release, a public read that
// any connection can make: the first one's makes it.
func (g *GitHub) fetchLatest(ctx context.Context, h hubstore.Hub) hubstore.Event {
	if len(h.Connections) == 0 {
		g.logIf(ctx, slog.LevelWarn, "no GitHub connection to read the latest runner release with")
		return runnerapp.LatestFailed{}
	}
	cl, err := g.client(h.Connections[0])
	var r github.Release
	if err == nil {
		r, err = cl.LatestRunnerRelease(ctx)
	}
	if err != nil {
		g.logIf(ctx, slog.LevelWarn, "couldn't read the latest runner release", "err", err)
		return runnerapp.LatestFailed{}
	}
	release := runnerapp.Release{Tag: r.Tag, Assets: []runnerapp.Asset{}}
	for _, a := range r.Assets {
		release.Assets = append(release.Assets, runnerapp.Asset{Name: a.Name, Digest: a.Digest})
	}
	return runnerapp.Latest{Release: release}
}

// list lists a target's runners for the comparison. A list that fails
// compares nothing.
func (g *GitHub) list(ctx context.Context, h hubstore.Hub, w hub.ListRunners) []hubstore.Event {
	t, cl, err := g.target(h, w.Target)
	var runners []github.Runner
	if err == nil {
		runners, err = cl.Runners(ctx, t.GitHub)
	}
	if err != nil {
		g.logIf(ctx, slog.LevelInfo, "couldn't list a target's runners for the comparison", "target", w.Target, "err", err)
		return nil
	}
	return []hubstore.Event{github.Listed(w.Target, runners)}
}

// checkTargets reads the installation's repositories, and for each of
// the connection's targets applies TargetLost on positive evidence that
// the Hub can't see it (github.Lost), unless it's already lost, and
// TargetRegained once a lost one is listed again. An organization target
// is listed again only if its runner group can be read too.
func (g *GitHub) checkTargets(ctx context.Context, h hubstore.Hub, w hub.CheckTargets) []hubstore.Event {
	i := slices.IndexFunc(h.Connections, func(c hubstore.Connection) bool { return c.Account == w.Account })
	if i < 0 {
		return nil
	}
	cl, err := g.client(h.Connections[i])
	if err != nil {
		g.logIf(ctx, slog.LevelError, "couldn't check a connection's targets", "connection", w.Account, "err", err)
		return nil
	}
	repos, err := cl.Repositories(ctx)
	if err != nil {
		g.logIf(ctx, slog.LevelInfo, "couldn't list the installation's repositories", "connection", w.Account, "err", err)
	}
	var events []hubstore.Event
	for _, t := range h.Targets {
		if t.Connection != w.Account {
			continue
		}
		lost := !h.Pool.Targets[t.Key].Lost.IsZero()
		switch {
		case !lost && github.Lost(t.GitHub, repos, err):
			g.logIf(ctx, slog.LevelWarn, "the Hub can no longer see a target on GitHub", "target", t.Key, "err", err)
			events = append(events, pool.TargetLost{Target: t.Key})
		case lost && err == nil && !github.Lost(t.GitHub, repos, nil) && g.groupVisible(ctx, cl, t.GitHub):
			g.logIf(ctx, slog.LevelInfo, "the Hub can see a lost target on GitHub again", "target", t.Key)
			events = append(events, pool.TargetRegained{Target: t.Key})
		}
	}
	return events
}

// groupVisible reports whether t's runner group can be read, which a
// repository target has none of.
func (g *GitHub) groupVisible(ctx context.Context, cl *github.Client, t github.Target) bool {
	if !t.IsOrganization() {
		return true
	}
	if _, err := cl.Group(ctx, t); err != nil {
		g.logIf(ctx, slog.LevelInfo, "couldn't read a lost target's runner group", "target", t.String(), "err", err)
		return false
	}
	return true
}

// The reads the operator's target choices need, through each
// connection's client: package hubops lists what a connection offers,
// and checks a target before the Hub adds it.

// Repositories lists the repositories c's installation covers.
func (g *GitHub) Repositories(ctx context.Context, c hubstore.Connection) ([]github.Repository, error) {
	cl, err := g.client(c)
	if err != nil {
		return nil, err
	}
	return cl.Repositories(ctx)
}

// RunnerGroups lists the runner groups of c's organization.
func (g *GitHub) RunnerGroups(ctx context.Context, c hubstore.Connection) ([]github.Group, error) {
	cl, err := g.client(c)
	if err != nil {
		return nil, err
	}
	return cl.RunnerGroups(ctx, c.Account)
}

// Group reads the runner group of t, an organization target of c.
func (g *GitHub) Group(ctx context.Context, c hubstore.Connection, t github.Target) (github.Group, error) {
	cl, err := g.client(c)
	if err != nil {
		return github.Group{}, err
	}
	return cl.Group(ctx, t)
}

// errMissing is GitHub work naming a node, a target or a connection the
// Hub doesn't hold.
var errMissing = errors.New("the Hub holds no")

// target returns the Hub's target key and the client of its
// connection.
func (g *GitHub) target(h hubstore.Hub, key string) (hubstore.Target, *github.Client, error) {
	i := slices.IndexFunc(h.Targets, func(t hubstore.Target) bool { return t.Key == key })
	if i < 0 {
		return hubstore.Target{}, nil, fmt.Errorf("%w target %q", errMissing, key)
	}
	t := h.Targets[i]
	j := slices.IndexFunc(h.Connections, func(c hubstore.Connection) bool { return c.Account == t.Connection })
	if j < 0 {
		return t, nil, fmt.Errorf("%w GitHub connection %q for target %q", errMissing, t.Connection, key)
	}
	cl, err := g.client(h.Connections[j])
	return t, cl, err
}

// client returns the client for c, made the first time it's asked for,
// and again whenever c changes.
func (g *GitHub) client(c hubstore.Connection) (*github.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cl, ok := g.clients[c.Account]; ok && cl.conn == c {
		return cl.client, nil
	}
	path := filepath.Join(g.c.Dir, c.KeyFile)
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the private key of GitHub connection %q: %w", c.Account, err)
	}
	cl, err := github.New(github.Config{BaseURL: g.c.BaseURL, AppID: c.AppID, Installation: c.Installation, Key: secret.New(string(key)), HTTP: g.c.HTTP})
	if err != nil {
		return nil, fmt.Errorf("the private key of GitHub connection %q in %s: %w", c.Account, path, err)
	}
	g.clients[c.Account] = client{conn: c, client: cl}
	return cl, nil
}

// runnerLabels returns the labels of a node's runners: the pool label,
// the node's platform and its custom labels (ADR 0003).
func runnerLabels(h hubstore.Hub, node string) ([]string, error) {
	i := slices.IndexFunc(h.Pool.Nodes, func(n pool.Node) bool { return n.ID == node })
	if i < 0 {
		return nil, fmt.Errorf("%w node %q", errMissing, node)
	}
	p, ok := goPlatforms[h.Pool.Nodes[i].Platform]
	if !ok {
		// Never what the node stated: the failure reaches the node's slot,
		// which the operator reads.
		return nil, fmt.Errorf("%w runner labels for node %q, whose platform is unknown", errMissing, node)
	}
	return github.RunnerLabels(h.Settings.PoolLabel, p.goos, p.goarch, h.Labels[node])
}

// goPlatforms are the GOOS and GOARCH of each platform.
var goPlatforms = map[runnerapp.Platform]struct{ goos, goarch string }{
	runnerapp.LinuxX64:   {"linux", "amd64"},
	runnerapp.LinuxARM64: {"linux", "arm64"},
	runnerapp.WinX64:     {"windows", "amd64"},
	runnerapp.WinARM64:   {"windows", "arm64"},
	runnerapp.OSXARM64:   {"darwin", "arm64"},
}

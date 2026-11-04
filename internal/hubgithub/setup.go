package hubgithub

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/github"
	"github.com/meschoening/silkpool/internal/hubstore"
	"github.com/meschoening/silkpool/internal/secret"
)

// The calls of the Hub's setup, which package hubops makes for the
// dashboard: GitHub's clock, the new App's credentials, the operator's
// sign-in, and the App's installation.

func (g *GitHub) setup() github.Setup {
	return github.Setup{API: g.c.BaseURL, Web: g.c.Web, HTTP: g.c.HTTP}
}

// Clock reads GitHub's clock, which fails if the Hub can't reach
// GitHub's API.
func (g *GitHub) Clock(ctx context.Context) (time.Time, error) { return g.setup().Clock(ctx) }

// CreateApp converts the code of the App the manifest's flow created,
// writes the App's private key and client secret into the Hub's state
// folder, each mode 0600 and named for the App's ID, and returns the
// connection to the App's owner, not yet installed, with the owner.
func (g *GitHub) CreateApp(ctx context.Context, code string) (hubstore.Connection, github.Account, error) {
	app, err := g.setup().ConvertManifest(ctx, code)
	if err != nil {
		return hubstore.Connection{}, github.Account{}, err
	}
	kind := hubstore.Personal
	if app.Owner.Organization {
		kind = hubstore.Organization
	}
	c := hubstore.Connection{
		Account: app.Owner.Login, Kind: kind, AppID: app.ID, AppSlug: app.Slug, ClientID: app.ClientID,
		KeyFile: fmt.Sprintf("app-%d.pem", app.ID), SecretFile: fmt.Sprintf("app-%d.secret", app.ID),
	}
	if err := g.writeSecret(c.KeyFile, app.Key); err != nil {
		return hubstore.Connection{}, github.Account{}, err
	}
	if err := g.writeSecret(c.SecretFile, app.ClientSecret); err != nil {
		return hubstore.Connection{}, github.Account{}, err
	}
	g.logIf(ctx, slog.LevelInfo, "GitHub created the App", "app", app.Slug, "id", app.ID, "owner", app.Owner.Login)
	return c, app.Owner, nil
}

// writeSecret writes v into the file name in the state folder, mode
// 0600, replacing it whole.
func (g *GitHub) writeSecret(name string, v secret.Value) error {
	f, err := os.CreateTemp(g.c.Dir, "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(v.Reveal()); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(g.c.Dir, name))
}

// Installation looks up the installation id with the JWT of c's App.
func (g *GitHub) Installation(ctx context.Context, c hubstore.Connection, id int64) (github.Installation, error) {
	cl, err := g.client(c)
	if err != nil {
		return github.Installation{}, err
	}
	return cl.Installation(ctx, id)
}

// SignIn exchanges the code of a sign-in through c's App, with the
// client secret in the state folder, and returns who signed in. The
// user token lives only as long as the call.
func (g *GitHub) SignIn(ctx context.Context, c hubstore.Connection, code, redirect string) (github.Account, error) {
	b, err := os.ReadFile(filepath.Join(g.c.Dir, c.SecretFile))
	if err != nil {
		return github.Account{}, fmt.Errorf("read the client secret of GitHub connection %q: %w", c.Account, err)
	}
	token, err := g.setup().Exchange(ctx, c.ClientID, secret.New(strings.TrimSpace(string(b))), code, redirect)
	if err != nil {
		return github.Account{}, err
	}
	return g.setup().User(ctx, token)
}

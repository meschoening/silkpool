package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// A Repository is a repository on GitHub.
type Repository struct {
	ID          int64
	Owner, Name string
	Private     bool
}

// FullName is owner/name.
func (r Repository) FullName() string { return r.Owner + "/" + r.Name }

// repository is a repository as GitHub's JSON has it.
type repository struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Private bool   `json:"private"`
	Owner   struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r repository) public() Repository {
	return Repository{ID: r.ID, Owner: r.Owner.Login, Name: r.Name, Private: r.Private}
}

// ErrInstallationGone and ErrInstallationSuspended report positive
// evidence that the connection's installation is gone or suspended:
// GitHub's list of the App's installations, read with a valid App JWT,
// doesn't hold it, or holds it suspended.
var (
	ErrInstallationGone      = errors.New("the App's installation is gone")
	ErrInstallationSuspended = errors.New("the App's installation is suspended")
)

// Repositories lists the repositories the installation covers. When
// GitHub refuses the installation a token, or refuses one it already
// holds (401, 403 or 404), it reads the App's installations to tell why:
// an error wrapping ErrInstallationGone or ErrInstallationSuspended is
// positive evidence, and any other error is none.
func (c *Client) Repositories(ctx context.Context) ([]Repository, error) {
	repos, err := list[repository](ctx, c, metadataOnly, "/installation/repositories?per_page=100", "repositories")
	if err != nil {
		if s := Status(err); s == http.StatusUnauthorized || s == http.StatusForbidden || s == http.StatusNotFound {
			if why := c.installationState(ctx); why != nil {
				return nil, fmt.Errorf("%w (%v)", why, err)
			}
		}
		return nil, err
	}
	out := []Repository{}
	for _, r := range repos {
		out = append(out, r.public())
	}
	return out, nil
}

// installationState returns ErrInstallationGone or
// ErrInstallationSuspended if the App's list of its installations shows
// that, and nil if it doesn't or can't be read.
func (c *Client) installationState(ctx context.Context) error {
	jwt, err := c.jwt()
	if err != nil {
		return nil
	}
	next := "/app/installations?per_page=100"
	for next != "" {
		var page []struct {
			ID          int64   `json:"id"`
			SuspendedAt *string `json:"suspended_at"`
		}
		resp, err := c.call(ctx, bearer(jwt), http.MethodGet, next, nil, &page)
		if err != nil {
			return nil
		}
		for _, in := range page {
			if in.ID == c.installation {
				if in.SuspendedAt != nil {
					return ErrInstallationSuspended
				}
				return nil
			}
		}
		next = nextLink(resp.Header.Get("Link"))
		if next != "" && !strings.HasPrefix(next, c.base+"/") {
			return nil
		}
	}
	return ErrInstallationGone
}

// Lost reports whether the answer of Repositories, repos and err, is
// positive evidence that t is lost, so the Hub applies TargetLost: the
// installation is gone or suspended, or a successful list no longer
// holds t's repository, matched by RepoID when it's set. Anything else,
// a lone 404 included, is no evidence at all.
func Lost(t Target, repos []Repository, err error) bool {
	if err != nil {
		return errors.Is(err, ErrInstallationGone) || errors.Is(err, ErrInstallationSuspended)
	}
	if t.IsOrganization() {
		return false
	}
	for _, r := range repos {
		if t.RepoID != 0 && r.ID == t.RepoID ||
			t.RepoID == 0 && strings.EqualFold(r.FullName(), t.Owner+"/"+t.Repo) {
			return false
		}
	}
	return true
}

// An Account is a GitHub account: a user, or an organization.
type Account struct {
	ID           int64
	Login        string
	Organization bool
}

// account is an account as GitHub's JSON has it.
type account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (a account) public() Account {
	return Account{ID: a.ID, Login: a.Login, Organization: a.Type == "Organization"}
}

// An Installation is an App's installation on an account.
type Installation struct {
	ID, AppID int64
	Account   Account
	// Selected reports that it covers only the repositories the operator
	// selected, not all of the account's.
	Selected  bool
	Suspended bool
}

// Installation looks up the installation id with the App's JWT, as
// setup does with the installation_id GitHub's browser redirect hands
// the Hub, which anyone could have written. Another App's installation,
// or one that's gone, reads as 404.
func (c *Client) Installation(ctx context.Context, id int64) (Installation, error) {
	jwt, err := c.jwt()
	if err != nil {
		return Installation{}, err
	}
	var out struct {
		ID          int64   `json:"id"`
		AppID       int64   `json:"app_id"`
		Account     account `json:"account"`
		Selection   string  `json:"repository_selection"`
		SuspendedAt *string `json:"suspended_at"`
	}
	if _, err := c.call(ctx, bearer(jwt), http.MethodGet, fmt.Sprintf("/app/installations/%d", id), nil, &out); err != nil {
		return Installation{}, err
	}
	return Installation{ID: out.ID, AppID: out.AppID, Account: out.Account.public(), Selected: out.Selection == "selected", Suspended: out.SuspendedAt != nil}, nil
}

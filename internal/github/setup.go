package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/meschoening/silkpool/internal/secret"
)

// Web is github.com, where the operator's browser goes in setup: the App
// manifest's form, sign-in, and the App's install page.
const Web = "https://github.com"

// A Setup makes the calls of the Hub's setup that come before an App's
// installation, or need none: reading GitHub's clock, converting the
// code the App manifest's flow returns into the new App's credentials,
// and signing the operator in through the App. The browser's part of
// each flow is the dashboard's; these are the Hub's own calls. Its zero
// value calls github.com.
//
// Like a Client's, the secrets it hands over and takes are each a
// secret.Value.
type Setup struct {
	// API is the REST API's root, API when empty, and Web github.com's,
	// Web when empty. Tests give a fake's URL for both.
	API, Web string
	// HTTP makes the requests. When nil, a client with a 30 s timeout.
	HTTP *http.Client
}

func (s Setup) api() string { return strings.TrimRight(cmpOr(s.API, API), "/") }
func (s Setup) web() string { return strings.TrimRight(cmpOr(s.Web, Web), "/") }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s Setup) http() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Clock reads GitHub's clock: the Date of its answer to the rate limit's
// state, a call that costs none of it. A failure means the Hub can't
// reach GitHub's API.
func (s Setup) Clock(ctx context.Context) (time.Time, error) {
	resp, err := s.send(ctx, http.MethodGet, s.api()+"/rate_limit", "", nil, secret.Value{}, nil)
	if err != nil {
		return time.Time{}, err
	}
	at, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return time.Time{}, &Error{Op: "GET /rate_limit", Message: "GitHub's answer has no Date"}
	}
	return at, nil
}

// A NewApp is the App the manifest's flow created, and its credentials.
type NewApp struct {
	ID         int64
	Slug, Name string
	Owner      Account
	// ClientID signs the operator in through the App, with ClientSecret.
	ClientID     string
	ClientSecret secret.Value
	// Key is the App's private key, in the PEM GitHub hands out once.
	Key         secret.Value
	Permissions map[string]string
}

// ConvertManifest converts the code GitHub hands the browser after it
// creates an App from a manifest into the App's credentials. GitHub does
// that once per code, which expires an hour after the App is created:
// any other code reads as 404.
func (s Setup) ConvertManifest(ctx context.Context, code string) (NewApp, error) {
	var out struct {
		ID           int64             `json:"id"`
		Slug         string            `json:"slug"`
		Name         string            `json:"name"`
		Owner        account           `json:"owner"`
		ClientID     string            `json:"client_id"`
		ClientSecret string            `json:"client_secret"`
		PEM          string            `json:"pem"`
		Permissions  map[string]string `json:"permissions"`
	}
	path := "/app-manifests/" + url.PathEscape(code) + "/conversions"
	if _, err := s.send(ctx, http.MethodPost, s.api()+path, "POST /app-manifests/{code}/conversions", nil, secret.Value{}, &out); err != nil {
		return NewApp{}, err
	}
	if out.ID == 0 || out.ClientID == "" || out.ClientSecret == "" || out.PEM == "" {
		return NewApp{}, &Error{Op: "POST /app-manifests/{code}/conversions", Message: "the answer lacks the App's credentials"}
	}
	app := NewApp{ID: out.ID, Slug: out.Slug, Name: out.Name, Owner: out.Owner.public(), ClientID: out.ClientID,
		ClientSecret: secret.New(out.ClientSecret), Key: secret.New(out.PEM), Permissions: out.Permissions}
	if _, err := parseKey(app.Key); err != nil {
		return NewApp{}, &Error{Op: "POST /app-manifests/{code}/conversions", Message: err.Error()}
	}
	return app, nil
}

// Exchange exchanges the code GitHub hands the browser when the
// operator signs in through the App with clientID for a user token,
// which says who signed in. GitHub does that once per code, and only
// with the App's client secret and the redirect URI the sign-in named.
func (s Setup) Exchange(ctx context.Context, clientID string, clientSecret secret.Value, code, redirect string) (secret.Value, error) {
	form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret.Reveal()}, "code": {code}, "redirect_uri": {redirect}}
	var out struct {
		Token       string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	const op = "POST /login/oauth/access_token"
	if _, err := s.send(ctx, http.MethodPost, s.web()+"/login/oauth/access_token", op, form, secret.Value{}, &out); err != nil {
		return secret.Value{}, err
	}
	// GitHub answers a refusal with 200 and an error, which the dashboard
	// shows the operator.
	if out.Error != "" || out.Token == "" {
		return secret.Value{}, &Error{Status: http.StatusOK, Op: op, Message: secret.Scrub(strings.TrimSpace(out.Error + ": " + out.Description))}
	}
	return secret.New(out.Token), nil
}

// User returns the user a user token from Exchange belongs to.
func (s Setup) User(ctx context.Context, token secret.Value) (Account, error) {
	var out account
	if _, err := s.send(ctx, http.MethodGet, s.api()+"/user", "", nil, secret.New("Bearer "+token.Reveal()), &out); err != nil {
		return Account{}, err
	}
	if out.ID == 0 || out.Login == "" {
		return Account{}, &Error{Op: "GET /user", Message: "the answer names no user"}
	}
	return out.public(), nil
}

// send makes one request to target, with form as its body when it isn't
// nil and auth as its Authorization header when it isn't empty, and
// decodes a successful answer into out. op names the call, or is empty
// for its method and path. A failure is an *Error, normalized, which
// never carries the form's secrets.
func (s Setup) send(ctx context.Context, method, target, op string, form url.Values, auth secret.Value, out any) (*http.Response, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if op == "" {
		op = method + " " + u.Path
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "silkpool")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if a := auth.Reveal(); a != "" {
		req.Header.Set("Authorization", a)
	}
	resp, err := s.http().Do(req)
	if err != nil {
		return nil, &Error{Op: op, Message: noAnswer(err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &Error{Op: op, Message: "read the answer: " + noAnswer(err)}
	}
	if resp.StatusCode/100 != 2 {
		return resp, failure(op, resp, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp, &Error{Op: op, Message: fmt.Sprintf("couldn't read GitHub's %d answer: %s", resp.StatusCode, scrubbed(err))}
		}
	}
	return resp, nil
}

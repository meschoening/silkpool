// Package github is the Hub's client for GitHub: the calls that carry out
// the effects of the registration core (package registration), plus the
// reads that check a target before every mint, find lost targets, and
// pick the runner version. The decisions behind it are in "Define how an
// operator sets up a Hub and connects GitHub",
// "Define the registration scope model and labels"
// and ADR 0011.
//
// A Client acts for one GitHub connection: a private App and its one
// installation. It signs App JWTs with the App's key and keeps
// installation tokens in memory only, each narrowed to the target it acts
// on and to the product's permissions for that kind of target. The key,
// the JWTs, the tokens and the JIT configs it mints are each a
// secret.Value, so printing, logging or serializing a Client, its Config
// or what it returns never shows one (package secret). What GitHub and
// the network say is scrubbed as the client copies it into an Error, or
// into a sentence of its own, so a secret in it reaches none of its
// callers: not a mint's failure, the Hub's log, or what the Hub shows the
// operator.
//
// Package githubfake is a stateful fake of the same GitHub, and one
// contract suite runs against both, so the fake can't drift
// (docs/testing.md).
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/meschoening/silkpool/internal/secret"
)

// API is github.com's REST API, the only GitHub Silkpool supports.
const API = "https://api.github.com"

// Config is what a Client needs to act for one GitHub connection.
type Config struct {
	// BaseURL is the REST API's root: API when empty, or a fake's URL.
	BaseURL string
	// AppID is the App's ID, and Installation the ID of its installation.
	AppID        int64
	Installation int64
	// Key is the App's private key, in the PEM GitHub hands out.
	Key secret.Value
	// HTTP makes the requests. When nil, a client with a 30 s timeout.
	HTTP *http.Client
	// Now reads the clock, for JWTs and token expiry: time.Now when nil.
	Now func() time.Time
}

// A Client calls GitHub for one GitHub connection. It's safe for
// concurrent use.
//
// Once an answer shows the connection's rate limit is spent, it calls
// GitHub no more until GitHub's reset time: each call fails at once as a
// rate limit (429), without a request.
type Client struct {
	base         string
	appID        int64
	installation int64
	key          *rsa.PrivateKey
	http         *http.Client
	now          func() time.Time

	mu     sync.Mutex
	tokens map[string]token // by scope, see tokenFor
	// limited is when GitHub may be called again after an answer that
	// spent the rate limit, or zero. See backOff.
	limited time.Time
}

// A token is an installation token and when it expires. It lives only in
// a Client's memory.
type token struct {
	value   secret.Value
	expires time.Time
}

// tokenSlack is how long before its expiry a token is replaced, so a
// call never starts with a token about to lapse.
const tokenSlack = 5 * time.Minute

// New returns a Client for c, or an error if c's key can't be parsed.
func New(c Config) (*Client, error) {
	key, err := parseKey(c.Key)
	if err != nil {
		return nil, err
	}
	cl := &Client{
		base:         strings.TrimRight(c.BaseURL, "/"),
		appID:        c.AppID,
		installation: c.Installation,
		key:          key,
		http:         c.HTTP,
		now:          c.Now,
		tokens:       map[string]token{},
	}
	if cl.base == "" {
		cl.base = API
	}
	if cl.http == nil {
		cl.http = &http.Client{Timeout: 30 * time.Second}
	}
	if cl.now == nil {
		cl.now = time.Now
	}
	return cl, nil
}

// parseKey parses an App's private key, in the PEM GitHub hands out.
// Its errors never quote the key.
func parseKey(key secret.Value) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(key.Reveal()))
	if block == nil {
		return nil, errors.New("no PEM block in the App's private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the App's private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the App's private key isn't an RSA key")
	}
	return rk, nil
}

// An Error is a call GitHub failed or refused.
type Error struct {
	// Status is GitHub's HTTP status, normalized: 429 for any rate
	// limit, primary or secondary, and 0 when GitHub couldn't be reached
	// or its answer couldn't be read.
	Status int
	// Op names the call, as "METHOD /path".
	Op string
	// Message is GitHub's message, or why there was no answer. What
	// GitHub or the network wrote in it is scrubbed (package secret), and
	// the client's own words stay as they are.
	Message string
	// token reports that it was the installation token request that
	// failed, before the call itself.
	token bool
}

// tokenFailed reports whether err is a failed installation token
// request.
func tokenFailed(err error) (*Error, bool) {
	e, ok := errors.AsType[*Error](err)
	return e, ok && e.token
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s: no answer from GitHub: %s", e.Op, e.Message)
	}
	return fmt.Sprintf("%s: %d %s", e.Op, e.Status, e.Message)
}

// Status returns err's normalized status (see Error), or 0 if err isn't
// an *Error.
func Status(err error) int {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Status
	}
	return 0
}

// jwtLifetime is how long an App JWT is valid. GitHub refuses one that
// expires more than 10 minutes after it's issued, and iat is backdated
// by a minute against clock drift.
const jwtLifetime = 9 * time.Minute

// jwt returns an App JWT for now.
func (c *Client) jwt() (secret.Value, error) {
	now := c.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": strconv.FormatInt(c.appID, 10),
	})
	if err != nil {
		return secret.Value{}, err
	}
	signed := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return secret.Value{}, err
	}
	return secret.New(signed + "." + base64.RawURLEncoding.EncodeToString(sig)), nil
}

// A scope is what an installation token is narrowed to: the
// repositories it may reach, none meaning all of the installation's, and
// its permissions.
type scope struct {
	repos []string
	perms map[string]string
}

// key names s for the token cache.
func (s scope) key() string {
	var b strings.Builder
	for _, r := range s.repos {
		b.WriteString(strings.ToLower(r))
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, p := range slices.Sorted(maps.Keys(s.perms)) {
		b.WriteString(p + "=" + s.perms[p] + ",")
	}
	return b.String()
}

// metadataOnly is the scope for reads every installation may make: its
// repositories, and public releases.
var metadataOnly = scope{perms: map[string]string{"metadata": "read"}}

// tokenFor returns an installation token for s, minting one if the
// cache holds none with more than tokenSlack left, and reports whether
// it came from the cache. A failure is an *Error that tokenFailed
// recognizes.
func (c *Client) tokenFor(ctx context.Context, s scope) (secret.Value, bool, error) {
	key := s.key()
	c.mu.Lock()
	t, ok := c.tokens[key]
	c.mu.Unlock()
	if ok && t.expires.Sub(c.now()) > tokenSlack {
		return t.value, true, nil
	}
	tok, err := c.mintToken(ctx, s)
	if err != nil {
		if e, ok := errors.AsType[*Error](err); ok {
			e.token = true
		} else {
			err = &Error{Op: "sign the App JWT", Message: err.Error(), token: true}
		}
		return secret.Value{}, false, err
	}
	c.mu.Lock()
	c.tokens[key] = tok
	c.mu.Unlock()
	return tok.value, false, nil
}

// forget drops s's token from the cache, if it's still value.
func (c *Client) forget(s scope, value secret.Value) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens[s.key()].value.Equal(value) {
		delete(c.tokens, s.key())
	}
}

// mintToken asks GitHub for an installation token for s.
func (c *Client) mintToken(ctx context.Context, s scope) (token, error) {
	jwt, err := c.jwt()
	if err != nil {
		return token{}, err
	}
	body := map[string]any{"permissions": s.perms}
	if len(s.repos) > 0 {
		body["repositories"] = s.repos
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", c.installation)
	if _, err := c.call(ctx, bearer(jwt), http.MethodPost, path, body, &out); err != nil {
		return token{}, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return token{}, &Error{Op: "POST " + path, Message: "the answer carries no token"}
	}
	return token{value: secret.New(out.Token), expires: out.ExpiresAt}, nil
}

// bearer returns the Authorization header for an App JWT.
func bearer(jwt secret.Value) secret.Value { return secret.New("Bearer " + jwt.Reveal()) }

// withToken returns the Authorization header for an installation token.
func withToken(tok secret.Value) secret.Value { return secret.New("token " + tok.Reveal()) }

// do makes one call with an installation token for s. If GitHub no
// longer accepts a cached token (401), as when the installation is
// suspended or deleted, it drops it and tries once more with a new one.
func (c *Client) do(ctx context.Context, s scope, method, path string, body, out any) (*http.Response, error) {
	tok, cached, err := c.tokenFor(ctx, s)
	if err != nil {
		return nil, err
	}
	resp, err := c.call(ctx, withToken(tok), method, path, body, out)
	if !cached || Status(err) != http.StatusUnauthorized {
		return resp, err
	}
	c.forget(s, tok)
	if tok, _, err = c.tokenFor(ctx, s); err != nil {
		return nil, err
	}
	return c.call(ctx, withToken(tok), method, path, body, out)
}

// call makes one request to path, or to the absolute URL path when it
// starts with the base URL, a later page GitHub's Link header named,
// with auth as its Authorization header, and decodes a successful answer
// into out when out isn't nil. A failure is an *Error, normalized.
func (c *Client) call(ctx context.Context, auth secret.Value, method, path string, body, out any) (*http.Response, error) {
	target := path
	if !strings.HasPrefix(path, c.base+"/") {
		target = c.base + path
	}
	op := method + " " + opPath(c.base, path)
	if until := c.limitedUntil(); !until.IsZero() {
		return nil, &Error{Status: http.StatusTooManyRequests, Op: op,
			Message: "not sent: the rate limit is spent until " + until.UTC().Format(time.RFC3339)}
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "silkpool")
	req.Header.Set("Authorization", auth.Reveal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Op: op, Message: noAnswer(err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, &Error{Op: op, Message: "read the answer: " + noAnswer(err)}
	}
	if resp.StatusCode/100 != 2 {
		e := failure(op, resp, data)
		c.backOff(resp.Header, e.Status == http.StatusTooManyRequests)
		return resp, e
	}
	c.backOff(resp.Header, false)
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(data, out); err != nil {
			return resp, &Error{Op: op, Message: fmt.Sprintf("couldn't read GitHub's %d answer: %s", resp.StatusCode, scrubbed(err))}
		}
	}
	return resp, nil
}

// scrubbed is err's text with every secret in it scrubbed, for an
// Error's message: what GitHub and the network say in it isn't
// Silkpool's. That includes Go's errors about GitHub's answer, which
// quote parts of it, such as a map's keys or a time it can't parse.
func scrubbed(err error) string { return secret.Scrub(err.Error()) }

// backOff notes when GitHub may be called again, after an answer that
// spent the rate limit: one that's limited, or any with no requests
// remaining. As GitHub's guidance has it, that's Retry-After seconds
// from now if the answer has it, else X-RateLimit-Reset if no requests
// remain, and else a minute from now.
func (c *Client) backOff(h http.Header, limited bool) {
	spent := h.Get("X-RateLimit-Remaining") == "0"
	if !limited && !spent {
		return
	}
	now := c.now()
	until := now.Add(time.Minute)
	if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil && secs >= 0 {
		until = now.Add(time.Duration(secs) * time.Second)
	} else if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && spent {
		until = time.Unix(reset, 0)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.limited) {
		c.limited = until
	}
}

// limitedUntil returns when GitHub may be called again, or zero if it
// may be called now.
func (c *Client) limitedUntil() time.Time {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !now.Before(c.limited) {
		return time.Time{}
	}
	return c.limited
}

// opPath is path's path, without its query, for naming a call. path is
// Silkpool's own, relative to base, or a later page's URL on base, which
// GitHub's Link header named. That one is GitHub's text, so its path is
// scrubbed (package secret) as it's copied into the name, once it's
// decoded.
func opPath(base, path string) string {
	p, linked := strings.CutPrefix(path, base+"/")
	if linked {
		p = "/" + p
	}
	if u, err := url.Parse(p); err == nil {
		p = u.Path
	}
	if linked {
		return secret.Scrub(p)
	}
	return p
}

// noAnswer describes a transport error without the request's URL, which
// carries nothing secret but repeats the Op, and scrubbed: a proxy's or
// the network's words are in it.
func noAnswer(err error) string {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	return scrubbed(err)
}

// failure builds the *Error for a non-2xx answer, with GitHub's message
// scrubbed. Any rate limit reads as 429: GitHub answers a primary one
// with 403 or 429 and no requests remaining, and a secondary one with
// 403 or 429 and a message saying so.
func failure(op string, resp *http.Response, data []byte) *Error {
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &msg) // an unreadable body leaves the message empty
	e := &Error{Status: resp.StatusCode, Op: op, Message: secret.Scrub(msg.Message)}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(e.Message), "rate limit") {
			e.Status = http.StatusTooManyRequests
		}
	}
	return e
}

// list fetches every page of a list at path, which must hold a query,
// into items, following GitHub's Link headers. field is the JSON field
// holding the page's items, or empty for a bare array.
func list[T any](ctx context.Context, c *Client, s scope, path, field string) ([]T, error) {
	var all []T
	next := path
	for next != "" {
		var page []T
		var out any = &page
		var wrapped map[string]json.RawMessage
		if field != "" {
			out = &wrapped
		}
		resp, err := c.do(ctx, s, http.MethodGet, next, nil, out)
		if err != nil {
			return nil, err
		}
		if field != "" {
			if raw, ok := wrapped[field]; ok {
				if err := json.Unmarshal(raw, &page); err != nil {
					return nil, &Error{Op: "GET " + opPath(c.base, next), Message: "couldn't read GitHub's answer: " + scrubbed(err)}
				}
			}
		}
		all = append(all, page...)
		next = nextLink(resp.Header.Get("Link"))
		if next != "" && !strings.HasPrefix(next, c.base+"/") {
			return nil, &Error{Op: "GET " + opPath(c.base, path), Message: "GitHub's next page is on another host"}
		}
		// A request's error would quote GitHub's link whole.
		if _, err := url.Parse(next); err != nil {
			return nil, &Error{Op: "GET " + opPath(c.base, path), Message: "GitHub's next page isn't a URL"}
		}
	}
	return all, nil
}

// nextLink returns the rel="next" URL of a Link header, or "".
func nextLink(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		u, params, ok := strings.Cut(part, ";")
		if !ok {
			continue
		}
		for p := range strings.SplitSeq(params, ";") {
			if strings.TrimSpace(p) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(u), "<>")
			}
		}
	}
	return ""
}

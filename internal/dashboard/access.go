package dashboard

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"sync"
	"time"

	"github.com/meschoening/silkpool/internal/hubops"
	"github.com/meschoening/silkpool/internal/node"
	"github.com/meschoening/silkpool/internal/secret"
)

// The cookies: the setup code, once the setup link has handed it over;
// the session, once the operator has signed in; and the state of a trip
// to GitHub and back, which the manifest's flow and sign-in hand GitHub
// and check when it sends the browser back. Each is HttpOnly, Secure and
// SameSite=Lax, which GitHub's redirects back carry.
const (
	setupCookie   = "silkpool_setup"
	sessionCookie = "silkpool_session"
	stateCookie   = "silkpool_state"
)

// SessionLifetime is how long a sign-in lasts, unless the Hub restarts
// first.
const SessionLifetime = 24 * time.Hour

// stateLifetime is how long a trip to GitHub may take.
const stateLifetime = 30 * time.Minute

func setCookie(w http.ResponseWriter, name string, v secret.Value, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: v.Reveal(), Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

// cookie returns the value of r's cookie name, or the empty secret.
func cookie(r *http.Request, name string) secret.Value {
	c, err := r.Cookie(name)
	if err != nil {
		return secret.Value{}
	}
	return secret.New(c.Value)
}

// sessions are the signed-in sessions, in memory only, by the SHA-256 of
// their cookie's token, so the map holds no token.
type sessions struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[[sha256.Size]byte]session
}

// A session is who signed in, when the session ends, and the
// enrollment code it made last.
type session struct {
	hubops.SignedIn
	ends time.Time
	made madeCode
}

// A madeCode is an enrollment code a session made, by its ID, with its
// text, which Add node shows that session while the code is open. The
// Hub keeps only its hash.
type madeCode struct {
	id   string
	text secret.Value
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{now: now, m: map[[sha256.Size]byte]session{}}
}

func tokenKey(token secret.Value) [sha256.Size]byte { return sha256.Sum256([]byte(token.Reveal())) }

// open starts a session for who, and returns its token and when it
// ends. It forgets the sessions that have ended.
func (s *sessions) open(who hubops.SignedIn) (secret.Value, time.Time) {
	token := newToken()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if !now.Before(v.ends) {
			delete(s.m, k)
		}
	}
	ends := now.Add(SessionLifetime)
	s.m[tokenKey(token)] = session{SignedIn: who, ends: ends}
	return token, ends
}

// get returns the session of token, if it hasn't ended.
func (s *sessions) get(token secret.Value) (session, bool) {
	if token.Reveal() == "" {
		return session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[tokenKey(token)]
	if !ok || !s.now().Before(v.ends) {
		return session{}, false
	}
	return v, true
}

// keep keeps c as the code the session of token made last.
func (s *sessions) keep(token secret.Value, c madeCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[tokenKey(token)]; ok {
		v.made = c
		s.m[tokenKey(token)] = v
	}
}

// end ends the session of token.
func (s *sessions) end(token secret.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, tokenKey(token))
}

// An access is who a request comes from, as far as the dashboard can
// tell, and how far setup has gone.
type access struct {
	progress hubops.SetupProgress
	// who is the operator, signed in, or zero.
	who hubops.SignedIn
	// code is the setup code the browser holds, if it opens setup, and
	// reassign reports one whose sign-in reassigns the operator.
	code     secret.Value
	reassign bool
	// made is the enrollment code the session made last, if any.
	made madeCode
}

// signedIn reports a request from the operator, signed in.
func (a access) signedIn() bool { return a.who.ID != 0 }

// opens reports a request that may take setup's steps: the operator,
// signed in, or, until the operator is recorded, a browser with the
// setup code.
func (a access) opens() bool {
	return a.signedIn() || a.progress.Operator == 0 && a.code.Reveal() != "" && !a.reassign
}

// origin is where a change made by a comes from, for history.
func (a access) origin() node.Origin { return node.Origin{Surface: node.Dashboard, Login: a.who.Login} }

// access returns who r comes from. A session counts only while its
// user is the operator, so reassigning the operator ends the old one's.
func (d *Dashboard) access(ctx context.Context, r *http.Request) (access, error) {
	p, err := d.ops.Setup(ctx)
	if err != nil {
		return access{}, err
	}
	a := access{progress: p}
	if s, ok := d.sessions.get(cookie(r, sessionCookie)); ok && p.Operator != 0 && s.ID == p.Operator {
		a.who, a.made = s.SignedIn, s.made
	}
	if code := cookie(r, setupCookie); code.Reveal() != "" {
		if reassign, err := d.ops.SetupCodeOpens(ctx, code); err == nil {
			a.code, a.reassign = code, reassign
		}
	}
	return a, nil
}

// sameSecret reports whether a and b hold the same non-empty secret.
func sameSecret(a, b secret.Value) bool {
	return a.Reveal() != "" && subtle.ConstantTimeCompare([]byte(a.Reveal()), []byte(b.Reveal())) == 1
}

// newState starts a trip to GitHub: it returns a fresh state and sets it
// as the state cookie, which the trip back must match.
func (d *Dashboard) newState(w http.ResponseWriter) secret.Value {
	state := newToken()
	setCookie(w, stateCookie, state, d.now().Add(stateLifetime))
	return state
}

// backFromGitHub reports whether r, GitHub sending the browser back,
// carries the state the trip started with, and ends the trip.
func backFromGitHub(w http.ResponseWriter, r *http.Request) bool {
	ok := sameSecret(cookie(r, stateCookie), secret.New(r.URL.Query().Get("state")))
	clearCookie(w, stateCookie)
	return ok
}

// Package secret keeps Silkpool's secrets out of everything it prints,
// logs and serializes, on the Hub and on nodes alike. The rules are the
// Redaction section of "Define Silkpool's events, audit trail, notices
// and redaction".
//
// It works two ways:
//
//   - By type. Each secret Silkpool holds is a Value, which prints and
//     serializes as Redacted, so a log line, JSON, diagnostics or a page
//     can't leak one by accident. That covers JIT configs, installation
//     tokens, the App's private key and client secret, the Hub's TLS key,
//     the node key, enrollment codes and the setup code, and GitHub
//     sign-in tokens and session cookies.
//   - By pattern. Text Silkpool didn't write (copied runner logs,
//     listener output and failure details) goes through Copy or Scrub as
//     it's copied, which replace the secrets they recognize with
//     Redacted.
//
// The package is pure: it never reads the clock, sleeps, or does I/O of
// its own.
package secret

import (
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
)

// Redacted is what a Value prints as, and what Copy and Scrub put in
// place of each secret they find.
const Redacted = "[redacted]"

// A Value holds a secret. Reveal is the only way to get the secret back:
// fmt with any verb, encoding/json, encoding.TextMarshaler and log/slog
// all give Redacted. A Value held in an unexported field, which fmt
// prints without calling its methods, prints as a pointer.
//
// Values can't be compared with ==: Equal compares the secrets in
// constant time. The zero Value holds the empty secret.
type Value struct {
	// s points at the secret, because fmt prints a pointer it reaches
	// through an unexported field as its address, never what it points
	// at.
	s *string
	// Two Values can't be compared with ==, which would compare the
	// pointers.
	_ [0]func()
}

// New returns a Value holding s.
func New(s string) Value { return Value{s: &s} }

// Reveal returns the secret, for the one place that has to use it: an
// Authorization header, a JIT config handed to its runner, a key being
// parsed. Never print or log what it returns.
func (v Value) Reveal() string {
	if v.s == nil {
		return ""
	}
	return *v.s
}

// Equal reports whether v and w hold the same secret, in time that
// depends only on their lengths.
func (v Value) Equal(w Value) bool {
	return subtle.ConstantTimeCompare([]byte(v.Reveal()), []byte(w.Reveal())) == 1
}

// Format prints Redacted for every verb, ignoring flags, width and
// precision.
func (Value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Redacted) }

// String returns Redacted.
func (Value) String() string { return Redacted }

// GoString returns Redacted.
func (Value) GoString() string { return Redacted }

// MarshalJSON returns Redacted as a JSON string.
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText returns Redacted.
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// LogValue returns Redacted, for log/slog.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

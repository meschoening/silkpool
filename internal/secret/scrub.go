package secret

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

// CodePrefix starts every code Silkpool makes for someone to copy: the
// enrollment codes and the setup code. The rest of a code is at least
// 22 characters from A–Z, a–z, 0–9, "_" and "-", which a secret of 128
// bits or more in unpadded base64url always fills. That's what lets
// Copy and Scrub find a code in text Silkpool didn't write, the way
// GitHub's prefixes let them find its tokens.
const CodePrefix = "silkpool_"

// The patterns Copy and Scrub replace. A token's minimum length keeps
// words like "ghs_" or "bearer token" in ordinary lines as they are:
// GitHub's tokens run to 36 characters or more after their prefix, and
// no bearer credential is shorter than 16. Nothing marks where a match
// may start, so a token still matches inside a JSON escape (\nghs_…)
// or right after other letters.
var (
	// tokens finds the secrets that sit within a line: GitHub's tokens
	// (ghp_, ghs_, gho_, ghu_, ghr_ and github_pat_), Silkpool's codes,
	// and bearer credentials of any kind. A bearer credential's scheme
	// stays, and only its first submatch, the credential, is replaced.
	tokens = regexp.MustCompile(`(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]{20,}` +
		`|` + CodePrefix + `[A-Za-z0-9_-]{22,}` +
		`|(?i:bearer)[ \t]+([A-Za-z0-9._~+/-]{16,}=*)`)
	// keyBegin and keyEnd mark a PEM private key block: PKCS #1, PKCS #8,
	// EC, OpenSSH, encrypted or PGP.
	keyBegin = regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
	keyEnd   = regexp.MustCompile(`-----END[A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
)

// Scrub returns text with every secret it finds replaced with Redacted,
// as Copy does. It's for text already in memory, such as a failure's
// details.
func Scrub(text string) string {
	var b strings.Builder
	_ = Copy(&b, strings.NewReader(text)) // neither can fail
	return b.String()
}

// Copy copies src to dst line by line, with every secret it finds
// replaced with Redacted: GitHub's tokens, Silkpool's codes, bearer
// credentials and PEM private key blocks. Everything else, line endings
// included, comes through byte for byte.
//
// A private key block becomes one Redacted, from its BEGIN line to its
// END line, with the text before and after its markers kept. A block
// that never ends runs to the end of src, since anything after its
// BEGIN might be the key.
//
// It writes each line to dst as soon as it has scrubbed it, in one
// Write, and holds only that line in memory, so a copy can scrub as it
// goes: bound src to bound a line. It returns the first error from src
// or dst. On an error from src, the line it was reading is dropped,
// since its end might hold the rest of a secret.
func Copy(dst io.Writer, src io.Reader) error {
	r := bufio.NewReader(src)
	var s scrubber
	for {
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		out := s.line(line)
		if err == io.EOF {
			out += s.end()
		}
		if out != "" {
			if _, err := io.WriteString(dst, out); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// A scrubber scrubs one line at a time, so it remembers whether it's
// inside a private key block.
type scrubber struct {
	// inKey reports that a private key block has begun and not ended.
	// The lines inside it are dropped, line endings included, so the
	// block becomes one Redacted.
	inKey bool
	// eol is the line ending of the last line dropped inside a block.
	eol string
}

// line returns line scrubbed, with its line ending, if it has one.
func (s *scrubber) line(line string) string {
	text, eol := line, ""
	if t, ok := strings.CutSuffix(text, "\n"); ok {
		text, eol = t, "\n"
		if t, ok := strings.CutSuffix(text, "\r"); ok {
			text, eol = t, "\r\n"
		}
	}
	var b strings.Builder
	for {
		if s.inKey {
			loc := keyEnd.FindStringIndex(text)
			if loc == nil {
				if line != "" { // the empty read at the end of src isn't a line
					s.eol = eol
				}
				return b.String()
			}
			text, s.inKey = text[loc[1]:], false
			continue
		}
		loc := keyBegin.FindStringIndex(text)
		if loc == nil {
			b.WriteString(scrubTokens(text))
			b.WriteString(eol)
			return b.String()
		}
		b.WriteString(scrubTokens(text[:loc[0]]))
		b.WriteString(Redacted)
		text, s.inKey = text[loc[1]:], true
	}
}

// end returns what finishes the text: a block that never ended ends
// with the line ending of its last line.
func (s *scrubber) end() string {
	if s.inKey {
		return s.eol
	}
	return ""
}

// scrubTokens returns s with each match of tokens replaced.
func scrubTokens(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range tokens.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[0], m[1]
		if m[2] >= 0 {
			start, end = m[2], m[3]
		}
		b.WriteString(s[last:start])
		b.WriteString(Redacted)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

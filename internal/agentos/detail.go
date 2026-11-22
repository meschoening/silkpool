package agentos

import (
	"unicode"
	"unicode/utf8"

	"github.com/meschoening/silkpool/internal/secret"
)

// sentence is err as a failure's detail: a sentence, scrubbed.
func sentence(err error) string {
	d := scrubbed(err)
	r, size := utf8.DecodeRuneInString(d)
	return string(unicode.ToUpper(r)) + d[size:]
}

// scrubbed is err's text with every secret in it scrubbed, for a
// failure's detail or the OS's log: what the machine and GitHub say in
// it isn't Silkpool's.
func scrubbed(err error) string { return secret.Scrub(err.Error()) }

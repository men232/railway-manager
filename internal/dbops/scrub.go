package dbops

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/men232/railway-manager/internal/text"
)

const (
	redacted = "[redacted]"
	// minPiece is the shortest piece of a password that is redacted on its
	// own: a client may echo a password cut short (psql's statement echo).
	minPiece = 8
)

// scrubbedError is an error with every password taken out. It unwraps only to
// the sentinels callers test for, never to the raw error.
type scrubbedError struct {
	msg   string
	cause error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.cause }

// scrub returns err with every secret, its URL-encoded and SQL-quoted forms
// and any piece of 8 characters or more of them replaced by "[redacted]", and
// with control characters removed. Every error leaving this package goes
// through it: client output is untrusted.
func scrub(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	var cause error
	for _, s := range []error{ErrIncomplete, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, s) {
			cause = s
			break
		}
	}
	msg := redact(text.Sanitize(redact(err.Error(), secrets)), secrets)
	return &scrubbedError{msg: msg, cause: cause}
}

// forms are the spellings of a secret a client may print back.
func forms(secrets []string) []string {
	var out []string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		out = append(out, s, encodeURIComponent(s), url.QueryEscape(s), url.PathEscape(s),
			strings.ReplaceAll(s, "'", "''"), redisArg(s))
	}
	return out
}

// redact replaces each secret form in s, and each piece of one at least
// minPiece long (the whole form when it is shorter), by "[redacted]".
func redact(s string, secrets []string) string {
	fs := forms(secrets)
	if len(fs) == 0 || s == "" {
		return s
	}
	hide := make([]bool, len(s))
	for i := range s {
		for _, f := range fs {
			n := longestPiece(s[i:], f)
			if n >= min(minPiece, len(f)) && n > 0 {
				for k := i; k < i+n; k++ {
					hide[k] = true
				}
			}
		}
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if !hide[i] {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString(redacted)
		for i < len(s) && hide[i] {
			i++
		}
	}
	return b.String()
}

// longestPiece is the length of the longest prefix of s that occurs in f.
func longestPiece(s, f string) int {
	n := 0
	for n < len(s) && n < len(f) && strings.Contains(f, s[:n+1]) {
		n++
	}
	return n
}

// encodeURIComponent percent-encodes s as JavaScript's encodeURIComponent
// does, so the Go side and a mongosh script agree on the encoding.
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

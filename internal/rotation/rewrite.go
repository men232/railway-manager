package rotation

import (
	"errors"
	"net/url"
	"strings"

	"github.com/men232/railway-manager/internal/text"
)

// RewriteURL replaces the password of a credential URL whose user is user:
// scheme, user, host list and everything after the
// hosts stay byte-identical; the new password is percent-encoded with only
// RFC 3986 unreserved characters left as they are, which userinfo always
// accepts. A URL whose password boundary is unclear (an "@" followed by
// "?" or "#" that the old password may hold) is refused. Errors never carry
// the URL or either password.
func RewriteURL(rawURL, user, newPassword string) (string, error) {
	if newPassword == "" {
		return "", errors.New("rewrite URL: the new password is empty")
	}
	spans, gotUser := passwordSpans(rawURL)
	switch {
	case len(spans) == 0:
		return "", errors.New("rewrite URL: not a credential URL")
	case len(spans) > 1:
		return "", errors.New("rewrite URL: the password's end is unclear (an @ before ? or #) · set this place by hand")
	case gotUser != user:
		return "", errors.New("rewrite URL: the URL's user is not " + text.Sanitize(user))
	}
	sp := spans[0]
	return rawURL[:sp[0]] + encodeUserinfo(newPassword) + rawURL[sp[1]:], nil
}

// RebaseURL is the rewrite of current — what a place holds now — with
// password, when current is still a clear credential URL of user naming the
// hosts of rewritten, the URL its rewrite was made as (the rewrite follows
// the current URL); ok is false otherwise. Hosts compare
// in order, case-insensitively. It never returns an error: nothing it reads
// is reported.
func RebaseURL(current, rewritten, user, password string) (string, bool) {
	cu, ok := parseCredURL(current)
	if !ok || cu.unclear || cu.user != user {
		return "", false
	}
	ru, ok := parseCredURL(rewritten)
	if !ok || len(ru.hosts) != len(cu.hosts) {
		return "", false
	}
	for i := range cu.hosts {
		if !strings.EqualFold(cu.hosts[i], ru.hosts[i]) {
			return "", false
		}
	}
	next, err := RewriteURL(current, user, password)
	return next, err == nil
}

// IsCredURL reports whether value reads as a credential URL
// (scheme://user:password@hosts…).
func IsCredURL(value string) bool {
	_, ok := parseCredURL(value)
	return ok
}

// passwordSpans are the [start, end) byte ranges of a credential URL's
// password: one when clear, the lazy and the greedy reading when unclear,
// none when value is not a credential URL; and the URL's user.
func passwordSpans(value string) ([][2]int, string) {
	l := credURLLazy.FindStringSubmatchIndex(value)
	g := credURLGreedy.FindStringSubmatchIndex(value)
	if l == nil || g == nil {
		return nil, ""
	}
	// Group 2 is the user, group 3 the host list: the password runs from
	// after "user:" to the "@" before the hosts.
	spans := [][2]int{{l[5] + 1, l[6] - 1}}
	if g[6] != l[6] {
		spans = append(spans, [2]int{g[5] + 1, g[6] - 1})
	}
	return spans, value[l[4]:l[5]]
}

// passwordHashes hashes every reading of a credential URL's password, as
// written and percent-decoded.
func passwordHashes(value string) []string {
	spans, _ := passwordSpans(value)
	var out []string
	for _, sp := range spans {
		pw := value[sp[0]:sp[1]]
		out = append(out, hashOf(pw))
		if d, err := url.PathUnescape(pw); err == nil && d != pw {
			out = append(out, hashOf(d))
		}
	}
	return out
}

// encodeUserinfo percent-encodes every byte but the RFC 3986 unreserved
// characters (ALPHA, DIGIT, "-", ".", "_", "~"), upper-case hex.
func encodeUserinfo(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) * 3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

package tui

import (
	"strings"
	"unicode"
)

// LinkStyled wraps styled in an OSC 8 hyperlink to url. styled is text the
// caller has already sanitized and then styled: styling a link with lipgloss
// would break its escapes apart, so the style goes inside. A url that is
// empty or holds a control or space character gives the text alone, so
// nothing from outside can end the escape early. Callers build url from
// values they have validated. ansi.StringWidth ignores the escapes, so a link
// takes the cells of its text.
func LinkStyled(url, styled string) string {
	if url == "" || strings.IndexFunc(url, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return styled
	}
	return "\x1b]8;;" + url + "\x1b\\" + styled + "\x1b]8;;\x1b\\"
}

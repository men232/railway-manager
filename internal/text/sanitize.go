// Package text holds string helpers shared by the TUI and CLI.
package text

import (
	"strings"
	"unicode"
)

// Sanitize removes every ANSI escape sequence (CSI, OSC, DCS and other string
// sequences, two-byte escapes), every other C0/C1 control character and the
// invisible characters that reorder or hide text (bidi, ZWSP, BOM, soft
// hyphen, tags, Hangul fillers…). Tab, newline and the line and paragraph
// separators become one space each. Use it on every string that comes from outside
// before it is drawn.
func Sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\t' || r == '\n' || r == 0x2028 || r == 0x2029:
			b.WriteByte(' ')
		case r == 0x1b:
			i = skipEscape(rs, i)
		case r == 0x9b: // C1 CSI
			i = skipCSI(rs, i+1)
		case r == 0x9d || r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f: // C1 OSC, DCS, SOS, PM, APC
			i = skipString(rs, i+1)
		case r < 0x20 || (r >= 0x7f && r <= 0x9f) || invisible(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// skipEscape returns the index of the last rune of the sequence starting at
// the ESC at rs[i].
func skipEscape(rs []rune, i int) int {
	if i+1 >= len(rs) {
		return i
	}
	switch rs[i+1] {
	case '[':
		return skipCSI(rs, i+2)
	case ']', 'P', 'X', '^', '_':
		return skipString(rs, i+2)
	}
	if rs[i+1] < 0x20 {
		return i // lone ESC; the next control or escape is judged on its own
	}
	// Two-byte escape; intermediates (0x20-0x2f) may precede the final byte.
	j := i + 1
	for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x2f {
		j++
	}
	if j >= len(rs) {
		return len(rs) - 1
	}
	if rs[j] > 0x7e {
		return j - 1 // not a final byte: keep the rune as text
	}
	return j
}

// skipCSI consumes parameter and intermediate bytes up to the final byte
// (0x40-0x7e), returning its index.
func skipCSI(rs []rune, j int) int {
	for ; j < len(rs); j++ {
		if rs[j] >= 0x40 && rs[j] <= 0x7e {
			return j
		}
		if rs[j] < 0x20 || rs[j] > 0x7e {
			return j - 1 // malformed: keep this rune, it is judged on its own
		}
	}
	return len(rs) - 1
}

// skipString consumes a string sequence up to BEL, ST (ESC \) or C1 ST.
func skipString(rs []rune, j int) int {
	for ; j < len(rs); j++ {
		switch {
		case rs[j] == 0x07 || rs[j] == 0x9c:
			return j
		case rs[j] == 0x1b:
			if j+1 < len(rs) && rs[j+1] == '\\' {
				return j + 1
			}
			return j - 1 // a new escape starts; let the caller see it
		}
	}
	return len(rs) - 1
}

// invisible reports the characters that draw nothing yet reorder or hide
// text, so one name can pass for another: every format control (bidi
// embeddings, overrides and isolates, direction marks, zero width space, BOM,
// soft hyphen, word joiner, invisible operators, tags, interlinear
// annotation) but the zero width joiner (emoji) and non-joiner (Persian
// words), and the Hangul fillers, which are letters that show as blanks.
func invisible(r rune) bool {
	switch r {
	case 0x200c, 0x200d:
		return false
	case 0x115f, 0x1160, 0x3164, 0xffa0:
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

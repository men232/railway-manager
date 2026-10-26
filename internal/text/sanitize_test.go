package text

import "testing"

func TestSanitize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\x1b[31mb\x1b]8;;x\x07c\nd\te", "abc d e"},
		{"ok", "ok"},
		{"\x1b[2J", ""},
		{"x\x1b]0;title\x1b\\y", "xy"},
		{"a\x00b\x7fc\rd", "abcd"},
		{"a\u009b31mb\u009d0;t\u0007c", "abc"},
		{"héllo ✓", "héllo ✓"},
		{"trail\x1b", "trail"},
		{"\x1b[é hello", "é hello"},
		{"\x1b\x1b[2J", ""},
		{"a\x1b[1;3\x00b", "ab"},
		{"\x1bPdcs data\x1b\\z", "z"},
		// bidi overrides and isolates, marks, zero width space, BOM
		{"a\u202ab\u202bc\u202cd\u202de\u202ef", "abcdef"},
		{"a\u2066b\u2067c\u2068d\u2069e", "abcde"},
		{"a\u200eb\u200fc\u200bd\ufeffe", "abcde"},
		// the Arabic letter mark is a bidi mark too
		{"a\u061cb", "ab"},
		// the zero width joiner builds emoji: kept
		{"\U0001F468\u200d\U0001F4BB", "\U0001F468\u200d\U0001F4BB"},
		// the zero width non-joiner shapes Persian words: kept
		{"\u0645\u06cc\u200c\u062e\u0648\u0627\u0645", "\u0645\u06cc\u200c\u062e\u0648\u0627\u0645"},
		// other invisible characters that make one name look like another:
		// soft hyphen, word joiner, invisible operators, Mongolian vowel
		// separator, tag characters, interlinear annotation, Hangul fillers
		{"a\u00adp\u2060i\u2062x\u180ey", "apixy"},
		{"a\U000E0041\U000E007Fb\ufff9c\ufffbd", "abcd"},
		{"a\u115fb\u1160c\u3164d\uffa0e", "abcde"},
		// line and paragraph separators end a line, as a newline does
		{"a\u2028b\u2029c", "a b c"},
	}
	for _, c := range cases {
		if got := Sanitize(c.in); got != c.want {
			t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

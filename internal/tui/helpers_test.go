package tui

import "testing"

func TestDriftText(t *testing.T) {
	for _, tt := range []struct {
		nerd     bool
		in, want string
	}{
		{false, "", ""},
		{false, "variable gone", "⚠ drifted: variable gone"},
		{false, "value \x1b[31mchanged\r\nsecond line", "⚠ drifted: value changed"},
		{true, "", ""},
		{true, "variable gone", " drifted: variable gone"},
	} {
		if got := DriftText(Icons{Nerd: tt.nerd}, tt.in); got != tt.want {
			t.Errorf("DriftText(nerd %v, %q) = %q, want %q", tt.nerd, tt.in, got, tt.want)
		}
	}
}

func isPUA(r rune) bool { return r >= 0xE000 && r <= 0xF8FF || r >= 0xF0000 }

// The run mark and the drift mark have a Nerd form; the plain set emits no
// Private Use Area rune.
func TestIconsRunMarkAndDrift(t *testing.T) {
	plain, nerd := Icons{}, Icons{Nerd: true}
	for name, g := range map[string][2]string{
		"RunMark": {plain.RunMark(), nerd.RunMark()},
		"Drift":   {plain.Drift(), nerd.Drift()},
	} {
		if g[0] == "" || g[1] == "" || g[0] == g[1] {
			t.Errorf("%s: plain %q nerd %q, want two forms", name, g[0], g[1])
		}
		for _, r := range g[0] {
			if isPUA(r) {
				t.Errorf("%s: plain form has a Private Use rune %U", name, r)
			}
		}
	}
	if plain.RunMark() != "◆" || plain.Drift() != "⚠" {
		t.Errorf("plain forms changed: %q %q", plain.RunMark(), plain.Drift())
	}
}

package version

import (
	"strings"
	"testing"
)

func TestFallbacks(t *testing.T) {
	// Built by go test without ldflags: Version comes from the embedded file.
	if Version != "v"+strings.TrimSpace(embedded) {
		t.Errorf("Version = %q, want v + VERSION (%q)", Version, embedded)
	}
	if Commit == "" || BuildDate == "" {
		t.Errorf("Commit %q / BuildDate %q must fall back to unknown, never empty", Commit, BuildDate)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name                string
		v, c, d, file       string
		settings            map[string]string
		wantV, wantC, wantD string
	}{
		{"ldflags win", "v9.9.9", "abc1234", "2026-01-01", "0.1.0", map[string]string{"vcs.revision": "ffffffffff"}, "v9.9.9", "abc1234", "2026-01-01"},
		{"file and vcs", "", "", "", "0.1.0\n", map[string]string{"vcs.revision": "0123456789", "vcs.time": "2026-10-24T10:00:00Z"}, "v0.1.0", "0123456", "2026-10-24"},
		{"nothing", "", "", "", " ", nil, "v0.0.0-dev", "unknown", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, c, d := resolve(tc.v, tc.c, tc.d, tc.file, tc.settings)
			if v != tc.wantV || c != tc.wantC || d != tc.wantD {
				t.Errorf("resolve = %q %q %q, want %q %q %q", v, c, d, tc.wantV, tc.wantC, tc.wantD)
			}
		})
	}
}

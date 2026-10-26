// Package tuitest holds helpers for TUI tests: golden files, secret guards,
// and a deterministic color profile.
package tuitest

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

var update = flag.Bool("update", false, "rewrite golden files")

// shotsEnv names a directory: when set, screen tests render in true color and
// Golden also writes each frame there, colors kept, for the README screenshots
// (see docs/assets/shots).
const shotsEnv = "RM_SHOTS"

// Profile is the color profile screen tests build their theme with: Ascii,
// or TrueColor while RM_SHOTS is set. Goldens are the same either way, since
// Golden strips ANSI sequences.
func Profile() termenv.Profile {
	if os.Getenv(shotsEnv) != "" {
		return termenv.TrueColor
	}
	return termenv.Ascii
}

// Init makes rendering deterministic: no colors, no terminal queries. Call it
// from TestMain.
func Init() {
	lipgloss.SetColorProfile(termenv.Ascii)
	lipgloss.SetHasDarkBackground(true)
}

// Plain strips ANSI sequences from a rendered frame, except while RM_SHOTS is
// set: then the colors stay, for Golden to write out.
func Plain(frame string) string {
	if os.Getenv(shotsEnv) != "" {
		return frame
	}
	return ansi.Strip(frame)
}

// Golden compares got with testdata/<name> next to the calling test, or
// rewrites that file under -update. ANSI sequences are stripped first: a theme
// built for Ascii (NO_COLOR) still emits attributes such as reverse and bold,
// and goldens record the text and layout only.
func Golden(t *testing.T, name, got string) {
	t.Helper()
	if dir := os.Getenv(shotsEnv); dir != "" {
		wd, _ := os.Getwd()
		shot := filepath.Join(dir, filepath.Base(wd)+"_"+strings.TrimSuffix(name, ".golden")+".ansi")
		if err := os.WriteFile(shot, []byte(got), 0o644); err != nil {
			t.Fatalf("shot %s: %v", name, err)
		}
	}
	got = ansi.Strip(got)
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("golden %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run with -update)", name, err)
	}
	if string(want) != got {
		t.Errorf("golden %s differs (run with -update to accept)\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

// NoSecrets fails when the frame contains any of the secrets. The failure
// names the secret by position only.
func NoSecrets(t *testing.T, frame string, secrets ...string) {
	t.Helper()
	for i, s := range secrets {
		if s != "" && strings.Contains(frame, s) {
			t.Errorf("frame contains secret #%d", i)
		}
	}
}

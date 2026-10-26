package tui_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/men232/railway-manager/internal/tui"
)

// The refusal for a run executing elsewhere names its pid and the lock
// file only — never the run directory, whose journal records the change.
func TestRunElsewhereText(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	got := tui.RunElsewhere(filepath.Join(home, ".railway-manager"), "r1", 4242)
	want := "run r1 is running in another window (pid 4242) · if pid 4242 has exited, remove ~/.railway-manager/runs/r1/lock, then restart"
	if got != want {
		t.Errorf("RunElsewhere = %q, want %q", got, want)
	}
}

// A corrupt run's recovery moves its directory aside, never removes it.
func TestRunRecoveryText(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	got := tui.RunRecovery(filepath.Join(home, ".railway-manager"), "r1")
	want := "move ~/.railway-manager/runs/r1 out of ~/.railway-manager/runs (keep it: it may record a database password change), then restart"
	if got != want {
		t.Errorf("RunRecovery = %q, want %q", got, want)
	}
}

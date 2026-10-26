package workdir

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPrivate(t *testing.T) {
	dir := t.TempDir()
	if err := Private(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".metadata_never_index")); err != nil {
		t.Errorf("no Spotlight marker: %v", err)
	}
	if err := Private(dir); err != nil { // idempotent
		t.Fatal(err)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	out, err := exec.Command("tmutil", "isexcluded", dir).CombinedOutput()
	if err != nil {
		t.Skipf("tmutil: %v", err)
	}
	if !strings.Contains(string(out), "[Excluded]") {
		t.Errorf("not excluded from Time Machine: %s", out)
	}
}

func TestCleanTemps(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old, fresh := now.Add(-time.Hour), now
	write := func(rel string, at time.Time) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := []string{
		write("changeset.json.123.tmp", old),
		write(filepath.Join("runs", "r1", "changeset.json.9.tmp"), old),
	}
	kept := []string{
		write("changeset.json.456.tmp", fresh), // a write in progress
		write("changeset.json", old),
		write(filepath.Join("runs", "r1", "journal.json"), old),
	}
	n, err := CleanTemps(dir, time.Minute, now)
	if err != nil || n != len(gone) {
		t.Errorf("CleanTemps = %d, %v", n, err)
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed", p)
		}
	}
}

// A work dir whose path holds glob characters is still cleaned, and
// nothing outside it is touched.
func TestCleanTempsGlobInPath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "work[1]")
	sibling := filepath.Join(root, "work1")
	old := time.Now().Add(-time.Hour)
	var temps []string
	for _, d := range []string{filepath.Join(dir, "runs", "r1"), filepath.Join(sibling, "runs", "r1")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "changeset.json.1.tmp")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		temps = append(temps, p)
	}
	n, err := CleanTemps(dir, time.Minute, time.Now())
	if err != nil || n != 1 {
		t.Errorf("CleanTemps = %d, %v", n, err)
	}
	if _, err := os.Stat(temps[0]); !os.IsNotExist(err) {
		t.Error("the work dir's temp file is left")
	}
	if _, err := os.Stat(temps[1]); err != nil {
		t.Error("a temp file outside the work dir was removed")
	}
}

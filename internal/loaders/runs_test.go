package loaders_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/loaders"
)

func TestRunsSkipsCorruptRun(t *testing.T) {
	dir := t.TempDir()
	for id, journal := range map[string]string{
		"good": `{"runId":"good","state":"finished"}`,
		"bad":  `{"state":zxSECRET`,
	} {
		d := filepath.Join(dir, "runs", id)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for f, body := range map[string]string{"journal.json": journal, "changeset.json": `{}`} {
			if err := os.WriteFile(filepath.Join(d, f), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	m := loaders.Runs(&apply.Runs{Dir: dir})().(loaders.RunsMsg)
	if len(m.Runs) != 1 || m.Runs[0].ID != "good" {
		t.Errorf("runs = %+v", m.Runs)
	}
	if len(m.Warnings) != 1 || m.Warnings[0] != "run bad: journal is corrupt" {
		t.Errorf("warnings = %q", m.Warnings)
	}
	if len(m.Corrupt) != 1 || m.Corrupt[0] != "bad" {
		t.Errorf("corrupt = %q", m.Corrupt)
	}
}

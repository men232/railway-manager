package apply

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestJournalPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs", "run123")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "journal.json")
	newValue := "n3w-v4lue-xyz"
	c := ch("a", "var", pl("e1", "s1"))
	c.New = json.RawMessage(`"` + newValue + `"`)

	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.RunID != "run123" || len(j.Steps) != 0 || len(j.Changes) != 0 {
		t.Fatalf("new journal = %+v", j)
	}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := StepRecord{Status: "done", PatchID: "patch-1", At: at, Data: json.RawMessage(`{"pids":[1,2]}`)}
	if err := j.Set("patch:e1", rec); err != nil {
		t.Fatal(err)
	}
	if err := j.Set("redeploy:e1/s1", StepRecord{Status: "failed", Error: "boom", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := j.SetOutcome(c.ID, Outcome{Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := j.SetState("failed"); err != nil {
		t.Fatal(err)
	}
	if got := j.Step("patch:e1"); got.PatchID != "patch-1" {
		t.Errorf("Step = %+v", got)
	}

	j2, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if j2.State != "failed" || j2.RunID != "run123" || !j2.Created.Equal(j.Created) {
		t.Errorf("reopened = %+v", j2)
	}
	if !reflect.DeepEqual(j2.Steps, j.Steps) {
		t.Errorf("Steps = %+v, want %+v", j2.Steps, j.Steps)
	}
	if !reflect.DeepEqual(j2.Changes, j.Changes) {
		t.Errorf("Changes = %+v", j2.Changes)
	}
	if got := j2.Outcome("a"); got.Status != "done" {
		t.Errorf("Outcome = %+v", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), newValue) {
		t.Errorf("journal contains a change value")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestJournalMissingIsNotWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if _, err := OpenJournal(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("OpenJournal created the file: %v", err)
	}
}

func TestJournalCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(path); err == nil {
		t.Error("corrupt journal opened")
	}
}

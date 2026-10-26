package apply

import (
	"path/filepath"
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
)

// A marker change (variable.keep) has no step: the run is done with it once
// the run finished, and keeps it until then.
func TestReportMarker(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "r1", "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	keep := changeset.Change{ID: "k1", Kind: "variable.keep"}
	other := changeset.Change{ID: "o1", Kind: "unknown"}
	cs := changeset.ChangeSet{Changes: []changeset.Change{keep, other}}
	marker := func(c changeset.Change) bool { return c.Kind == "variable.keep" }
	tr := newTracker(Graph{Steps: map[string]Step{}})

	rep := report(cs, j, tr, nil, marker, true)
	if _, ok := rep.Done["k1"]; !ok {
		t.Errorf("finished: keep not done: %+v", rep)
	}
	if _, ok := rep.Kept["o1"]; !ok {
		t.Errorf("a change no step serves is still kept: %+v", rep)
	}
	rep = report(cs, j, tr, nil, marker, false)
	if _, ok := rep.Kept["k1"]; !ok {
		t.Errorf("unfinished: keep must stay: %+v", rep)
	}
}

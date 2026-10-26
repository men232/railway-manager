package view_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/view"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func catalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{{ID: "e1", Name: "dev", CanAccess: true, ServiceIDs: []string{"api", "db", "web"}}},
		Services:     []railway.Service{{ID: "api", Name: "api"}, {ID: "db", Name: "db"}, {ID: "web", Name: "web"}},
	}}}
}

func vars() []railway.VarRecord {
	return []railway.VarRecord{
		{EnvID: "e1", ServiceID: "db", Name: "URL", Unrendered: "pg-one"},
		{EnvID: "e1", ServiceID: "api", Name: "DB", Unrendered: "${{db.URL}}"},
		{EnvID: "e1", ServiceID: "api", Name: "PLAIN", Unrendered: "one"},
		{EnvID: "e1", ServiceID: "api", Name: "OLD", Unrendered: "x"},
	}
}

func pl(svc string) domain.Place { return domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: svc} }

type fx struct {
	st    *store.Store
	v     *view.Views
	cs    *changeset.ChangeSet
	calls int
}

func newFx(t *testing.T) *fx {
	t.Helper()
	st := store.New()
	st.ReplaceCatalog(catalog())
	st.ReplaceVariables("p1", vars())
	reg := kinds.NewRegistry()
	f := &fx{st: st, cs: &changeset.ChangeSet{}}
	f.v = &view.Views{
		Store: st, Reg: reg,
		Compile: func(cs changeset.ChangeSet) (apply.Graph, error) {
			f.calls++
			return apply.Compile(cs, st, reg,
				func(envID string, changes []changeset.Change) apply.Step {
					return kinds.NewPatchStep(envID, changes, reg, "p1")
				}, kinds.NewRedeployStep)
		},
	}
	return f
}

func (f *fx) set(t *testing.T, svc, name, value string) {
	t.Helper()
	c, err := kinds.SetVariable(f.st, f.cs.Changes, pl(svc), name, value, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(c, nil); err != nil {
		t.Fatal(err)
	}
}

func TestServicesPending(t *testing.T) {
	f := newFx(t)
	f.set(t, "api", "PLAIN", "two")
	f.set(t, "api", "NEW", "n")
	rows := f.v.Services(f.cs, []string{"e1"})
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, r := range rows {
		want := 0
		if r.Service == "api" {
			want = 2
		}
		if r.Pending != want {
			t.Errorf("%s pending = %d, want %d", r.Service, r.Pending, want)
		}
	}
}

func TestPlaceVariablesOverlay(t *testing.T) {
	f := newFx(t)
	f.set(t, "api", "PLAIN", "a value of 24 chars!!!!!")
	f.set(t, "api", "ADDED", "n")
	del, err := kinds.DeleteVariable(f.st, pl("api"), "OLD", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(del, nil); err != nil {
		t.Fatal(err)
	}
	rows := f.v.PlaceVariables(f.cs, pl("api"))
	got := map[string]view.VarRow{}
	var names []string
	for _, r := range rows {
		got[r.Name] = r
		names = append(names, r.Name)
	}
	if !slices.IsSorted(names) || len(names) != 4 {
		t.Fatalf("names = %v", names)
	}
	if s := got["PLAIN"].Overlay.Short; s != "→ 24 chars" {
		t.Errorf("edited overlay = %q", s)
	}
	if got["ADDED"].Overlay.Short != "+ added" || got["ADDED"].Change == nil {
		t.Errorf("added = %+v", got["ADDED"])
	}
	if got["OLD"].Overlay.Short != "✗ delete" {
		t.Errorf("deleted = %+v", got["OLD"].Overlay)
	}
	if got["DB"].Change != nil || got["DB"].Length != len("${{db.URL}}") {
		t.Errorf("untouched = %+v", got["DB"])
	}
}

func TestPlaceVariablesKeepsPendingEditOfVanishedVariable(t *testing.T) {
	f := newFx(t)
	f.set(t, "api", "PLAIN", "two")
	del, err := kinds.DeleteVariable(f.st, pl("api"), "OLD", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(del, nil); err != nil {
		t.Fatal(err)
	}
	// both variables vanish from Railway
	var kept []railway.VarRecord
	for _, r := range vars() {
		if r.Name != "PLAIN" && r.Name != "OLD" {
			kept = append(kept, r)
		}
	}
	f.st.ReplaceVariables("p1", kept)
	got := map[string]view.VarRow{}
	for _, r := range f.v.PlaceVariables(f.cs, pl("api")) {
		got[r.Name] = r
	}
	for _, n := range []string{"PLAIN", "OLD"} {
		r, ok := got[n]
		if !ok || !r.Missing || r.Change == nil {
			t.Errorf("%s = %+v, ok=%v", n, r, ok)
		}
	}
	if got["DB"].Missing {
		t.Error("untouched row flagged missing")
	}
}

func TestCacheRecomputesOnRevision(t *testing.T) {
	f := newFx(t)
	f.set(t, "db", "URL", "pg-two")
	f.v.ChangeSet(f.cs, nil)
	f.v.ChangeSet(f.cs, nil)
	if f.calls != 1 {
		t.Fatalf("calls = %d after two reads", f.calls)
	}
	f.cs.ToggleRedeploy("e1/api")
	f.v.ChangeSet(f.cs, nil)
	if f.calls != 2 {
		t.Errorf("calls = %d after change-set rev bump", f.calls)
	}
	f.st.ReplaceVariables("p1", vars())
	f.v.ChangeSet(f.cs, nil)
	if f.calls != 3 {
		t.Errorf("calls = %d after store rev bump", f.calls)
	}
	f.v.ChangeSet(f.cs, nil)
	if f.calls != 3 {
		t.Errorf("calls = %d, want no recompute", f.calls)
	}
}

func TestChangeSetViewRedeploys(t *testing.T) {
	f := newFx(t)
	f.set(t, "db", "URL", "pg-two")
	cv := f.v.ChangeSet(f.cs, nil)
	if cv.Err != "" {
		t.Fatal(cv.Err)
	}
	on := func(cv view.ChangeSetView) map[string]bool {
		m := map[string]bool{}
		for _, r := range cv.Redeploys {
			m[r.PlaceKey] = r.On
		}
		return m
	}
	m := on(cv)
	if !m["e1/db"] || !m["e1/api"] || len(m) != 2 {
		t.Fatalf("redeploys = %v", cv.Redeploys)
	}
	if len(cv.Groups) != 1 || cv.Groups[0].Label != "Proj/dev/db" || cv.Groups[0].Changes[0].Overlay.Short == "" {
		t.Errorf("groups = %+v", cv.Groups)
	}
	f.cs.ToggleRedeploy("e1/api")
	m = on(f.v.ChangeSet(f.cs, nil))
	if !m["e1/db"] {
		t.Errorf("db should stay on: %v", m)
	}
	if on, ok := m["e1/api"]; !ok || on {
		t.Errorf("api should be listed off: %v", m)
	}
}

// A change whose own data is refused (as one loaded corrupt from disk) is
// marked with the reason and left out of the redeploys, since it cannot be
// applied; the valid ones are untouched.
func TestChangeSetViewMarksInvalid(t *testing.T) {
	f := newFx(t)
	f.set(t, "db", "URL", "pg-two")
	f.cs.Changes = append(f.cs.Changes,
		changeset.Change{ID: "bad1", Kind: "service.teleport", Place: pl("web"), Target: "x"},
		changeset.Change{ID: "bad2", Kind: "variable.set", Place: pl("web"), Target: "Y", New: []byte(`{`)},
	)
	f.cs.Rev++
	cv := f.v.ChangeSet(f.cs, nil)
	if cv.Err != "" {
		t.Fatalf("Err = %q: invalid changes must not reach the compile", cv.Err)
	}
	invalid := map[string]string{}
	for _, g := range cv.Groups {
		for _, r := range g.Changes {
			invalid[r.Change.ID] = r.Invalid
		}
	}
	if invalid["bad1"] != "unknown change kind service.teleport" {
		t.Errorf("unknown kind: Invalid = %q", invalid["bad1"])
	}
	if !strings.HasPrefix(invalid["bad2"], "variable.set: bad change data") {
		t.Errorf("corrupt set: Invalid = %q", invalid["bad2"])
	}
	if id := f.cs.Changes[0].ID; invalid[id] != "" {
		t.Errorf("valid change marked: %q", invalid[id])
	}
	for _, r := range cv.Redeploys {
		if r.PlaceKey == "e1/web" {
			t.Errorf("an invalid change brought a redeploy: %+v", cv.Redeploys)
		}
	}
}

// drifted puts a change of PLAIN (or name) at api, then lets Railway move:
// recs replace the store's variables. The inventory is marked read unless
// noInventory.
func driftFx(t *testing.T, edit func(f *fx), recs []railway.VarRecord, inventory bool) *fx {
	t.Helper()
	f := newFx(t)
	edit(f)
	f.st.ReplaceVariables("p1", recs)
	if inventory {
		f.st.FinishLoad(store.InventoryKey("p1"), now)
	}
	return f
}

func withVar(name, value string) []railway.VarRecord {
	var out []railway.VarRecord
	found := false
	for _, r := range vars() {
		if r.ServiceID == "api" && r.Name == name {
			found = true
			if value == "" {
				continue
			}
			r.Unrendered = value
		}
		out = append(out, r)
	}
	if !found && value != "" {
		out = append(out, railway.VarRecord{EnvID: "e1", ServiceID: "api", Name: name, Unrendered: value})
	}
	return out
}

// drift is the Drifted of the change on name at api in the change set view
// and in the place's variables.
func drift(t *testing.T, f *fx, name string) (row, place string) {
	t.Helper()
	found := false
	for _, g := range f.v.ChangeSet(f.cs, nil).Groups {
		for _, r := range g.Changes {
			if r.Name == name {
				row, found = r.Drifted, true
			}
		}
	}
	if !found {
		t.Fatalf("no change row for %s", name)
	}
	for _, r := range f.v.PlaceVariables(f.cs, pl("api")) {
		if r.Name == name {
			place = r.Drifted
		}
	}
	return row, place
}

func TestDriftValueChanged(t *testing.T) {
	f := driftFx(t, func(f *fx) { f.set(t, "api", "PLAIN", "two") }, withVar("PLAIN", "changed-meanwhile"), true)
	row, place := drift(t, f, "PLAIN")
	if row != "value changed on Railway" || place != row {
		t.Errorf("drifted = %q / %q", row, place)
	}
	// the change is kept and still applies: it brings its redeploy
	if cv := f.v.ChangeSet(f.cs, nil); cv.Err != "" || len(cv.Redeploys) == 0 {
		t.Errorf("a drifted change must still apply: %+v", cv)
	}
}

func TestDriftCreatedMeanwhile(t *testing.T) {
	f := driftFx(t, func(f *fx) { f.set(t, "api", "NEW", "n") }, withVar("NEW", "someone-else"), true)
	if row, place := drift(t, f, "NEW"); row != "variable now exists" || place != row {
		t.Errorf("drifted = %q / %q", row, place)
	}
}

func TestDriftGone(t *testing.T) {
	f := driftFx(t, func(f *fx) { f.set(t, "api", "PLAIN", "two") }, withVar("PLAIN", ""), true)
	if row, place := drift(t, f, "PLAIN"); row != "variable gone" || place != row {
		t.Errorf("drifted = %q / %q", row, place)
	}
}

func TestNoDriftBeforeInventory(t *testing.T) {
	f := driftFx(t, func(f *fx) { f.set(t, "api", "PLAIN", "two") }, withVar("PLAIN", "changed-meanwhile"), false)
	if row, place := drift(t, f, "PLAIN"); row != "" || place != "" {
		t.Errorf("drift before the inventory read: %q / %q", row, place)
	}
	// the inventory read lands: the memoised views see it
	f.st.FinishLoad(store.InventoryKey("p1"), now)
	if row, place := drift(t, f, "PLAIN"); row != "value changed on Railway" || place != row {
		t.Errorf("after the inventory read: %q / %q", row, place)
	}
}

// Railway already holding the new value, a delete whose variable is gone, an
// untouched base and an invalid change are not flagged.
func TestNoDriftWhenNotDrifted(t *testing.T) {
	f := driftFx(t, func(f *fx) {
		f.set(t, "api", "PLAIN", "two")
		f.set(t, "api", "NEW", "n")
		del, err := kinds.DeleteVariable(f.st, pl("api"), "OLD", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.cs.Put(del, nil); err != nil {
			t.Fatal(err)
		}
	}, withVar("PLAIN", "two"), true)
	f.st.ReplaceVariables("p1", func() []railway.VarRecord {
		var out []railway.VarRecord
		for _, r := range withVar("PLAIN", "two") {
			if r.Name != "OLD" {
				out = append(out, r)
			}
		}
		return out
	}())
	for _, n := range []string{"PLAIN", "NEW", "OLD"} {
		if row, place := drift(t, f, n); row != "" || place != "" {
			t.Errorf("%s drifted = %q / %q", n, row, place)
		}
	}
	// a corrupt change is invalid, never drifted
	f.cs.Changes = append(f.cs.Changes, changeset.Change{ID: "bad", Kind: "variable.set", Place: pl("api"), Target: "DB", New: []byte(`{`)})
	f.cs.Rev++
	for _, g := range f.v.ChangeSet(f.cs, nil).Groups {
		for _, r := range g.Changes {
			if r.Change.ID == "bad" && (r.Invalid == "" || r.Drifted != "") {
				t.Errorf("invalid row = %+v", r)
			}
		}
	}
}

// An inventory read that skipped the change's environment (deselected, no
// access, a partial read) holds nothing at its place: the change is not
// flagged gone.
func TestNoDriftWhenPlaceNotRead(t *testing.T) {
	f := driftFx(t, func(f *fx) { f.set(t, "api", "PLAIN", "two") }, nil, true)
	if row, place := drift(t, f, "PLAIN"); row != "" || place != "" {
		t.Errorf("env absent from the read: drifted = %q / %q", row, place)
	}
	var dbOnly []railway.VarRecord
	for _, r := range vars() {
		if r.ServiceID == "db" {
			dbOnly = append(dbOnly, r)
		}
	}
	f.st.ReplaceVariables("p1", dbOnly)
	if row, place := drift(t, f, "PLAIN"); row != "" || place != "" {
		t.Errorf("place absent from the read: drifted = %q / %q", row, place)
	}
}

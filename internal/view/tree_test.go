package view_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/view"
)

// Long values (20+ characters) group across names; short ones only within
// a name. None of these may ever reach a row.
const (
	longA  = "long-value-number-one-xx"
	longB  = "long-value-number-two-xx"
	shortA = "s-one"
	shortB = "s-two"
)

// treeCatalog has one project with two environments: "dev" (e1) and
// "alpha" (e2), which sorts first. Both hold api and web.
func treeCatalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{
			{ID: "e1", Name: "dev", CanAccess: true, ServiceIDs: []string{"api", "web"}},
			{ID: "e2", Name: "alpha", CanAccess: true, ServiceIDs: []string{"web", "api"}},
		},
		Services: []railway.Service{{ID: "api", Name: "api"}, {ID: "web", Name: "web"}},
	}}}
}

func treeVars() []railway.VarRecord {
	return []railway.VarRecord{
		{EnvID: "e1", ServiceID: "api", Name: "KEY", Unrendered: longA, UpdatedAt: "2026-10-20T10:00:00"},
		{EnvID: "e1", ServiceID: "web", Name: "KEY", Unrendered: longB},
		{EnvID: "e2", ServiceID: "api", Name: "KEY", Unrendered: longA},
		{EnvID: "e2", ServiceID: "web", Name: "OTHER_KEY", Unrendered: longA},
		{EnvID: "e1", ServiceID: "api", Name: "USER", Unrendered: shortA},
		{EnvID: "e1", ServiceID: "web", Name: "PASS", Unrendered: shortA},
		{EnvID: "e1", ServiceID: "", Name: "SHARED", Unrendered: shortB},
		{EnvID: "e1", ServiceID: "web", Name: "SEALED", Unrendered: "", Sealed: true},
		{EnvID: "e2", ServiceID: "web", Name: "SEALED", Unrendered: "", Sealed: true},
		{EnvID: "e1", ServiceID: "api", Name: "RAILWAY_ENV", Unrendered: shortB},
		{EnvID: "e2", ServiceID: "api", Name: "RAILWAY_ENV", Unrendered: shortB},
		{EnvID: "e1", ServiceID: "web", Name: "API", Unrendered: "${{api.KEY}}"},
	}
}

var bothEnvs = []string{"e1", "e2"}

func tpl(env, svc string) domain.Place {
	return domain.Place{ProjectID: "p1", EnvID: env, ServiceID: svc}
}

func newTreeFx(t *testing.T) *fx {
	t.Helper()
	f := newFx(t)
	f.st.ReplaceCatalog(treeCatalog())
	f.st.ReplaceVariables("p1", treeVars())
	return f
}

func (f *fx) setAt(t *testing.T, p domain.Place, name, value string) {
	t.Helper()
	c, err := kinds.SetVariable(f.st, f.cs.Changes, p, name, value, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(c, nil); err != nil {
		t.Fatal(err)
	}
}

func group(t *testing.T, gs []view.GroupRow, key string) view.GroupRow {
	t.Helper()
	for _, g := range gs {
		if g.Key == key {
			return g
		}
	}
	var keys []string
	for _, g := range gs {
		keys = append(keys, g.Key)
	}
	t.Fatalf("no group %q in %v", key, keys)
	return view.GroupRow{}
}

func placeKeys(g view.GroupRow) []string {
	var out []string
	for _, p := range g.Places {
		out = append(out, p.Place.Key()+"/"+p.Name)
	}
	return out
}

func TestTreeByNameCountsValues(t *testing.T) {
	f := newTreeFx(t)
	gs := f.v.Tree(f.cs, bothEnvs, nil, view.ByName)
	var names []string
	for _, g := range gs {
		names = append(names, g.Label)
	}
	if !slices.IsSorted(names) {
		t.Errorf("groups not sorted by name: %v", names)
	}
	g := group(t, gs, "name:KEY")
	if g.Label != "KEY" || !slices.Equal(g.Names, []string{"KEY"}) {
		t.Errorf("label %q names %v", g.Label, g.Names)
	}
	// alpha (e2) sorts before dev (e1); services by name
	want := []string{"e2/api/KEY", "e1/api/KEY", "e1/web/KEY"}
	if got := placeKeys(g); !slices.Equal(got, want) {
		t.Errorf("places = %v, want %v", got, want)
	}
	if g.Values != 2 || g.Pending != 0 {
		t.Errorf("values %d pending %d, want 2 0", g.Values, g.Pending)
	}
	if !slices.Equal(g.Envs, []string{"Proj/alpha", "Proj/dev"}) {
		t.Errorf("envs = %v", g.Envs)
	}
	p := g.Places[1]
	if p.Env != "Proj/dev" || p.Service != "api" || p.LocationID != "e1/api/KEY" || p.Length != len(longA) {
		t.Errorf("place = %+v", p)
	}
	if !p.UpdatedAt.Equal(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("updated = %v", p.UpdatedAt)
	}
	if sh := group(t, gs, "name:SHARED"); sh.Places[0].Service != "shared" || !sh.Places[0].Place.Shared() {
		t.Errorf("shared place = %+v", sh.Places[0])
	}
	if api := group(t, gs, "name:API").Places[0]; api.Ref != "api.KEY" {
		t.Errorf("ref = %q", api.Ref)
	}
}

func TestTreeByNameCountsPending(t *testing.T) {
	f := newTreeFx(t)
	f.setAt(t, tpl("e1", "api"), "KEY", longB) // a value the group already holds
	g := group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByName), "name:KEY")
	if g.Values != 2 || g.Pending != 1 {
		t.Errorf("same value: values %d pending %d, want 2 1", g.Values, g.Pending)
	}
	f.setAt(t, tpl("e2", "api"), "KEY", "a brand new long value!!")
	g = group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByName), "name:KEY")
	if g.Values != 3 || g.Pending != 2 {
		t.Errorf("new value: values %d pending %d, want 3 2", g.Values, g.Pending)
	}
	p := g.Places[0]
	if p.Pending == nil || p.Overlay.Short != "→ 24 chars" || p.Overlay.Tone != "new" {
		t.Errorf("pending place = %+v", p)
	}
}

func TestTreeByValueGroupsAcrossNames(t *testing.T) {
	f := newTreeFx(t)
	gs := f.v.Tree(f.cs, bothEnvs, nil, view.ByValue)
	fpA := domain.Fingerprint("KEY", longA, false, true, "e1", "api")
	g := group(t, gs, "fp:"+fpA)
	if !slices.Equal(g.Names, []string{"KEY", "OTHER_KEY"}) || g.Label != "KEY = OTHER_KEY" {
		t.Errorf("names %v label %q", g.Names, g.Label)
	}
	if len(g.Places) != 3 || g.Values != 1 {
		t.Errorf("places %v values %d", placeKeys(g), g.Values)
	}
	// the most places first
	if gs[0].Key != g.Key {
		t.Errorf("first group = %q", gs[0].Key)
	}
	// a short value groups only within its name
	group(t, gs, "fp:"+domain.Fingerprint("USER", shortA, false, true, "", ""))
	group(t, gs, "fp:"+domain.Fingerprint("PASS", shortA, false, true, "", ""))
}

func TestTreeByValueSealedAlone(t *testing.T) {
	f := newTreeFx(t)
	gs := f.v.Tree(f.cs, bothEnvs, nil, view.ByValue)
	alone := 0
	for i, g := range gs {
		special := strings.HasPrefix(g.Label, "RAILWAY_") || g.Label == "SEALED"
		if !special {
			if alone > 0 {
				t.Errorf("group %q after the one-place groups (at %d)", g.Label, i)
			}
			continue
		}
		alone++
		if len(g.Places) != 1 {
			t.Errorf("%s: %d places, want 1", g.Key, len(g.Places))
		}
		if g.Label == "SEALED" && !g.Places[0].Sealed {
			t.Errorf("%s not sealed", g.Key)
		}
	}
	if alone != 4 {
		t.Errorf("one-place groups = %d, want 4", alone)
	}
	keys := map[string]bool{}
	for _, g := range gs {
		if keys[g.Key] {
			t.Errorf("duplicate key %q", g.Key)
		}
		keys[g.Key] = true
	}
	for i := 1; i < len(gs); i++ {
		a, b := gs[i-1], gs[i]
		sa := strings.HasPrefix(a.Label, "RAILWAY_") || a.Label == "SEALED"
		sb := strings.HasPrefix(b.Label, "RAILWAY_") || b.Label == "SEALED"
		if sa == sb && (len(a.Places) < len(b.Places) || (len(a.Places) == len(b.Places) && a.Names[0] > b.Names[0])) {
			t.Errorf("order: %q (%d) before %q (%d)", a.Label, len(a.Places), b.Label, len(b.Places))
		}
	}
}

func TestTreeScopedToPlace(t *testing.T) {
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
	f.cs.Changes = append(f.cs.Changes, changeset.Change{ID: "bad", Kind: "variable.set", Place: pl("api"), Target: "BROKEN", New: []byte(`{`)})
	f.cs.Rev++
	// PLAIN drifts on Railway and OLD vanishes from it
	var recs []railway.VarRecord
	for _, r := range withVar("PLAIN", "changed-meanwhile") {
		if r.Name != "OLD" {
			recs = append(recs, r)
		}
	}
	f.st.ReplaceVariables("p1", recs)
	f.st.FinishLoad(store.InventoryKey("p1"), now)

	want := f.v.PlaceVariables(f.cs, pl("api"))
	scope := pl("api")
	gs := f.v.Tree(f.cs, nil, &scope, view.ByName)
	var got []view.PlaceRow
	for _, g := range gs {
		got = append(got, g.Places...)
	}
	if len(got) != len(want) {
		t.Fatalf("tree has %d places, PlaceVariables %d", len(got), len(want))
	}
	sawDrift, sawInvalid, sawMissing := false, false, false
	for i, w := range want {
		g := got[i]
		if g.Place != scope || g.Name != w.Name || g.LocationID != w.LocationID || g.Overlay != w.Overlay ||
			g.Missing != w.Missing || g.Drifted != w.Drifted || g.Invalid != w.Invalid || g.Sealed != w.Sealed ||
			g.Length != w.Length || (g.Pending == nil) != (w.Change == nil) {
			t.Errorf("row %d: tree %+v, PlaceVariables %+v", i, g, w)
		}
		if g.Pending != nil && g.Pending.ID != w.Change.ID {
			t.Errorf("row %d: pending %s, want %s", i, g.Pending.ID, w.Change.ID)
		}
		sawDrift = sawDrift || w.Drifted != ""
		sawInvalid = sawInvalid || w.Invalid != ""
		sawMissing = sawMissing || w.Missing
		if w.Invalid != "" && w.Drifted != "" {
			t.Errorf("%s: invalid and drifted", w.Name)
		}
	}
	if !sawDrift || !sawInvalid || !sawMissing {
		t.Errorf("fixture lacks drift (%v), invalid (%v) or missing (%v)", sawDrift, sawInvalid, sawMissing)
	}
}

func TestTreeLettersStable(t *testing.T) {
	f := newTreeFx(t)
	letters := func() []int {
		g := group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByName), "name:KEY")
		var out []int
		for _, p := range g.Places {
			out = append(out, p.Letter)
		}
		return out
	}
	// e2/api longA, e1/api longA, e1/web longB
	want := []int{0, 0, 1}
	for range 5 {
		if got := letters(); !slices.Equal(got, want) {
			t.Fatalf("letters = %v, want %v", got, want)
		}
	}
	f.setAt(t, tpl("e1", "web"), "KEY", "another long value here!")
	if got := letters(); !slices.Equal(got, want) {
		t.Errorf("after a pending edit letters = %v, want %v", got, want)
	}
	f.setAt(t, tpl("e2", "web"), "KEY", longA)
	g := group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByName), "name:KEY")
	if p := g.Places[1]; p.Place.Key() != "e2/web" || p.Letter != -1 || p.Fingerprint != "" {
		t.Errorf("pending add = %+v, want no letter and no fingerprint", p)
	}
}

func TestTreeByFromHistory(t *testing.T) {
	f := newTreeFx(t)
	by := func(env, svc, name string) string {
		t.Helper()
		g := group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByName), "name:"+name)
		for _, p := range g.Places {
			if p.Place.EnvID == env && p.Place.ServiceID == svc {
				return p.By
			}
		}
		t.Fatalf("no place %s/%s for %s", env, svc, name)
		return ""
	}
	if got := by("e1", "api", "KEY"); got != "…" {
		t.Errorf("unread: %q", got)
	}
	f.st.SetMembers("p1", []railway.Member{{ID: "u-alice-0001", Name: "Alice"}, {ID: "u-bob-00002", Name: "Bob"}})
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		{ID: "h0", Object: "Variable", Names: []string{"USER"}, ServiceIDs: []string{"api"}, Actor: ""},
		{ID: "h1", Object: "Variable", Names: []string{"KEY"}, ServiceIDs: []string{"api"}, Actor: "u-alice-0001"},
		{ID: "h2", Object: "Variable", Names: []string{"KEY", "SHARED"}, ServiceIDs: []string{"api"}, Actor: "u-bob-00002"},
		{ID: "h3", Object: "Variable", Names: []string{"SHARED"}, Actor: "u-bob-00002"},
		{ID: "h4", Object: "Variable", Names: []string{"PASS"}, ServiceIDs: []string{"web"}, Actor: "stranger-principal"},
	}})
	cases := []struct{ env, svc, name, want string }{
		{"e1", "api", "KEY", "Alice"},     // the newest matching entry
		{"e1", "web", "KEY", "?"},         // no entry for web
		{"e2", "api", "KEY", "…"},         // e2 not read
		{"e1", "", "SHARED", "Bob"},       // shared: an entry without services
		{"e1", "api", "USER", "?"},        // an entry without an actor
		{"e1", "web", "PASS", "stranger"}, // not a member: MemberName's short id
	}
	for _, c := range cases {
		if got := by(c.env, c.svc, c.name); got != c.want {
			t.Errorf("%s/%s %s: by = %q, want %q", c.env, c.svc, c.name, got, c.want)
		}
	}
}

func TestTreePendingAdd(t *testing.T) {
	f := newTreeFx(t)
	f.setAt(t, tpl("e2", "web"), "NEW_KEY", longA)
	g := group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByName), "name:NEW_KEY")
	if len(g.Places) != 1 || g.Pending != 1 || g.Values != 1 {
		t.Fatalf("group = %+v", g)
	}
	p := g.Places[0]
	if p.Pending == nil || p.Overlay.Short != "+ added" || p.Place != tpl("e2", "web") || p.Service != "web" || p.Missing {
		t.Errorf("place = %+v", p)
	}
	// by value it joins the group of the value it will hold
	fpA := domain.Fingerprint("KEY", longA, false, true, "e1", "api")
	byVal := group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByValue), "fp:"+fpA)
	if !slices.Contains(byVal.Names, "NEW_KEY") || len(byVal.Places) != 4 || byVal.Pending != 1 {
		t.Errorf("by value = %v names %v pending %d", placeKeys(byVal), byVal.Names, byVal.Pending)
	}
	// a pending delete of a variable that never existed is not a place
	del, err := kinds.DeleteVariable(f.st, tpl("e1", "api"), "NOPE", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(del, nil); err != nil {
		t.Fatal(err)
	}
	for _, g := range f.v.Tree(f.cs, bothEnvs, nil, view.ByName) {
		if g.Label == "NOPE" {
			t.Errorf("delete of an absent variable listed: %+v", g)
		}
	}
}

func TestTreeNeverHoldsValues(t *testing.T) {
	f := newTreeFx(t)
	f.setAt(t, tpl("e1", "api"), "KEY", "pending-secret-value-1234")
	f.setAt(t, tpl("e1", "web"), "FRESH", "pending-short")
	secrets := []string{longA, longB, shortA, shortB, "pending-secret-value-1234", "pending-short", "${{"}
	for _, g := range []view.Grouping{view.ByName, view.ByValue} {
		for _, gr := range f.v.Tree(f.cs, bothEnvs, nil, g) {
			dump := fmt.Sprintf("%+v", gr)
			for _, p := range gr.Places {
				if p.Pending != nil {
					if len(p.Pending.New) != 0 || p.Pending.Base.Value != "" {
						t.Errorf("%s: pending change holds a value", p.LocationID)
					}
					dump += fmt.Sprintf("%+v", *p.Pending)
				}
			}
			for _, s := range secrets {
				if strings.Contains(dump, s) {
					t.Errorf("grouping %d: a row holds %q", g, s)
				}
			}
		}
	}
	// the change set itself is untouched
	if len(f.cs.Changes[0].New) == 0 {
		t.Error("the tree stripped the live change")
	}
}

func TestTreeMemo(t *testing.T) {
	f := newTreeFx(t)
	f.v.Tree(f.cs, bothEnvs, nil, view.ByName)
	f.v.Tree(f.cs, bothEnvs, nil, view.ByName)
	if n := f.v.TreeBuilds(); n != 1 {
		t.Fatalf("builds = %d after two equal calls", n)
	}
	steps := []struct {
		name string
		do   func()
	}{
		{"grouping", func() { f.v.Tree(f.cs, bothEnvs, nil, view.ByValue) }},
		{"envs", func() { f.v.Tree(f.cs, []string{"e1"}, nil, view.ByValue) }},
		{"scope", func() { s := tpl("e1", "api"); f.v.Tree(f.cs, []string{"e1"}, &s, view.ByValue) }},
		{"change set", func() { f.setAt(t, tpl("e1", "api"), "KEY", longB); f.v.Tree(f.cs, []string{"e1"}, nil, view.ByValue) }},
		{"history", func() {
			f.st.AppendHistory("e1", "", railway.HistoryPage{})
			f.v.Tree(f.cs, []string{"e1"}, nil, view.ByValue)
		}},
		{"members", func() { f.st.SetMembers("p1", nil); f.v.Tree(f.cs, []string{"e1"}, nil, view.ByValue) }},
		{"variables", func() { f.st.ReplaceVariables("p1", treeVars()); f.v.Tree(f.cs, []string{"e1"}, nil, view.ByValue) }},
		{"inventory read", func() {
			f.st.FinishLoad(store.InventoryKey("p1"), now)
			f.v.Tree(f.cs, []string{"e1"}, nil, view.ByValue)
		}},
	}
	want := 1
	for _, s := range steps {
		s.do()
		want++
		if n := f.v.TreeBuilds(); n != want {
			t.Errorf("%s: builds = %d, want %d", s.name, n, want)
		}
	}
}

func TestTreeByOnlyFromVariableEntries(t *testing.T) {
	f := newTreeFx(t)
	f.st.SetMembers("p1", []railway.Member{{ID: "u-alice-0001", Name: "Alice"}, {ID: "u-bob-00002", Name: "Bob"}})
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		{ID: "h1", Object: "Service", Names: []string{"KEY"}, ServiceIDs: []string{"api"}, Actor: "u-bob-00002"},
		{ID: "h2", Object: "Volume", Names: []string{"SHARED"}, Actor: "u-bob-00002"},
		{ID: "h3", Object: "Variable", Names: []string{"KEY"}, ServiceIDs: []string{"api"}, Actor: "u-alice-0001"},
	}})
	gs := f.v.Tree(f.cs, []string{"e1"}, nil, view.ByName)
	if by := group(t, gs, "name:KEY").Places[0].By; by != "Alice" {
		t.Errorf("KEY by = %q, want Alice (a Service entry must not count)", by)
	}
	if by := group(t, gs, "name:SHARED").Places[0].By; by != "?" {
		t.Errorf("SHARED by = %q, want ? (a Volume entry must not count)", by)
	}
}

func TestTreeSealedHasNoLetterNorValue(t *testing.T) {
	f := newTreeFx(t)
	g := group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByName), "name:SEALED")
	if len(g.Places) != 2 || g.Values != 0 {
		t.Errorf("places %d values %d, want 2 0", len(g.Places), g.Values)
	}
	for _, p := range g.Places {
		if !p.Sealed || p.Letter != -1 {
			t.Errorf("%s: sealed %v letter %d, want no letter", p.LocationID, p.Sealed, p.Letter)
		}
	}
	// a pending new value of a sealed variable is unreadable: not counted
	f.setAt(t, tpl("e1", "web"), "SEALED", "a new sealed long value!!")
	if g := group(t, f.v.Tree(f.cs, bothEnvs, nil, view.ByName), "name:SEALED"); g.Values != 0 || g.Pending != 1 {
		t.Errorf("after edit: values %d pending %d, want 0 1", g.Values, g.Pending)
	}
}

// UpdatedAt reads Railway's zoneless format, RFC 3339, and is zero for
// anything else.
func TestTreeUpdatedAtFormats(t *testing.T) {
	f := newFx(t)
	f.st.ReplaceCatalog(treeCatalog())
	f.st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "api", Name: "A", Unrendered: "a", UpdatedAt: "2026-10-20T10:00:00"},
		{EnvID: "e1", ServiceID: "api", Name: "B", Unrendered: "b", UpdatedAt: "2026-10-20T12:00:00+02:00"},
		{EnvID: "e1", ServiceID: "api", Name: "C", Unrendered: "c", UpdatedAt: ""},
		{EnvID: "e1", ServiceID: "api", Name: "D", Unrendered: "d", UpdatedAt: "yesterday"},
	})
	gs := f.v.Tree(f.cs, []string{"e1"}, nil, view.ByName)
	want := map[string]time.Time{
		"name:A": time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
		"name:B": time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
		"name:C": {},
		"name:D": {},
	}
	for k, w := range want {
		if got := group(t, gs, k).Places[0].UpdatedAt; !got.Equal(w) {
			t.Errorf("%s: updated = %v, want %v", k, got, w)
		}
	}
}

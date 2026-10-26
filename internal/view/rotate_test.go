package view_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/view"
)

// Rotation fixture values: none may ever reach a row.
const (
	rotJWT     = "jwt-old-secret-value-0123456789"
	rotApp     = "app-old-secret-value-0123456789"
	rotRoot    = "mongo-root-password-0123456789"
	rotNew     = "new-secret-value-24chars"
	rotAddedV1 = "added-value-one-0123456789"
)

// rotCatalog: "prod" (e1) is listed before "alpha" (e2), which sorts first
// by name.
func rotCatalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{
			{ID: "e1", Name: "prod", CanAccess: true, ServiceIDs: []string{"api", "web", "worker", "mongo", "mongo2"}},
			{ID: "e2", Name: "alpha", CanAccess: true, ServiceIDs: []string{"api"}},
		},
		Services: []railway.Service{
			{ID: "api", Name: "api"}, {ID: "web", Name: "web"}, {ID: "worker", Name: "worker"},
			{ID: "mongo", Name: "mongo"}, {ID: "mongo2", Name: "mongo2"},
		},
	}}}
}

func rotVars() []railway.VarRecord {
	return []railway.VarRecord{
		{EnvID: "e1", ServiceID: "api", Name: "JWT_SECRET", Unrendered: rotJWT},
		{EnvID: "e1", ServiceID: "web", Name: "JWT_SECRET", Unrendered: rotJWT},
		{EnvID: "e2", ServiceID: "api", Name: "JWT_SECRET", Unrendered: rotJWT},
		{EnvID: "e1", ServiceID: "api", Name: "APP_SECRET", Unrendered: rotApp},
		{EnvID: "e1", ServiceID: "mongo2", Name: "APP_SECRET", Unrendered: rotApp},
		{EnvID: "e1", ServiceID: "mongo", Name: "MONGO_INITDB_ROOT_PASSWORD", Unrendered: rotRoot},
		{EnvID: "e1", ServiceID: "mongo2", Name: "MONGO_INITDB_ROOT_PASSWORD", Unrendered: "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"},
		{EnvID: "e1", ServiceID: "worker", Name: "USES", Unrendered: "${{api.JWT_SECRET}}"},
		{EnvID: "e1", ServiceID: "web", Name: "CHAIN", Unrendered: "${{worker.USES}}"},
		{EnvID: "e1", ServiceID: "mongo2", Name: "JWT", Unrendered: "${{api.JWT_SECRET}}"},
		// A replica node: a database place however its members are counted.
		{EnvID: "e1", ServiceID: "mongo2", Name: "REPLICA_SET_NAME", Unrendered: "rs0"},
	}
}

var rotOpts = rotation.Options{EnvIDs: []string{"e1", "e2"}}

func newRotFx(t *testing.T) *fx {
	t.Helper()
	f := newFx(t)
	f.st.ReplaceCatalog(rotCatalog())
	f.st.ReplaceVariables("p1", rotVars())
	return f
}

// rotateSet puts a Rotate-made variable.set.
func (f *fx) rotateSet(t *testing.T, p domain.Place, name, value, group string) {
	t.Helper()
	c, err := kinds.SetVariable(f.st, f.cs.Changes, p, name, value, now)
	if err != nil {
		t.Fatal(err)
	}
	c.Group = group
	c.Origin = &changeset.Origin{Producer: "rotate", Row: group}
	if err := f.cs.Put(c, nil); err != nil {
		t.Fatal(err)
	}
}

func rotRow(t *testing.T, rows []view.RotateRow, label string) view.RotateRow {
	t.Helper()
	for _, r := range rows {
		if r.Label == label {
			return r
		}
	}
	var labels []string
	for _, r := range rows {
		labels = append(labels, r.Label)
	}
	t.Fatalf("no row %q in %v", label, labels)
	return view.RotateRow{}
}

func rotPlace(t *testing.T, r view.RotateRow, loc string) view.RotatePlace {
	t.Helper()
	for _, p := range r.Places {
		if p.Loc.ID == loc {
			return p
		}
	}
	t.Fatalf("no place %q in row %q", loc, r.Label)
	return view.RotatePlace{}
}

func assertNoRotValues(t *testing.T, rows []view.RotateRow) {
	t.Helper()
	dump := fmt.Sprintf("%+v", rows)
	for _, s := range []string{rotJWT, rotApp, rotRoot, rotNew, rotAddedV1} {
		if strings.Contains(dump, s) {
			t.Errorf("a value leaked into the rows")
		}
	}
}

func TestRotateDecisions(t *testing.T) {
	f := newRotFx(t)
	jwtFP := f.st.Variable("e1/api/JWT_SECRET").Fingerprint
	f.rotateSet(t, tpl("e1", "api"), "JWT_SECRET", rotNew, rotation.GroupKey(jwtFP))

	rows := f.v.Rotate(f.cs, rotOpts, []string{"e1", "e2"}, view.RotateRuns{})
	r := rotRow(t, rows, "JWT_SECRET")
	if r.Class != rotation.ClassGen || len(r.Places) != 3 {
		t.Fatalf("row class %v places %d, want GEN 3", r.Class, len(r.Places))
	}
	if r.Decided != 1 || r.Decidable != 3 {
		t.Errorf("decided %d/%d, want 1/3", r.Decided, r.Decidable)
	}
	p := rotPlace(t, r, "e1/api/JWT_SECRET")
	if p.Pending == nil || p.Overlay.Short != "→ 24 chars" || p.Letter != 0 || p.ReadOnly || p.DB || p.Drifted != "" {
		t.Errorf("decided place = %+v", p)
	}
	if p.Pending != nil && (p.Pending.New != nil || p.Pending.Base.Value != "") {
		t.Errorf("pending change keeps its values")
	}
	if q := rotPlace(t, r, "e1/web/JWT_SECRET"); q.Pending != nil || q.Overlay.Short != "" {
		t.Errorf("undecided place = %+v", q)
	}
	// store order: prod before alpha
	if !slices.Equal(r.Envs, []string{"Proj/prod", "Proj/alpha"}) {
		t.Errorf("envs = %v", r.Envs)
	}
	assertNoRotValues(t, rows)
}

func TestRotateForeignChange(t *testing.T) {
	f := newRotFx(t)
	c, err := kinds.DeleteVariable(f.st, tpl("e1", "web"), "JWT_SECRET", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(c, nil); err != nil {
		t.Fatal(err)
	}
	r := rotRow(t, f.v.Rotate(f.cs, rotOpts, []string{"e1", "e2"}, view.RotateRuns{}), "JWT_SECRET")
	if r.Decided != 1 {
		t.Errorf("decided = %d, want 1", r.Decided)
	}
	p := rotPlace(t, r, "e1/web/JWT_SECRET")
	if p.Pending == nil || p.Pending.Kind != "variable.delete" || p.Pending.Origin != nil {
		t.Errorf("place pending = %+v", p.Pending)
	}
}

func TestRotateReadOnlyAndDB(t *testing.T) {
	f := newRotFx(t)
	rows := f.v.Rotate(f.cs, rotOpts, []string{"e1"}, view.RotateRuns{})
	jwt := rotRow(t, rows, "JWT_SECRET")
	if p := rotPlace(t, jwt, "e2/api/JWT_SECRET"); !p.ReadOnly || p.DB {
		t.Errorf("alpha place = %+v, want read only", p)
	}
	if p := rotPlace(t, jwt, "e1/api/JWT_SECRET"); p.ReadOnly {
		t.Errorf("prod place read only")
	}
	if jwt.Decidable != 2 {
		t.Errorf("JWT decidable = %d, want 2", jwt.Decidable)
	}
	app := rotRow(t, rows, "APP_SECRET")
	if p := rotPlace(t, app, "e1/mongo2/APP_SECRET"); !p.DB || p.ReadOnly {
		t.Errorf("mongo member place = %+v, want DB", p)
	}
	if app.Decidable != 1 {
		t.Errorf("APP decidable = %d, want 1", app.Decidable)
	}
	// foreign changes on a read-only place and on a database place are
	// shown but not counted as decided
	for _, at := range []struct {
		p    domain.Place
		name string
	}{{tpl("e2", "api"), "JWT_SECRET"}, {tpl("e1", "mongo2"), "APP_SECRET"}} {
		c, err := kinds.DeleteVariable(f.st, at.p, at.name, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.cs.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	rows = f.v.Rotate(f.cs, rotOpts, []string{"e1"}, view.RotateRuns{})
	jwt, app = rotRow(t, rows, "JWT_SECRET"), rotRow(t, rows, "APP_SECRET")
	if p := rotPlace(t, jwt, "e2/api/JWT_SECRET"); p.Pending == nil {
		t.Errorf("read-only place lost its pending change")
	}
	if p := rotPlace(t, app, "e1/mongo2/APP_SECRET"); p.Pending == nil {
		t.Errorf("database place lost its pending change")
	}
	for _, r := range rows {
		if r.Decided != 0 || r.Decided > r.Decidable {
			t.Errorf("row %s decided %d/%d, want 0", r.Label, r.Decided, r.Decidable)
		}
	}
	// no write environments: nothing is writable
	for _, r := range f.v.Rotate(f.cs, rotOpts, nil, view.RotateRuns{}) {
		if r.Decidable != 0 {
			t.Errorf("row %s decidable %d with no write envs", r.Label, r.Decidable)
		}
	}
	// a password row with a Problem (root user unknown) takes no decision
	root := rotRow(t, rows, "mongo root")
	if root.Password == nil || root.Problem == "" || root.Decidable != 0 {
		t.Errorf("password row %+v decidable = %d", root.Password, root.Decidable)
	}
}

func TestRotateAddedRow(t *testing.T) {
	f := newRotFx(t)
	g := rotation.AddGroupKey("NEW_KEY")
	f.rotateSet(t, tpl("e1", "api"), "NEW_KEY", rotAddedV1, g)
	f.rotateSet(t, tpl("e1", "web"), "NEW_KEY", rotAddedV1, g)
	f.set(t, "api", "OTHER_NEW", "plain") // a Variables add: no row

	rows := f.v.Rotate(f.cs, rotOpts, []string{"e1"}, view.RotateRuns{})
	var added []view.RotateRow
	for _, r := range rows {
		if r.Section == rotation.SectionAdded {
			added = append(added, r)
		}
	}
	if len(added) != 1 {
		t.Fatalf("added rows = %d, want 1", len(added))
	}
	r := added[0]
	if r.Label != "NEW_KEY" || len(r.Places) != 2 || r.Decided != 2 || r.Decidable != 2 {
		t.Errorf("added row %q places %d decided %d/%d", r.Label, len(r.Places), r.Decided, r.Decidable)
	}
	for _, p := range r.Places {
		if p.Letter != -1 || p.Pending == nil || p.Loc.Name != "NEW_KEY" {
			t.Errorf("added place = %+v", p)
		}
	}
	// sections stay in order
	if !slices.IsSortedFunc(rows, func(a, b view.RotateRow) int { return int(a.Section) - int(b.Section) }) {
		t.Errorf("rows not in section order")
	}
	assertNoRotValues(t, rows)
}

func TestRotateMemo(t *testing.T) {
	f := newRotFx(t)
	w := []string{"e1"}
	f.v.Rotate(f.cs, rotOpts, w, view.RotateRuns{})
	f.v.Rotate(f.cs, rotOpts, w, view.RotateRuns{})
	if n := f.v.RotateBuilds(); n != 1 {
		t.Fatalf("builds = %d, want 1", n)
	}
	f.set(t, "api", "JWT_SECRET", rotNew)
	f.v.Rotate(f.cs, rotOpts, w, view.RotateRuns{})
	if n := f.v.RotateBuilds(); n != 2 {
		t.Errorf("after a change: builds = %d, want 2", n)
	}
	// a store refresh: same change set and options
	recs := rotVars()
	recs[0].UpdatedAt = "2026-10-21T08:00:00"
	f.st.ReplaceVariables("p1", recs)
	rows := f.v.Rotate(f.cs, rotOpts, w, view.RotateRuns{})
	if n := f.v.RotateBuilds(); n != 3 {
		t.Errorf("after a refresh: builds = %d, want 3", n)
	}
	if p := rotPlace(t, rotRow(t, rows, "JWT_SECRET"), "e1/api/JWT_SECRET"); p.Loc.Updated.IsZero() {
		t.Errorf("refreshed data not in the row: %+v", p.Loc)
	}
	f.v.Rotate(f.cs, rotation.Options{EnvIDs: []string{"e1"}}, w, view.RotateRuns{})
	if n := f.v.RotateBuilds(); n != 4 {
		t.Errorf("after new options: builds = %d, want 4", n)
	}
	f.v.Rotate(f.cs, rotation.Options{EnvIDs: []string{"e1"}}, []string{"e1", "e2"}, view.RotateRuns{})
	if n := f.v.RotateBuilds(); n != 5 {
		t.Errorf("after new write envs: builds = %d, want 5", n)
	}
}

func TestRotateRefs(t *testing.T) {
	f := newRotFx(t)
	rows := f.v.Rotate(f.cs, rotOpts, []string{"e1"}, view.RotateRuns{})
	r := rotRow(t, rows, "JWT_SECRET")
	want := []view.RefChain{
		{Loc: "e1/web/CHAIN", Label: "CHAIN ← worker.USES ← api.JWT_SECRET"},
		{Loc: "e1/mongo2/JWT", Label: "JWT ← api.JWT_SECRET", DB: true},
		{Loc: "e1/worker/USES", Label: "USES ← api.JWT_SECRET"},
	}
	if !slices.Equal(r.Refs, want) {
		t.Errorf("refs = %+v\nwant %+v", r.Refs, want)
	}
	if app := rotRow(t, rows, "APP_SECRET"); len(app.Refs) != 0 {
		t.Errorf("APP refs = %+v", app.Refs)
	}
}

// rotFP is the store's fingerprint of a name at a place.
func (f *fx) rotFP(t *testing.T, p domain.Place, name string) string {
	t.Helper()
	for _, v := range f.st.VariablesAt(p) {
		if v.Name == name {
			return v.Fingerprint
		}
	}
	t.Fatalf("no %s at %s", name, p.Key())
	return ""
}

func TestChangeSetCoverage(t *testing.T) {
	f := newRotFx(t)
	at := func(env, svc string) domain.Place { return domain.Place{ProjectID: "p1", EnvID: env, ServiceID: svc} }
	jwt := rotation.GroupKey(f.rotFP(t, at("e1", "api"), "JWT_SECRET"))
	app := rotation.GroupKey(f.rotFP(t, at("e1", "api"), "APP_SECRET"))
	root := rotation.GroupKey(f.rotFP(t, at("e1", "mongo"), "MONGO_INITDB_ROOT_PASSWORD"))

	f.rotateSet(t, at("e1", "api"), "JWT_SECRET", rotNew, jwt)                    // 1 of 3
	f.rotateSet(t, at("e1", "api"), "APP_SECRET", rotNew, app)                    // 1 of 2
	f.rotateSet(t, at("e1", "mongo"), "MONGO_INITDB_ROOT_PASSWORD", rotNew, root) // 1 of 1: full
	f.rotateSet(t, at("e1", "web"), "ADDED", rotAddedV1, rotation.AddGroupKey("ADDED"))
	other, err := kinds.SetVariable(f.st, f.cs.Changes, at("e1", "worker"), "OTHER", rotNew, now)
	if err != nil {
		t.Fatal(err)
	}
	other.Group = "import:x"
	if err := f.cs.Put(other, nil); err != nil {
		t.Fatal(err)
	}

	got := f.v.ChangeSet(f.cs, rotOpts.EnvIDs).Coverage
	want := []rotation.Coverage{{Group: app, Label: "APP_SECRET", Changed: 1, Total: 2}, {Group: jwt, Label: "JWT_SECRET", Changed: 1, Total: 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("coverage = %+v, want %+v", got, want)
	}

	// a put (change set revision) moves it
	f.rotateSet(t, at("e1", "mongo2"), "APP_SECRET", rotNew, app)
	want = []rotation.Coverage{{Group: jwt, Label: "JWT_SECRET", Changed: 1, Total: 3}}
	if got := f.v.ChangeSet(f.cs, rotOpts.EnvIDs).Coverage; !slices.Equal(got, want) {
		t.Errorf("after put = %+v, want %+v", got, want)
	}

	// a refresh finding a place of the old value (store revision) moves it
	f.st.ReplaceVariables("p1", append(rotVars(), railway.VarRecord{EnvID: "e1", ServiceID: "worker", Name: "JWT_SECRET", Unrendered: rotJWT}))
	want = []rotation.Coverage{{Group: jwt, Label: "JWT_SECRET", Changed: 1, Total: 4}}
	if got := f.v.ChangeSet(f.cs, rotOpts.EnvIDs).Coverage; !slices.Equal(got, want) {
		t.Errorf("after refresh = %+v, want %+v", got, want)
	}
	if got := fmt.Sprintf("%+v", f.v.ChangeSet(f.cs, rotOpts.EnvIDs).Coverage); strings.Contains(got, rotJWT) || strings.Contains(got, rotApp) {
		t.Error("a value reached the view")
	}
}

// AllEnvIDs only widens detection: the rows show the selected environments
// (password rows aside: they show every place), the memo key includes it.
func TestRotateAllEnvIDsKeepsRowsOfSelected(t *testing.T) {
	f := newRotFx(t)
	sel := rotation.Options{EnvIDs: []string{"e1"}}
	wide := rotation.Options{EnvIDs: []string{"e1"}, AllEnvIDs: []string{"e1", "e2"}}
	a := f.v.Rotate(f.cs, sel, nil, view.RotateRuns{})
	b := f.v.Rotate(f.cs, wide, nil, view.RotateRuns{})
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("rows = %d, with AllEnvIDs %d", len(a), len(b))
	}
	for _, r := range b {
		if r.Password != nil {
			continue
		}
		for _, p := range r.Places {
			if p.Loc.Place.EnvID != "e1" {
				t.Errorf("row %s shows a place of %s", r.Key, p.Loc.Place.EnvID)
			}
		}
	}
}

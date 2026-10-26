package rotation

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

// fixtureCatalog: one project with a prod and a dev environment.
func fixtureCatalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{
			{ID: "e1", Name: "prod", CanAccess: true, ServiceIDs: []string{"sapi", "sweb", "smongo", "smongo2", "spg"}},
			{ID: "e2", Name: "dev", CanAccess: true, ServiceIDs: []string{"sapi"}},
		},
		Services: []railway.Service{
			{ID: "sapi", Name: "api"}, {ID: "sweb", Name: "web"},
			{ID: "smongo", Name: "mongo"}, {ID: "smongo2", Name: "mongo-2"}, {ID: "spg", Name: "pg"},
		},
	}}}
}

// rec builds a variable record in prod; Rendered mirrors a literal value.
func rec(service, name, value string) railway.VarRecord {
	return railway.VarRecord{EnvID: "e1", ServiceID: service, Name: name, Unrendered: value, Rendered: value}
}

// newStore holds recs in p1. Records named like a Railway system value
// (railway.SystemVarNames) become system values, as discovery delivers them.
func newStore(recs ...railway.VarRecord) *store.Store {
	s := store.New()
	s.ReplaceCatalog(fixtureCatalog())
	vars, sys := splitSystem(recs)
	s.ReplaceVariables("p1", vars, sys...)
	return s
}

func splitSystem(recs []railway.VarRecord) ([]railway.VarRecord, []railway.SystemRecord) {
	var vars []railway.VarRecord
	var sys []railway.SystemRecord
	for _, r := range recs {
		if slices.Contains(railway.SystemVarNames, r.Name) {
			sys = append(sys, railway.SystemRecord{EnvID: r.EnvID, ServiceID: r.ServiceID, Name: r.Name, Value: r.Rendered})
			continue
		}
		vars = append(vars, r)
	}
	return vars, sys
}

func prodOnly() Options { return Options{EnvIDs: []string{"e1"}} }

// groupsWith returns every group that holds a name.
func groupsWith(a Analysis, name string) []Group {
	var out []Group
	for _, g := range a.Groups {
		for _, n := range g.Names {
			if n == name {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

func oneGroup(t *testing.T, a Analysis, name string) Group {
	t.Helper()
	gs := groupsWith(a, name)
	if len(gs) != 1 {
		t.Fatalf("groups with %s = %d, want 1", name, len(gs))
	}
	return gs[0]
}

func TestLabel(t *testing.T) {
	if got := Label([]string{"B", "A", "B"}); got != "A = B" {
		t.Errorf("Label = %q, want %q", got, "A = B")
	}
}

func TestGroupsAcrossNames(t *testing.T) {
	key := strings.Repeat("k", 40)
	s := newStore(
		rec("sapi", "DO_MODEL_KEY", key),
		rec("sweb", "RSS_DO_MODEL_KEY", key),
		rec("sweb", "POSTGRES_USER", "postgres"),
		rec("sweb", "POSTGRES_PASSWORD", "postgres"),
	)
	a := Analyze(s, prodOnly())
	g := oneGroup(t, a, "DO_MODEL_KEY")
	if !reflect.DeepEqual(g.Names, []string{"DO_MODEL_KEY", "RSS_DO_MODEL_KEY"}) {
		t.Errorf("Names = %v", g.Names)
	}
	if len(g.Locs) != 2 {
		t.Errorf("Locs = %d, want 2", len(g.Locs))
	}
	u := oneGroup(t, a, "POSTGRES_USER")
	p := oneGroup(t, a, "POSTGRES_PASSWORD")
	if u.Key == p.Key || len(u.Names) != 1 || len(p.Names) != 1 {
		t.Errorf("short equal values must not group across names: %v %v", u.Names, p.Names)
	}
}

func TestSkips(t *testing.T) {
	sealed := func(name string) railway.VarRecord {
		return railway.VarRecord{EnvID: "e1", ServiceID: "sweb", Name: name, Sealed: true}
	}
	s := newStore(
		rec("sapi", "X", "plain-value-of-some-length"),
		rec("sapi", "RAILWAY_PUBLIC_DOMAIN", "api.example.com"),
		rec("sweb", "REF", "${{api.X}}"),
		sealed("FOO"),
		sealed("FOO_SECRET"),
	)
	a := Analyze(s, prodOnly())
	for _, n := range []string{"RAILWAY_PUBLIC_DOMAIN", "REF", "FOO"} {
		if gs := groupsWith(a, n); len(gs) != 0 {
			t.Errorf("%s made %d groups, want 0", n, len(gs))
		}
	}
	g := oneGroup(t, a, "FOO_SECRET")
	if !g.Sealed || len(g.Locs) != 1 || !g.Locs[0].Sealed {
		t.Errorf("sealed FOO_SECRET: Sealed=%v locs=%d", g.Sealed, len(g.Locs))
	}
	if g.OldLen != 0 || g.Hex {
		t.Errorf("sealed counts as empty: OldLen=%d Hex=%v", g.OldLen, g.Hex)
	}
}

func TestOnlySelectedEnvs(t *testing.T) {
	v := strings.Repeat("z", 24)
	dev := rec("sapi", "SOME_SECRET", v)
	dev.EnvID = "e2"
	s := newStore(rec("sapi", "SOME_SECRET", v), dev)
	g := oneGroup(t, Analyze(s, prodOnly()), "SOME_SECRET")
	if len(g.Locs) != 1 || g.Locs[0].Place.EnvID != "e1" {
		t.Errorf("Locs = %+v, want only the prod one", g.Locs)
	}
	g = oneGroup(t, Analyze(s, Options{EnvIDs: []string{"e1", "e2"}}), "SOME_SECRET")
	if len(g.Locs) != 2 {
		t.Errorf("both envs selected: Locs = %d, want 2", len(g.Locs))
	}
}

func TestLeakTime(t *testing.T) {
	at := func(r railway.VarRecord, updated string) railway.VarRecord {
		r.UpdatedAt = updated
		return r
	}
	x := strings.Repeat("x", 30)
	y := strings.Repeat("y", 30)
	s := newStore(
		at(rec("sapi", "X_SECRET", x), "2026-10-01T00:00:00"),
		at(rec("sweb", "X_SECRET", x), "2026-10-20T00:00:00"),
		at(rec("sapi", "Y_SECRET", y), "2026-10-21T00:00:00"),
		at(rec("sweb", "Y_SECRET", y), "2026-10-22T00:00:00"),
		at(rec("sapi", "Z_SECRET", "zzz"), "not a time"),
	)
	o := prodOnly()
	o.Since = time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	a := Analyze(s, o)

	gx := oneGroup(t, a, "X_SECRET")
	if gx.AllFresh {
		t.Error("X_SECRET: AllFresh, want false")
	}
	fresh := map[string]bool{}
	for _, l := range gx.Locs {
		fresh[l.Place.ServiceID] = l.Fresh
	}
	if fresh["sapi"] || !fresh["sweb"] {
		t.Errorf("X_SECRET fresh = %v, want only sweb", fresh)
	}
	if !gx.Locs[0].Updated.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Updated = %v", gx.Locs[0].Updated)
	}

	gy := oneGroup(t, a, "Y_SECRET")
	if !gy.AllFresh || !strings.Contains(gy.Note, "already changed after the leak everywhere") {
		t.Errorf("Y_SECRET: AllFresh=%v note=%q", gy.AllFresh, gy.Note)
	}

	gz := oneGroup(t, a, "Z_SECRET")
	if !gz.Locs[0].Updated.IsZero() || gz.Locs[0].Fresh || gz.AllFresh {
		t.Errorf("unparsable time: %+v", gz.Locs[0])
	}

	none := Analyze(s, prodOnly())
	if g := oneGroup(t, none, "Y_SECRET"); g.AllFresh || g.Locs[0].Fresh {
		t.Error("without Since nothing is fresh")
	}
}

func TestSealedNote(t *testing.T) {
	s := newStore(railway.VarRecord{EnvID: "e1", ServiceID: "sweb", Name: "FOO_SECRET", Sealed: true})
	g := oneGroup(t, Analyze(s, prodOnly()), "FOO_SECRET")
	if !strings.HasPrefix(g.Note, "sealed: value unreadable, may be empty") {
		t.Errorf("Note = %q", g.Note)
	}
}

func TestGenerationShape(t *testing.T) {
	hex := strings.Repeat("0123456789abcdef", 4)
	s := newStore(
		rec("sapi", "HEX_SECRET", hex),
		rec("sapi", "MIXED_SECRET", "Abc123"),
		rec("sapi", "UPPER_SECRET", strings.ToUpper(hex)),
	)
	a := Analyze(s, prodOnly())
	if g := oneGroup(t, a, "HEX_SECRET"); g.OldLen != 64 || !g.Hex {
		t.Errorf("HEX_SECRET: OldLen=%d Hex=%v", g.OldLen, g.Hex)
	}
	if g := oneGroup(t, a, "MIXED_SECRET"); g.OldLen != 6 || g.Hex {
		t.Errorf("MIXED_SECRET: OldLen=%d Hex=%v", g.OldLen, g.Hex)
	}
	if g := oneGroup(t, a, "UPPER_SECRET"); g.Hex {
		t.Error("upper-case hex is not lowercase hex")
	}
}

func TestGroupOrder(t *testing.T) {
	s := newStore(
		rec("sapi", "PORT", "8080"),
		rec("sapi", "STRIPE_API_KEY", "sk_live_aaaaaaaaaaaaaaaaaaaa"),
		rec("sapi", "B_SECRET", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		rec("sapi", "Z_PASSWORD", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	)
	a := Analyze(s, prodOnly())
	var got []string
	for _, g := range a.Groups {
		got = append(got, Label(g.Names))
	}
	want := []string{"Z_PASSWORD", "B_SECRET", "STRIPE_API_KEY", "PORT"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// unselectedDBStore: e1 (selected) holds a URL to the TCP proxy of the
// postgres in e2 (not selected), plus a JWT_SECRET shared with e2's api.
func unselectedDBStore() *store.Store {
	s := store.New()
	c := fixtureCatalog()
	c.Projects[0].Environments[1].ServiceIDs = []string{"sapi", "spg"}
	s.ReplaceCatalog(c)
	r2 := func(svc, name, v string) railway.VarRecord {
		return railway.VarRecord{EnvID: "e2", ServiceID: svc, Name: name, Unrendered: v, Rendered: v}
	}
	vars, sys := splitSystem([]railway.VarRecord{
		r2("spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
		r2("spg", "POSTGRES_USER", "postgres"),
		r2("spg", "RAILWAY_TCP_PROXY_DOMAIN", "prod.proxy.rlwy.net"),
		r2("spg", "RAILWAY_TCP_PROXY_PORT", "23456"),
		r2("sapi", "JWT_SECRET", "jwt-secret-value-0123456789"),
		rec("sapi", "JWT_SECRET", "jwt-secret-value-0123456789"),
		rec("sapi", "DATABASE_URL", "postgres://postgres:pg-root-password-0123456789@prod.proxy.rlwy.net:23456/x"),
	})
	s.ReplaceVariables("p1", vars, sys...)
	return s
}

func TestAllEnvIDsDetectsDatabaseInUnselectedEnv(t *testing.T) {
	s := unselectedDBStore()

	// Selected environments only: the URL is not recognised.
	a := Analyze(s, prodOnly())
	if g := oneGroup(t, a, "DATABASE_URL"); g.DB != nil || len(a.DBs) != 0 {
		t.Errorf("without AllEnvIDs: DB = %v, DBs = %d", g.DB, len(a.DBs))
	}

	o := prodOnly()
	o.AllEnvIDs = []string{"e1", "e2"}
	a = Analyze(s, o)
	g := oneGroup(t, a, "DATABASE_URL")
	if g.Class != ClassURL || g.DB == nil || g.DB.Owner.EnvID != "e2" || g.DB.Owner.ServiceID != "spg" {
		t.Fatalf("DATABASE_URL: Class=%v DB=%+v", g.Class, g.DB)
	}
	// The root password lives in e2 only: no group, e2 is not read for groups.
	if gs := groupsWith(a, "POSTGRES_PASSWORD"); len(gs) != 0 {
		t.Errorf("groups of an unselected env: %+v", gs)
	}
	// Groups hold selected locations only.
	for _, g := range a.Groups {
		for _, l := range g.Locs {
			if l.Place.EnvID != "e1" {
				t.Errorf("group %v has a location in %s", g.Names, l.Place.EnvID)
			}
		}
	}
	if g := oneGroup(t, a, "JWT_SECRET"); len(g.Locs) != 1 {
		t.Errorf("JWT_SECRET locs = %d, want 1", len(g.Locs))
	}

	// Empty AllEnvIDs falls back to EnvIDs.
	o.AllEnvIDs = nil
	if got := Analyze(s, o); len(got.DBs) != 0 {
		t.Errorf("fallback: DBs = %d", len(got.DBs))
	}
}

func TestCoverageOfCountsSelectedEnvsOnly(t *testing.T) {
	s := unselectedDBStore()
	v := s.Variable("e1/sapi/JWT_SECRET")
	group := GroupKey(v.Fingerprint)
	if cov, ok := CoverageOf(s, nil, group, []string{"e1"}); !ok || cov.Total != 1 {
		t.Errorf("selected: %+v %v", cov, ok)
	}
	if cov, _ := CoverageOf(s, nil, group, []string{"e1", "e2"}); cov.Total != 2 {
		t.Errorf("both: %+v", cov)
	}
}

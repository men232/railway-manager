package rotation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
)

func TestRowsMerge(t *testing.T) {
	s := newStore(
		rec("sapi", "API_KEY", "old-key-one"), rec("sweb", "API_KEY", "old-key-two"),
		rec("sapi", "JWT_SECRET", "jwt-value-one"), rec("sweb", "JWT_SECRET", "jwt-value-one"),
		rec("sapi", "APP_SECRET", "jwt-value-one"),
		rec("sweb", "STRIPE_KEY", "stripe-1"), rec("sapi", "STRIPE_KEY", "stripe-2"),
	)
	a := Analyze(s, prodOnly())
	rows := Rows(a)
	var api *Row
	for i := range rows {
		if rows[i].Label == "API_KEY" {
			api = &rows[i]
		}
	}
	if api == nil || len(api.Groups) != 2 {
		t.Fatalf("API_KEY row = %+v", api)
	}
	if api.Key != api.Groups[0].Key {
		t.Errorf("Key = %q, want first group's", api.Key)
	}
	if Letter(0) != "a" || Letter(1) != "b" {
		t.Errorf("letters")
	}
	for _, r := range rows {
		if r.Label == "APP_SECRET = JWT_SECRET" && r.Class != ClassGen {
			t.Errorf("class = %v", r.Class)
		}
	}
}

func TestRowsMergeKeepsClassesApart(t *testing.T) {
	// SHARED is GEN through a second name in one group (SECRET) and ASK in
	// the other (API_KEY only): same name, different class, two rows.
	s := newStore(
		rec("sapi", "JWT_SECRET", "v1-aaaaaaaa"), rec("sweb", "JWT_SECRET", "v2-bbbbbbbb"),
		rec("sapi", "API_KEY", "v1-aaaaaaaa"),
	)
	rows := Rows(Analyze(s, prodOnly()))
	var gen, ask int
	for _, r := range rows {
		switch r.Class {
		case ClassGen:
			gen++
		case ClassAsk:
			ask++
		}
	}
	if gen != 1 || ask != 1 {
		t.Fatalf("gen=%d ask=%d rows=%d", gen, ask, len(rows))
	}
}

func TestRowsSections(t *testing.T) {
	s := newStore(
		rec("sapi", "JWT_SECRET", "jwt-value-one"),
		rec("sapi", "STRIPE_KEY", "stripe-1"),
		rec("sapi", "VITE_API_KEY", "public-1"),
		rec("sapi", "PLAIN", "x"),
		rec("smongo", "MONGO_INITDB_ROOT_PASSWORD", "mongo-pass"),
	)
	rows := Rows(Analyze(s, prodOnly()))
	by := map[string]Row{}
	for _, r := range rows {
		by[r.Label] = r
	}
	if by["JWT_SECRET"].Section != SectionGenerated {
		t.Errorf("GEN section")
	}
	if r := by["STRIPE_KEY"]; r.Section != SectionPaste || r.Hidden {
		t.Errorf("ASK row = %+v", r)
	}
	if r := by["VITE_API_KEY"]; r.Section != SectionPaste || !r.Hidden || !r.Public {
		t.Errorf("public row = %+v", r)
	}
	if r := by["PLAIN"]; r.Section != SectionPaste || !r.Hidden || r.Public {
		t.Errorf("hidden row = %+v", r)
	}
	if r := by["MONGO_INITDB_ROOT_PASSWORD"]; r.Section != SectionDatabase {
		t.Errorf("DB row = %+v", r)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Section < rows[i-1].Section {
			t.Fatalf("rows not sorted by section")
		}
	}
}

func TestRowsURLOwnRow(t *testing.T) {
	s := newStore(
		rec("spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
		rec("spg", "POSTGRES_USER", "postgres"),
		rec("spg", "RAILWAY_PRIVATE_DOMAIN", "pg.railway.internal"),
		rec("sapi", "USER_URL", "postgres://u:p@pg.railway.internal:5432/x"),
		rec("sweb", "USER_URL", "postgres://u:q@pg.railway.internal:5432/x"),
	)
	urls := 0
	for _, r := range Rows(Analyze(s, prodOnly())) {
		if r.Class == ClassURL {
			urls++
			if len(r.Groups) != 1 || r.Section != SectionDatabase {
				t.Errorf("URL row = %+v", r)
			}
		}
	}
	if urls != 2 {
		t.Fatalf("URL rows = %d, want 2", urls)
	}
}

func TestGenerate(t *testing.T) {
	for _, tc := range []struct{ old, want int }{{10, 24}, {24, 24}, {64, 64}} {
		for _, hex := range []bool{true, false} {
			v, err := Generate(tc.old, hex)
			if err != nil || len(v) != tc.want {
				t.Fatalf("Generate(%d,%v) len=%d err=%v", tc.old, hex, len(v), err)
			}
			if hex != isLowerHex(v) && hex {
				t.Errorf("not hex")
			}
			for _, c := range v {
				ok := c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
				if !hex {
					ok = ok || c >= 'g' && c <= 'z' || c >= 'A' && c <= 'Z'
				}
				if !ok {
					t.Fatalf("bad char %q (hex=%v)", c, hex)
				}
			}
		}
	}
	a, _ := Generate(64, false)
	b, _ := Generate(64, false)
	if a == b {
		t.Error("two calls equal")
	}
	if v, _ := Generate(64, false); !strings.ContainsAny(v, "ghijklmnopqrstuvwxyzGHIJKLMNOPQRSTUVWXYZ") {
		t.Error("alphanumeric value has no non-hex letter")
	}
}

func TestGroupKeys(t *testing.T) {
	if fp, ok := ParseGroupKey(GroupKey("abc123")); !ok || fp != "abc123" {
		t.Errorf("round trip = %q %v", fp, ok)
	}
	if AddGroupKey("X") != "rotate:add:X" {
		t.Errorf("add key")
	}
	if _, ok := ParseGroupKey("rotate:add:X"); ok {
		t.Error("add group parsed")
	}
	if _, ok := ParseGroupKey("limits:x"); ok {
		t.Error("foreign group parsed")
	}
}

func TestCoverage(t *testing.T) {
	var recs []railway.VarRecord
	svcs := []string{"sapi", "sweb", "smongo", "smongo2", "spg"}
	for _, sv := range svcs {
		recs = append(recs, rec(sv, "JWT_SECRET", "jwt-shared-value"))
	}
	recs = append(recs,
		railway.VarRecord{EnvID: "e2", ServiceID: "sapi", Name: "JWT_SECRET", Unrendered: "jwt-shared-value", Rendered: "jwt-shared-value"},
		railway.VarRecord{EnvID: "e1", Name: "JWT_SECRET", Unrendered: "jwt-shared-value", Rendered: "jwt-shared-value"},
		rec("sapi", "OTHER_SECRET", "different-value"),
	)
	s := newStore(recs...)
	v := s.Variable(domain.LocationID("e1", "sapi", "JWT_SECRET"))
	group := GroupKey(v.Fingerprint)
	mk := func(env, svc, group string) changeset.Change {
		return changeset.Change{
			ID: changeset.NewID(), Kind: "variable.set", Target: "JWT_SECRET", New: json.RawMessage(`"x"`),
			Place: domain.Place{ProjectID: "p1", EnvID: env, ServiceID: svc}, Group: group,
		}
	}
	changes := []changeset.Change{
		mk("e1", "sapi", group), mk("e1", "sweb", group), mk("e1", "smongo", group), mk("e2", "sapi", group),
		mk("e1", "spg", GroupKey("other")),
	}
	cov, ok := CoverageOf(s, changes, group, []string{"e1", "e2"})
	if !ok || cov != (Coverage{Group: group, Label: "JWT_SECRET", Changed: 4, Total: 7}) {
		t.Fatalf("coverage = %+v %v", cov, ok)
	}
	if _, ok := CoverageOf(s, changes, "limits:x", nil); ok {
		t.Error("non-rotate group covered")
	}
}

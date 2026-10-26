package kinds

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/store"
)

func mustCopy(t *testing.T, s *store.Store, p domain.Place, name string, src domain.Place, srcName string) changeset.Change {
	t.Helper()
	c, err := CopyVariable(s, nil, p, name, src, srcName, now)
	if err != nil {
		t.Fatalf("CopyVariable: %v", err)
	}
	return c
}

func TestCopyVariable(t *testing.T) {
	s := fixtureStore(t)
	s.ReplaceVariables("p1", append(fixtureVars(), railway.VarRecord{EnvID: "e1", ServiceID: "web", Name: "EMPTY"}))
	c := mustCopy(t, s, place("api"), "PLAIN", place("db"), "URL")
	if c.Kind != "variable.set" || c.Target != "PLAIN" || c.Place != place("api") || c.ID == "" || !c.Created.Equal(now) {
		t.Errorf("change = %+v", c)
	}
	if c.Base.Fingerprint != s.Variable("e1/api/PLAIN").Fingerprint {
		t.Errorf("base = %+v", c.Base)
	}
	var raw struct {
		Value string `json:"value"`
		Copy  struct {
			Loc         string `json:"loc"`
			Fingerprint string `json:"fingerprint"`
		} `json:"copy"`
	}
	if err := json.Unmarshal(c.New, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Value != "pg-one" {
		t.Error("value is not the source's unrendered value")
	}
	if raw.Copy.Loc != "e1/db/URL" || raw.Copy.Fingerprint != s.Variable("e1/db/URL").Fingerprint {
		t.Errorf("copy = %+v", raw.Copy)
	}
	if loc, ok := CopySource(c); !ok || loc != "e1/db/URL" {
		t.Errorf("CopySource = %q, %v", loc, ok)
	}
	if _, ok := CopySource(mustSet(t, s, place("api"), "PLAIN", "two")); ok {
		t.Error("a plain set has no copy source")
	}
	if got := NewRegistry().CheckData(c); got != nil {
		t.Errorf("CheckData = %v", got)
	}
	if o := NewRegistry()["variable.set"].Overlay(c); o.Short != "→ 6 chars" {
		t.Errorf("overlay = %+v", o)
	}

	for _, tc := range []struct{ svc, name, want string }{
		{"db", "KEY", "copy source is sealed"},
		{"web", "EMPTY", "copy source is empty"},
		{"db", "NOPE", "copy source not found"},
	} {
		_, err := CopyVariable(s, nil, place("api"), "PLAIN", place(tc.svc), tc.name, now)
		if err == nil || err.Error() != tc.want {
			t.Errorf("copy from %s.%s: err = %v, want %q", tc.svc, tc.name, err, tc.want)
		}
	}
}

func TestCopyStoreDrift(t *testing.T) {
	reg := NewRegistry()
	c := mustCopy(t, fixtureStore(t), place("api"), "PLAIN", place("db"), "URL")
	for _, tc := range []struct {
		name string
		now  []railway.VarRecord
		want string
	}{
		{"unchanged", fixtureVars(), ""},
		{"source changed", edited(func(r *railway.VarRecord) bool {
			if r.ServiceID == "db" && r.Name == "URL" {
				r.Unrendered = "pg-two"
			}
			return true
		}), DriftCopySource},
		{"source gone", edited(func(r *railway.VarRecord) bool { return r.ServiceID != "db" || r.Name != "URL" }), DriftCopySource},
		{"source place not read", edited(func(r *railway.VarRecord) bool { return r.ServiceID != "db" }), ""},
		{"target already holds the copy", edited(func(r *railway.VarRecord) bool {
			switch {
			case r.ServiceID == "db" && r.Name == "URL":
				r.Unrendered = "pg-two"
			case r.ServiceID == "api" && r.Name == "PLAIN":
				r.Unrendered = "pg-one"
			}
			return true
		}), ""},
		{"target drifted first", edited(func(r *railway.VarRecord) bool {
			if r.Name == "PLAIN" || r.Name == "URL" {
				r.Unrendered = "other"
			}
			return true
		}), DriftValueChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixtureStore(t)
			s.ReplaceVariables("p1", tc.now)
			if got := reg.StoreDrift(s, c); got != tc.want {
				t.Errorf("StoreDrift = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCopyCheck(t *testing.T) {
	s := fixtureStore(t)
	set := NewRegistry()["variable.set"]
	c := mustCopy(t, s, place("api"), "PLAIN", place("db"), "URL")
	shared := mustCopy(t, s, place("web"), "COPIED", place(""), "S")
	for _, tc := range []struct {
		name  string
		fake  func(f *railwaytest.Fake)
		c     changeset.Change
		drift apply.Drift
	}{
		{"unchanged", func(*railwaytest.Fake) {}, c, apply.DriftNone},
		{"source changed", func(f *railwaytest.Fake) { f.SetVar("e1", "db", "URL", "pg-two", false) }, c, apply.DriftChanged},
		{"source emptied", func(f *railwaytest.Fake) { f.SetVar("e1", "db", "URL", "", false) }, c, apply.DriftChanged},
		{"target already new", func(f *railwaytest.Fake) {
			f.SetVar("e1", "db", "URL", "pg-two", false)
			f.SetVar("e1", "api", "PLAIN", "pg-one", false)
		}, c, apply.DriftAlreadyNew},
		{"shared source unchanged", func(*railwaytest.Fake) {}, shared, apply.DriftNone},
		{"sealed target, source changed", func(f *railwaytest.Fake) { f.SetVar("e1", "db", "URL", "pg-two", false) },
			mustCopy(t, s, place("db"), "KEY", place("db"), "URL"), apply.DriftChanged},
		{"shared source changed", func(f *railwaytest.Fake) { f.SetVar("e1", "", "S", "shared-two", false) }, shared, apply.DriftChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			tc.fake(f)
			env, _ := newEnv(t, f, s)
			got, err := set.Check(context.Background(), env, tc.c)
			if err != nil || got != tc.drift {
				t.Errorf("Check = %v, %v; want %v", got, err, tc.drift)
			}
		})
	}
}

func TestPatchSkipReasonCopy(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.SetVar("e1", "db", "URL", "pg-two", false)
	c := mustCopy(t, s, place("api"), "PLAIN", place("db"), "URL")
	res, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{Status: "pending"}, c)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Outcomes[c.ID]
	if out.Status != "skipped" || !strings.Contains(out.Reason, "copy source") || !strings.Contains(out.Reason, "changed since the change was made") {
		t.Errorf("outcome = %+v", out)
	}
	if out.Reason != "copy source dev · db changed since the change was made" {
		t.Errorf("reason = %q", out.Reason)
	}
	if strings.Contains(out.Reason, "pg-") {
		t.Error("the reason holds a value")
	}
	if len(commits(f)) != 0 {
		t.Error("nothing should be written")
	}

	sr := NewRegistry()["variable.set"].(apply.SkipReasoner)
	env, _ := newEnv(t, f, s)
	ctx := context.Background()
	if got := sr.SkipReason(ctx, env, mustSet(t, s, place("api"), "PLAIN", "two")); got != "" {
		t.Errorf("SkipReason of a plain set = %q", got)
	}
	// Without a store the reason names no place.
	env.Store = nil
	if got := sr.SkipReason(ctx, env, c); got != "copy source changed since the change was made" {
		t.Errorf("SkipReason without a store = %q", got)
	}

	// The target itself changed: the default reason, not the copy source.
	f2 := newFake(t)
	f2.SetVar("e1", "api", "PLAIN", "three", false)
	res, _, _, err = runPatch(t, f2, s, NewRegistry(), apply.StepRecord{Status: "pending"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Outcomes[c.ID].Reason; got != "changed in Railway since the change was made" {
		t.Errorf("target drifted: reason = %q", got)
	}
}

func TestDecodeWithoutCopy(t *testing.T) {
	reg := NewRegistry()
	c := mustSet(t, fixtureStore(t), place("api"), "PLAIN", "two")
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{`{"value":"two"}`, true},
		{`{"value":"two","copy":{"loc":"e1/db/URL","fingerprint":"abc"}}`, true},
		{`{"value":"two","copy":{"loc":"e1/db/URL","fingerprint":"abc","x":1}}`, false},
		{`{"value":"two","copy":{"fingerprint":"abc"}}`, false},
		{`{"value":"two","copy":{"loc":"e1/db/URL"}}`, false},
	} {
		c.New = json.RawMessage(tc.raw)
		if err := reg.CheckData(c); (err == nil) != tc.ok {
			t.Errorf("CheckData(%s) = %v, want ok %v", tc.raw, err, tc.ok)
		}
	}
}

func TestSetValue(t *testing.T) {
	s := fixtureStore(t)
	if v, ok := SetValue(mustSet(t, s, place("api"), "PLAIN", "two")); !ok || v != "two" {
		t.Error("SetValue of a set is not its value")
	}
	if v, ok := SetValue(mustCopy(t, s, place("api"), "PLAIN", place("db"), "URL")); !ok || v != "pg-one" {
		t.Error("SetValue of a copy is not the copied value")
	}
	del, err := DeleteVariable(s, place("api"), "PLAIN", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := SetValue(del); ok {
		t.Error("a delete has no value")
	}
}

// A copy of a planned value holds that value and its link, and is checked
// against its own target only: Railway's value at the source is the old one.
func TestCopyPlanned(t *testing.T) {
	s := fixtureStore(t)
	src := mustSet(t, s, place("api"), "PLAIN", "fixture-planned-value")
	src.ID = "src"
	c, err := CopyPlanned(s, []changeset.Change{src}, place("web"), "PLAIN", src, now)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := SetValue(c); v != "fixture-planned-value" {
		t.Error("the copy does not hold the planned value")
	}
	if loc, ok := CopySource(c); !ok || loc != "e1/api/PLAIN" {
		t.Errorf("CopySource = %q, %v", loc, ok)
	}
	if got := NewRegistry().CheckData(c); got != nil {
		t.Errorf("CheckData = %v", got)
	}
	if _, err := validNew(c); err != nil {
		t.Errorf("validNew: %v", err)
	}
	if (variableKind{}).StoreDrift(s, c) != "" {
		t.Error("a planned copy drifts on its source's Railway value")
	}

	del := changeset.Change{ID: "del", Kind: kindDelete, Place: place("api"), Target: "PLAIN"}
	if _, err := CopyPlanned(s, nil, place("web"), "PLAIN", del, now); err == nil {
		t.Error("a delete was copied")
	}
	ref := mustSet(t, s, place("api"), "PLAIN", "${{db.URL}}")
	if _, err := CopyPlanned(s, nil, place("web"), "PLAIN", ref, now); err == nil || !strings.Contains(err.Error(), "reference") {
		t.Errorf("a reference was copied: %v", err)
	}
}

// CopiesOf follows copies of copies; Follow keeps the link.
func TestCopiesOfAndFollow(t *testing.T) {
	s := fixtureStore(t)
	a := mustSet(t, s, place("api"), "PLAIN", "fixture-a-value-0001")
	a.ID = "a"
	b, err := CopyPlanned(s, nil, place("web"), "PLAIN", a, now)
	if err != nil {
		t.Fatal(err)
	}
	b.ID = "b"
	c, err := CopyPlanned(s, nil, place("db"), "PLAIN", b, now)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = "c"
	other := mustSet(t, s, place("db"), "OTHER", "x")
	other.ID = "o"
	all := []changeset.Change{c, other, a, b}
	ids := func(cs []changeset.Change) []string {
		var out []string
		for _, x := range cs {
			out = append(out, x.ID)
		}
		return out
	}
	if got := ids(CopiesOf(all, "a")); strings.Join(got, ",") != "b,c" && strings.Join(got, ",") != "c,b" {
		t.Errorf("CopiesOf(a) = %v", got)
	}
	if got := CopiesOf(all, "o"); len(got) != 0 {
		t.Errorf("CopiesOf(o) = %v", ids(got))
	}
	f, err := Follow(b, "fixture-a-value-0002")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := SetValue(f); v != "fixture-a-value-0002" {
		t.Error("Follow kept the old value")
	}
	if got := ids(CopiesOf([]changeset.Change{a, f, c}, "a")); len(got) != 2 {
		t.Errorf("Follow lost the link: %v", got)
	}
}

package kinds

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/store"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func catalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{{ID: "e1", Name: "dev", CanAccess: true, ServiceIDs: []string{"api", "db", "web"}}},
		Services:     []railway.Service{{ID: "api", Name: "api"}, {ID: "db", Name: "db"}, {ID: "web", Name: "web"}},
	}}}
}

// fixtureVars: db.URL ← api.DB ← web.API_DB; shared.S ← api.S; db.KEY sealed.
func fixtureVars() []railway.VarRecord {
	return []railway.VarRecord{
		{EnvID: "e1", ServiceID: "db", Name: "URL", Unrendered: "pg-one"},
		{EnvID: "e1", ServiceID: "api", Name: "DB", Unrendered: "${{db.URL}}"},
		{EnvID: "e1", ServiceID: "web", Name: "API_DB", Unrendered: "${{api.DB}}"},
		{EnvID: "e1", ServiceID: "", Name: "S", Unrendered: "shared-one"},
		{EnvID: "e1", ServiceID: "api", Name: "S", Unrendered: "${{shared.S}}"},
		{EnvID: "e1", ServiceID: "db", Name: "KEY", Sealed: true},
		{EnvID: "e1", ServiceID: "api", Name: "PLAIN", Unrendered: "one"},
	}
}

func fixtureStore(t *testing.T) *store.Store {
	t.Helper()
	s := store.New()
	s.ReplaceCatalog(catalog())
	s.ReplaceVariables("p1", fixtureVars())
	return s
}

func place(svc string) domain.Place {
	return domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: svc}
}

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

// sleeps records the durations a step asked to sleep.
type sleeps struct{ got []time.Duration }

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.got = append(s.got, d)
	return nil
}

// newFake starts a fake holding the fixture variables.
func newFake(t *testing.T) *railwaytest.Fake {
	t.Helper()
	f := railwaytest.New(t)
	f.SetCatalog(catalog())
	for _, r := range fixtureVars() {
		f.SetVar(r.EnvID, r.ServiceID, r.Name, r.Unrendered, r.Sealed)
	}
	return f
}

func newEnv(t *testing.T, f *railwaytest.Fake, s *store.Store) (*apply.Env, *sleeps) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	j, err := apply.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	sl := &sleeps{}
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, []byte("ssh.railway.test "+testHostKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &apply.Env{
		KnownHosts: known,
		API: &railway.Client{
			T:            &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}},
			WriteAllowed: func(string) bool { return true },
		},
		Store:   s,
		Journal: j,
		RunID:   "r1",
		Sleep:   sl.sleep,
		Now:     func() time.Time { return now },
	}, sl
}

func mustSet(t *testing.T, s *store.Store, p domain.Place, name, value string) changeset.Change {
	t.Helper()
	c, err := SetVariable(s, nil, p, name, value, now)
	if err != nil {
		t.Fatalf("SetVariable(%s): %v", name, err)
	}
	return c
}

func TestSetVariableResolvesAtCreation(t *testing.T) {
	s := fixtureStore(t)
	c := mustSet(t, s, place("web"), "NEW", "x ${{db.URL}} y")
	if c.Kind != "variable.set" || c.Target != "NEW" || c.ID == "" || !c.Created.Equal(now) {
		t.Errorf("change = %+v", c)
	}
	if !c.Base.Absent {
		t.Errorf("base of a new variable should be absent: %+v", c.Base)
	}
	var nv struct {
		Value string   `json:"value"`
		Refs  []string `json:"refs"`
	}
	if err := json.Unmarshal(c.New, &nv); err != nil {
		t.Fatal(err)
	}
	if nv.Value != "x ${{db.URL}} y" || !reflect.DeepEqual(nv.Refs, []string{"e1/db/URL"}) {
		t.Errorf("New = %s", c.New)
	}
	reg := NewRegistry()
	if got := reg.Reads(c); !reflect.DeepEqual(got, []string{"var:e1/db/URL"}) {
		t.Errorf("Reads = %v", got)
	}
	if got := reg.Writes(c); !reflect.DeepEqual(got, []string{"var:e1/web/NEW"}) {
		t.Errorf("Writes = %v", got)
	}

	ex := mustSet(t, s, place("db"), "URL", "pg-two")
	if ex.Base.Absent || ex.Base.Fingerprint != s.Variable("e1/db/URL").Fingerprint || ex.Base.Value != "" {
		t.Errorf("base of an existing variable = %+v", ex.Base)
	}
	if sealed := mustSet(t, s, place("db"), "KEY", "k2"); !sealed.Base.Sealed {
		t.Errorf("sealed base = %+v", sealed.Base)
	}

	for value, want := range map[string]string{
		"${{nope.X}}":    "no service nope in dev",
		"${{db.NOPE}}":   "db has no NOPE",
		"${{shared.Q}}":  "shared has no Q",
		"${{MISSING_1}}": "web has no MISSING_1",
	} {
		_, err := SetVariable(s, nil, place("web"), "NEW", value, now)
		if err == nil || err.Error() != want {
			t.Errorf("SetVariable(%s) error = %v, want %q", value, err, want)
		}
	}
}

func TestSetVariableReferencesPending(t *testing.T) {
	s := fixtureStore(t)
	a := mustSet(t, s, place("db"), "A_NEW", "v")
	b, err := SetVariable(s, []changeset.Change{a}, place("web"), "B", "${{db.A_NEW}} ${{shared.S}}", now)
	if err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if got := NewRegistry().Reads(b); !reflect.DeepEqual(got, []string{"var:e1/db/A_NEW", "var:e1/shared/S"}) {
		t.Errorf("Reads = %v", got)
	}
	// Same-service and shared pending targets resolve too.
	same := mustSet(t, s, place("web"), "C_NEW", "v")
	sh := mustSet(t, s, place(""), "SH_NEW", "v")
	if _, err := SetVariable(s, []changeset.Change{same, sh}, place("web"), "D", "${{C_NEW}}${{shared.SH_NEW}}", now); err != nil {
		t.Errorf("same-service/shared pending: %v", err)
	}
	// A pending delete adds nothing.
	del := mustDelete(t, s, place("db"), "GONE")
	if _, err := SetVariable(s, []changeset.Change{del}, place("web"), "E", "${{db.GONE}}", now); err == nil {
		t.Error("a pending delete is no target")
	}
	// Validate accepts the stored ref without the pending change at hand,
	// and still refuses an unresolvable one.
	set := NewRegistry()["variable.set"]
	if err := set.Validate(s, b); err != nil {
		t.Errorf("Validate: %v", err)
	}
	bad := b
	bad.New = []byte(`{"value":"${{db.OTHER_NEW}}","refs":["e1/db/A_NEW"]}`)
	if err := set.Validate(s, bad); err == nil || err.Error() != "db has no OTHER_NEW" {
		t.Errorf("Validate bad = %v", err)
	}
}

func TestSetVariableRefusesRailwayNames(t *testing.T) {
	s := fixtureStore(t)
	_, err := SetVariable(s, nil, place("api"), "RAILWAY_X", "v", now)
	if err == nil || !strings.Contains(err.Error(), "managed by Railway") {
		t.Errorf("RAILWAY_X: %v", err)
	}
	_, err = SetVariable(s, nil, place("api"), "1BAD", "v", now)
	if err == nil || !strings.Contains(err.Error(), "invalid variable name") {
		t.Errorf("1BAD: %v", err)
	}
	if _, err := DeleteVariable(s, place("api"), "RAILWAY_PUBLIC_DOMAIN", now); err == nil || !strings.Contains(err.Error(), "managed by Railway") {
		t.Errorf("delete RAILWAY_*: %v", err)
	}
	// Validate repeats the checks on stored changes.
	c := mustSet(t, s, place("api"), "OK_1", "v")
	c.Target = "RAILWAY_Y"
	if err := NewRegistry()["variable.set"].Validate(s, c); err == nil {
		t.Error("Validate should refuse RAILWAY_Y")
	}
}

func TestDeleteReferencedRefused(t *testing.T) {
	s := fixtureStore(t)
	_, err := DeleteVariable(s, place("db"), "URL", now)
	if err == nil || err.Error() != "referenced by 1 variables: delete or change them first" {
		t.Errorf("delete db.URL: %v", err)
	}
	c, err := DeleteVariable(s, place("web"), "API_DB", now)
	if err != nil {
		t.Fatalf("delete web.API_DB: %v", err)
	}
	if c.Kind != "variable.delete" || c.Target != "API_DB" || c.Base.Absent || c.New != nil {
		t.Errorf("change = %+v", c)
	}
	if err := NewRegistry()["variable.delete"].Validate(s, c); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestVariableNoopAndOverlay(t *testing.T) {
	s := fixtureStore(t)
	reg := NewRegistry()
	set, del := reg["variable.set"], reg["variable.delete"]
	if !set.IsNoop(mustSet(t, s, place("api"), "PLAIN", "one")) {
		t.Error("set to the same value should be a noop")
	}
	if set.IsNoop(mustSet(t, s, place("api"), "PLAIN", "two")) {
		t.Error("set to another value is not a noop")
	}
	if set.IsNoop(mustSet(t, s, place("db"), "KEY", "")) {
		t.Error("a sealed base is never a noop")
	}
	gone, err := DeleteVariable(s, place("api"), "GONE", now)
	if err != nil {
		t.Fatal(err)
	}
	if !del.IsNoop(gone) {
		t.Error("deleting an absent variable is a noop")
	}

	for _, tc := range []struct {
		c    changeset.Change
		k    apply.Kind
		want apply.Overlay
	}{
		{mustSet(t, s, place("api"), "NEW", "v"), set, apply.Overlay{Short: "+ added", Long: "+ added", Tone: "new"}},
		{mustSet(t, s, place("api"), "PLAIN", "héllo"), set, apply.Overlay{Short: "→ 5 chars", Long: "→ new value (5 chars)", Tone: "new"}},
		{gone, del, apply.Overlay{Short: "✗ delete", Long: "✗ delete", Tone: "danger"}},
	} {
		if got := tc.k.Overlay(tc.c); got != tc.want {
			t.Errorf("Overlay(%s) = %+v, want %+v", tc.c.Target, got, tc.want)
		}
		if !tc.k.Secret(tc.c) {
			t.Errorf("Secret(%s) = false", tc.c.Target)
		}
	}
}

func TestVariablePatchFragments(t *testing.T) {
	s := fixtureStore(t)
	reg := NewRegistry()
	c := mustSet(t, s, place("api"), "PLAIN", "two")
	frag, ok := reg["variable.set"].Patch(c)
	want := map[string]any{"services": map[string]any{"api": map[string]any{"variables": map[string]any{"PLAIN": map[string]any{"value": "two"}}}}}
	if !ok || !reflect.DeepEqual(frag, want) {
		t.Errorf("set fragment = %v", frag)
	}
	d, err := DeleteVariable(s, place(""), "OLD", now)
	if err != nil {
		t.Fatal(err)
	}
	frag, ok = reg["variable.delete"].Patch(d)
	want = map[string]any{"sharedVariables": map[string]any{"OLD": nil}}
	if !ok || !reflect.DeepEqual(frag, want) {
		t.Errorf("shared delete fragment = %v", frag)
	}
}

func TestVariableCheck(t *testing.T) {
	s := fixtureStore(t)
	set := NewRegistry()["variable.set"]
	del := NewRegistry()["variable.delete"]
	c := mustSet(t, s, place("api"), "PLAIN", "two")
	for _, tc := range []struct {
		name  string
		fake  func(f *railwaytest.Fake)
		c     changeset.Change
		k     apply.Kind
		drift apply.Drift
	}{
		{"holds base", func(*railwaytest.Fake) {}, c, set, apply.DriftNone},
		{"holds new", func(f *railwaytest.Fake) { f.SetVar("e1", "api", "PLAIN", "two", false) }, c, set, apply.DriftAlreadyNew},
		{"holds other", func(f *railwaytest.Fake) { f.SetVar("e1", "api", "PLAIN", "three", false) }, c, set, apply.DriftChanged},
		{"set absent: still absent", func(*railwaytest.Fake) {}, mustSet(t, s, place("api"), "NEW_ONE", "v"), set, apply.DriftNone},
		{"set: base emptied", func(f *railwaytest.Fake) { f.SetVar("e1", "api", "PLAIN", "", false) }, c, set, apply.DriftChanged},
		{"set absent: someone added", func(f *railwaytest.Fake) { f.SetVar("e1", "api", "NEW_ONE", "w", false) },
			mustSet(t, s, place("api"), "NEW_ONE", "v"), set, apply.DriftChanged},
		{"sealed base", func(*railwaytest.Fake) {}, mustSet(t, s, place("db"), "KEY", "k2"), set, apply.DriftNone},
		{"sealed base gone", func(*railwaytest.Fake) {}, sealedSetAt(t, s, "web"), set, apply.DriftChanged},
		{"sealed delete: still there", func(*railwaytest.Fake) {}, mustDelete(t, s, place("db"), "KEY"), del, apply.DriftNone},
		// The fake cannot delete; the same change aimed at a service without KEY.
		{"sealed delete: gone", func(*railwaytest.Fake) {}, sealedDeleteAt(t, s, "web"), del, apply.DriftAlreadyNew},
		{"delete: still there", func(*railwaytest.Fake) {}, mustDelete(t, s, place("api"), "PLAIN"), del, apply.DriftNone},
		{"delete: already gone", func(f *railwaytest.Fake) {}, mustDelete(t, s, place("api"), "ABSENT"), del, apply.DriftAlreadyNew},
		{"delete: changed", func(f *railwaytest.Fake) { f.SetVar("e1", "api", "PLAIN", "three", false) },
			mustDelete(t, s, place("api"), "PLAIN"), del, apply.DriftChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			tc.fake(f)
			env, _ := newEnv(t, f, s)
			got, err := tc.k.Check(context.Background(), env, tc.c)
			if err != nil || got != tc.drift {
				t.Errorf("Check = %v, %v; want %v", got, err, tc.drift)
			}
		})
	}
}

// sealedSetAt is a sealed set of db.KEY aimed at another service, where
// KEY does not exist (the fake cannot delete).
func sealedSetAt(t *testing.T, s *store.Store, svc string) changeset.Change {
	t.Helper()
	c := mustSet(t, s, place("db"), "KEY", "k2")
	c.Place = place(svc)
	return c
}

func sealedDeleteAt(t *testing.T, s *store.Store, svc string) changeset.Change {
	t.Helper()
	c := mustDelete(t, s, place("db"), "KEY")
	c.Place = place(svc)
	return c
}

func mustDelete(t *testing.T, s *store.Store, p domain.Place, name string) changeset.Change {
	t.Helper()
	c, err := DeleteVariable(s, p, name, now)
	if err != nil {
		t.Fatalf("DeleteVariable(%s): %v", name, err)
	}
	return c
}

func TestVariableRedeploys(t *testing.T) {
	s := fixtureStore(t)
	set := NewRegistry()["variable.set"]
	keys := func(ps []domain.Place) []string {
		var out []string
		for _, p := range ps {
			if p.ProjectID != "p1" {
				t.Errorf("place %s has project %q", p.Key(), p.ProjectID)
			}
			out = append(out, p.Key())
		}
		return out
	}
	if got := keys(set.Redeploys(s, mustSet(t, s, place("db"), "URL", "pg-two"))); !reflect.DeepEqual(got, []string{"e1/db", "e1/api", "e1/web"}) {
		t.Errorf("db.URL redeploys = %v", got)
	}
	if got := keys(set.Redeploys(s, mustSet(t, s, place(""), "S", "shared-two"))); !reflect.DeepEqual(got, []string{"e1/api"}) {
		t.Errorf("shared S redeploys = %v", got)
	}

	// A reference cycle: a location is its own dependent; places stay unique.
	cyc := store.New()
	cyc.ReplaceCatalog(catalog())
	cyc.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "api", Name: "A", Unrendered: "${{web.B}}"},
		{EnvID: "e1", ServiceID: "web", Name: "B", Unrendered: "${{api.A}}"},
	})
	if got := keys(set.Redeploys(cyc, mustSet(t, cyc, place("api"), "A", "v"))); !reflect.DeepEqual(got, []string{"e1/api", "e1/web"}) {
		t.Errorf("cycle redeploys = %v", got)
	}
}

// TestVariableBadDataNeverPanicsOrSends: a variable.set frozen in a run is
// never validated. Compile (which calls Patch first) must not panic on
// corrupt data, Patch must not carry a value for it, and the run must fail
// before anything is sent.
func TestVariableBadDataNeverPanicsOrSends(t *testing.T) {
	for _, bad := range []string{
		``,
		`{`,
		`null`,
		`{}`,
		`{"value":null}`,
		`{"value":1}`,
		`{"value":"x","extra":1}`,
		`{"value":"x"} {}`,
	} {
		s := fixtureStore(t)
		f := newFake(t)
		c := mustSet(t, s, place("api"), "PLAIN", "two")
		c.New = json.RawMessage(bad)
		reg := NewRegistry()
		var frag map[string]any
		var g apply.Graph
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%q: panicked: %v", bad, r)
				}
			}()
			frag, _ = reg["variable.set"].Patch(c)
			var err error
			g, err = apply.Compile(changeset.ChangeSet{Changes: []changeset.Change{c}}, s, reg,
				func(envID string, changes []changeset.Change) apply.Step {
					return NewPatchStep(envID, changes, reg, "p1")
				}, NewRedeployStep)
			if err != nil {
				t.Fatalf("%q: Compile: %v", bad, err)
			}
		}()
		if len(frag) != 0 {
			t.Errorf("%q: Patch = %v, want empty", bad, frag)
		}
		st := g.Steps["patch:e1"]
		if st == nil {
			t.Fatalf("%q: no patch step: %v", bad, g.Order)
		}
		env, _ := newEnv(t, f, s)
		if _, err := st.Run(context.Background(), env, apply.StepRecord{}); err == nil {
			t.Errorf("%q: patch step succeeded", bad)
		}
		if _, err := reg["variable.set"].Check(context.Background(), env, c); err == nil {
			t.Errorf("%q: Check succeeded", bad)
		}
		if n := len(commits(f)); n != 0 {
			t.Errorf("%q: commits = %d, want 0", bad, n)
		}
	}
}

// TestVariableBadDataReasonHasNoSecret: a corrupt New holds a secret. The
// decoder's error can quote part of it, so the reason CheckData gives, the
// Check error, the patch step's error and the journal carry a fixed message
// and nothing of the value.
func TestVariableBadDataReasonHasNoSecret(t *testing.T) {
	for _, bad := range []string{
		`{"value":"zx\qSECRETzx"}`,
		"{\"value\":\"zx\x01SECRETzx\"}",
		`{"value":"zxSECRETzx"`,
		`{"value":"zxSECRETzx"} zx`,
	} {
		s := fixtureStore(t)
		f := newFake(t)
		reg := NewRegistry()
		c := mustSet(t, s, place("api"), "PLAIN", "two")
		c.New = json.RawMessage(bad)
		env, _ := newEnv(t, f, s)
		journalPath := filepath.Join(t.TempDir(), "journal.json")
		j, err := apply.OpenJournal(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		env.Journal = j
		var msgs []string
		if err := reg.CheckData(c); err != nil {
			msgs = append(msgs, err.Error())
		} else {
			t.Errorf("%q: CheckData passed", bad)
		}
		if _, err := reg["variable.set"].Check(context.Background(), env, c); err != nil {
			msgs = append(msgs, err.Error())
		} else {
			t.Errorf("%q: Check passed", bad)
		}
		g, err := apply.Compile(changeset.ChangeSet{Changes: []changeset.Change{c}}, s, reg,
			func(envID string, changes []changeset.Change) apply.Step {
				return NewPatchStep(envID, changes, reg, "p1")
			}, NewRedeployStep)
		if err != nil {
			t.Fatalf("%q: Compile: %v", bad, err)
		}
		var events []string
		_ = (&apply.Executor{}).Run(context.Background(), g, env, func(e apply.Event) {
			if e.Error != "" {
				events = append(events, e.Error)
			}
		})
		if len(events) == 0 {
			t.Errorf("%q: the run reported no error", bad)
		}
		msgs = append(msgs, events...)
		raw, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, string(raw))
		for i, m := range msgs[:len(msgs)-1] {
			if !strings.HasSuffix(m, "variable.set: bad change data") {
				t.Errorf("%q: message %d = %q, want the fixed reason", bad, i, m)
			}
		}
		for i, m := range msgs {
			for _, frag := range []string{"zx", "SECRET", `\q`, "\x01", "'q'", "'\\x01'"} {
				if strings.Contains(m, frag) {
					t.Errorf("%q: message %d holds %q: %q", bad, i, frag, m)
				}
			}
		}
	}
}

func TestVariableNewFingerprint(t *testing.T) {
	s := fixtureStore(t)
	reg := NewRegistry()
	long := "a long new value of 24!!"
	set := mustSet(t, s, place("api"), "PLAIN", long)
	if got, want := reg.NewFingerprint(set), domain.Fingerprint("PLAIN", long, false, true, "e1", "api"); got != want {
		t.Errorf("set: %q, want %q", got, want)
	}
	if got := reg.NewFingerprint(mustDelete(t, s, place("api"), "PLAIN")); got != "" {
		t.Errorf("delete: %q", got)
	}
	bad := set
	bad.New = json.RawMessage(`{`)
	if got := reg.NewFingerprint(bad); got != "" {
		t.Errorf("bad data: %q", got)
	}
	if got := reg.NewFingerprint(changeset.Change{Kind: "service.teleport"}); got != "" {
		t.Errorf("unknown kind: %q", got)
	}
}

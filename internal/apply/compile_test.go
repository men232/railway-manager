package apply

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

type fakeStep struct {
	key          string
	deps, serves []string
	wait         bool
	reopenable   bool
}

func (f *fakeStep) WaitsForSuccess() bool { return f.wait }
func (f *fakeStep) Reopenable() bool      { return f.reopenable }

func (f *fakeStep) Key() string      { return f.key }
func (f *fakeStep) Deps() []string   { return f.deps }
func (f *fakeStep) Serves() []string { return f.serves }
func (f *fakeStep) Lock() string     { return "" }
func (f *fakeStep) Label() string    { return f.key }
func (f *fakeStep) Run(context.Context, *Env, StepRecord) (Result, error) {
	return Result{}, nil
}

// fakeKind: patch=true contributes a patch fragment; redeploys defaults to
// the change's own place for patch kinds; own builds own steps.
type fakeKind struct {
	name      string
	patch     bool
	redeploys map[string][]domain.Place // change id → places
	own       func(c changeset.Change) []Step
	reopen    bool // Reopener
}

func (k *fakeKind) Reopen() bool { return k.reopen }

func (k *fakeKind) Name() string                                  { return k.name }
func (k *fakeKind) Validate(*store.Store, changeset.Change) error { return nil }
func (k *fakeKind) IsNoop(changeset.Change) bool                  { return false }
func (k *fakeKind) Overlay(changeset.Change) Overlay              { return Overlay{} }
func (k *fakeKind) Secret(changeset.Change) bool                  { return false }
func (k *fakeKind) Writes(c changeset.Change) []string            { return []string{c.Key()} }
func (k *fakeKind) Reads(changeset.Change) []string               { return nil }
func (k *fakeKind) Check(context.Context, *Env, changeset.Change) (Drift, error) {
	return DriftNone, nil
}
func (k *fakeKind) Patch(c changeset.Change) (map[string]any, bool) {
	if !k.patch {
		return nil, false
	}
	return map[string]any{c.Target: string(c.New)}, true
}
func (k *fakeKind) Redeploys(_ *store.Store, c changeset.Change) []domain.Place {
	if ps, ok := k.redeploys[c.ID]; ok {
		return ps
	}
	if k.patch {
		return []domain.Place{c.Place}
	}
	return nil
}
func (k *fakeKind) OwnSteps(c changeset.Change) []Step {
	if k.own == nil {
		return nil
	}
	return k.own(c)
}

func pl(env, svc string) domain.Place {
	return domain.Place{ProjectID: "p", EnvID: env, ServiceID: svc}
}

func ch(id, kind string, p domain.Place) changeset.Change {
	return changeset.Change{ID: id, Kind: kind, Place: p, Target: "T_" + id, New: json.RawMessage(`"v"`)}
}

type fixture struct {
	reg     Registry
	vars    *fakeKind
	ownDeps map[string][]string // change id → declared deps of own:<id>
}

func newFixture() *fixture {
	f := &fixture{vars: &fakeKind{name: "var", patch: true, redeploys: map[string][]domain.Place{}}, ownDeps: map[string][]string{}}
	f.reg = Registry{
		"var": f.vars,
		"restart": &fakeKind{name: "restart", own: func(c changeset.Change) []Step {
			return []Step{&fakeStep{key: "restart:" + c.Place.Key(), serves: []string{c.ID}, deps: f.ownDeps[c.ID]}}
		}},
		"redeploy": &fakeKind{name: "redeploy", own: func(c changeset.Change) []Step {
			return []Step{&fakeStep{key: "redeploy:" + c.Place.Key(), serves: []string{c.ID}, deps: f.ownDeps[c.ID]}}
		}},
		"own": &fakeKind{name: "own", own: func(c changeset.Change) []Step {
			return []Step{&fakeStep{key: "own:" + c.ID, serves: []string{c.ID}, deps: f.ownDeps[c.ID]}}
		}},
		"dbpw": &fakeKind{name: "dbpw", reopen: true, own: func(c changeset.Change) []Step {
			return []Step{&fakeStep{key: "own:" + c.ID, serves: []string{c.ID}, deps: f.ownDeps[c.ID], reopenable: true}}
		}},
		"dbpw2": &fakeKind{name: "dbpw2", reopen: true, own: func(c changeset.Change) []Step {
			return []Step{
				&fakeStep{key: "pre:" + c.ID, serves: []string{c.ID}},
				&fakeStep{key: "own:" + c.ID, serves: []string{c.ID}, deps: []string{"pre:" + c.ID}, reopenable: true},
			}
		}},
	}
	return f
}

func (f *fixture) compile(t *testing.T, cs changeset.ChangeSet) Graph {
	t.Helper()
	g, err := f.compileErr(cs)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return g
}

func (f *fixture) compileErr(cs changeset.ChangeSet) (Graph, error) {
	newPatch := func(envID string, cs []changeset.Change) Step {
		var ids []string
		for _, c := range cs {
			ids = append(ids, c.ID)
		}
		return &fakeStep{key: "patch:" + envID, serves: ids}
	}
	newRedeploy := func(p domain.Place, serves, deps []string, wait bool) Step {
		return &fakeStep{key: "redeploy:" + p.Key(), serves: serves, deps: deps, wait: wait}
	}
	return Compile(cs, store.New(), f.reg, newPatch, newRedeploy)
}

func keys(g Graph) []string {
	var ks []string
	for k := range g.Steps {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func wantDeps(t *testing.T, g Graph, key string, want ...string) {
	t.Helper()
	st, ok := g.Steps[key]
	if !ok {
		t.Fatalf("no step %s in %v", key, keys(g))
	}
	if got := sorted(st.Deps()); !slices.Equal(got, sorted(want)) {
		t.Errorf("%s deps = %v, want %v", key, got, want)
	}
}

func wantServes(t *testing.T, g Graph, key string, want ...string) {
	t.Helper()
	st, ok := g.Steps[key]
	if !ok {
		t.Fatalf("no step %s in %v", key, keys(g))
	}
	if got := sorted(st.Serves()); !slices.Equal(got, sorted(want)) {
		t.Errorf("%s serves = %v, want %v", key, got, want)
	}
}

func TestCompileOnePatchPerEnv(t *testing.T) {
	f := newFixture()
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{
		ch("a", "var", pl("e1", "s1")), ch("b", "var", pl("e1", "s2")), ch("c", "var", pl("e2", "s1")),
	}})
	wantServes(t, g, "patch:e1", "a", "b")
	wantServes(t, g, "patch:e2", "c")
	n := 0
	for _, k := range keys(g) {
		if strings.HasPrefix(k, "patch:") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("patch steps = %d in %v", n, keys(g))
	}
}

func TestCompileDerivedRedeploys(t *testing.T) {
	f := newFixture()
	f.vars.redeploys["a"] = []domain.Place{pl("e1", "s1"), pl("e1", "s2")}
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{ch("a", "var", pl("e1", "s1"))}})
	if want := []string{"patch:e1", "redeploy:e1/s1", "redeploy:e1/s2"}; !slices.Equal(keys(g), want) {
		t.Fatalf("steps = %v, want %v", keys(g), want)
	}
	wantDeps(t, g, "redeploy:e1/s1", "patch:e1")
	wantDeps(t, g, "redeploy:e1/s2", "patch:e1")
	wantServes(t, g, "redeploy:e1/s2", "a")
	if g.Order[0] != "patch:e1" {
		t.Errorf("Order = %v", g.Order)
	}
}

func TestCompileRedeployOff(t *testing.T) {
	f := newFixture()
	f.vars.redeploys["a"] = []domain.Place{pl("e1", "s1"), pl("e1", "s2")}
	g := f.compile(t, changeset.ChangeSet{
		Changes:     []changeset.Change{ch("a", "var", pl("e1", "s1"))},
		RedeployOff: []string{"e1/s2"},
	})
	if _, ok := g.Steps["redeploy:e1/s2"]; ok {
		t.Errorf("redeploy:e1/s2 present: %v", keys(g))
	}
	if _, ok := g.Steps["redeploy:e1/s1"]; !ok {
		t.Errorf("redeploy:e1/s1 missing: %v", keys(g))
	}
}

func TestCompileRestartCollapses(t *testing.T) {
	f := newFixture()
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{
		ch("r", "restart", pl("e1", "s1")), ch("a", "var", pl("e1", "s1")),
	}})
	if _, ok := g.Steps["restart:e1/s1"]; ok {
		t.Errorf("restart step present: %v", keys(g))
	}
	wantServes(t, g, "redeploy:e1/s1", "a", "r")
	wantDeps(t, g, "redeploy:e1/s1", "patch:e1")
}

func TestCompileExplicitRedeployMerges(t *testing.T) {
	f := newFixture()
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{
		ch("x", "redeploy", pl("e1", "s1")), ch("a", "var", pl("e1", "s1")),
	}})
	wantServes(t, g, "redeploy:e1/s1", "a", "x")
	wantDeps(t, g, "redeploy:e1/s1", "patch:e1")
}

func TestCompileRestartAlone(t *testing.T) {
	f := newFixture()
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{ch("r", "restart", pl("e1", "s1"))}})
	if want := []string{"restart:e1/s1"}; !slices.Equal(keys(g), want) {
		t.Fatalf("steps = %v", keys(g))
	}
	wantDeps(t, g, "restart:e1/s1")
}

func TestCompileRestartWithRedeployOffWaitsForPatch(t *testing.T) {
	f := newFixture()
	g := f.compile(t, changeset.ChangeSet{
		Changes:     []changeset.Change{ch("r", "restart", pl("e1", "s1")), ch("a", "var", pl("e1", "s1"))},
		RedeployOff: []string{"e1/s1"},
	})
	wantServes(t, g, "restart:e1/s1", "r")
	wantDeps(t, g, "restart:e1/s1", "patch:e1")
}

func TestCompileRequires(t *testing.T) {
	f := newFixture()
	a := ch("A", "own", pl("e1", "db"))
	b := ch("B", "var", pl("e1", "s1"))
	b.Requires = []string{"A"}
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{a, b}})
	wantDeps(t, g, "patch:e1", "own:A")
	if i, j := slices.Index(g.Order, "own:A"), slices.Index(g.Order, "patch:e1"); i < 0 || i > j {
		t.Errorf("Order = %v", g.Order)
	}

	b.Requires = []string{"zzz"}
	_, err := f.compileErr(changeset.ChangeSet{Changes: []changeset.Change{a, b}})
	if err == nil || !strings.Contains(err.Error(), "requires a change outside this run: zzz") {
		t.Errorf("err = %v", err)
	}
}

func TestCompileGroupBlocksAcrossEnvs(t *testing.T) {
	f := newFixture()
	x := ch("x", "var", pl("e1", "s1"))
	y := ch("y", "var", pl("e2", "s1"))
	x.Group, y.Group = "g", "g"
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{x, y}})
	wantDeps(t, g, "redeploy:e1/s1", "patch:e1", "patch:e2")
	wantDeps(t, g, "redeploy:e2/s1", "patch:e1", "patch:e2")
}

func TestCompileDropsMissingDeps(t *testing.T) {
	f := newFixture()
	f.ownDeps["o"] = []string{"redeploy:e1/s1"}
	g := f.compile(t, changeset.ChangeSet{
		Changes:     []changeset.Change{ch("a", "var", pl("e1", "s1")), ch("o", "own", pl("e1", "s1"))},
		RedeployOff: []string{"e1/s1"},
	})
	wantDeps(t, g, "own:o")
}

func TestCompileCycle(t *testing.T) {
	f := newFixture()
	f.ownDeps["o"] = []string{"patch:e1"}
	a := ch("a", "var", pl("e1", "s1"))
	a.Requires = []string{"o"}
	_, err := f.compileErr(changeset.ChangeSet{Changes: []changeset.Change{a, ch("o", "own", pl("e1", "s9"))}})
	if !errors.Is(err, ErrCycle) {
		t.Errorf("err = %v", err)
	}
}

func TestCompileUnknownKind(t *testing.T) {
	f := newFixture()
	_, err := f.compileErr(changeset.ChangeSet{Changes: []changeset.Change{ch("a", "nope", pl("e1", "s1"))}})
	if err == nil || !strings.Contains(err.Error(), "unknown kind nope") {
		t.Errorf("err = %v", err)
	}
}

func TestCompileOrderDeterministic(t *testing.T) {
	f := newFixture()
	f.vars.redeploys["a"] = []domain.Place{pl("e1", "s1"), pl("e1", "s2"), pl("e1", "s3")}
	cs := changeset.ChangeSet{Changes: []changeset.Change{
		ch("a", "var", pl("e1", "s1")), ch("b", "var", pl("e2", "s1")), ch("c", "var", pl("e3", "s4")),
		ch("o", "own", pl("e1", "s1")), ch("r", "restart", pl("e9", "s1")),
	}}
	first := f.compile(t, cs).Order
	for range 20 {
		if got := f.compile(t, cs).Order; !slices.Equal(got, first) {
			t.Fatalf("Order %v != %v", got, first)
		}
	}
	want := []string{"own:o", "patch:e1", "patch:e2", "patch:e3", "restart:e9/s1",
		"redeploy:e1/s1", "redeploy:e1/s2", "redeploy:e1/s3", "redeploy:e2/s1", "redeploy:e3/s4"}
	if !slices.Equal(first, want) {
		t.Errorf("Order = %v, want %v", first, want)
	}
}

func TestRegistryIsEffects(t *testing.T) {
	f := newFixture()
	var e changeset.Effects = f.reg
	c := ch("a", "var", pl("e1", "s1"))
	if got := e.Writes(c); !slices.Equal(got, []string{c.Key()}) {
		t.Errorf("Writes = %v", got)
	}
	if got := e.Writes(ch("a", "nope", pl("e1", "s1"))); got != nil {
		t.Errorf("unknown kind Writes = %v", got)
	}
}

type countingAPI struct {
	RailwayAPI
	calls      int
	unrendered bool
}

func (c *countingAPI) ReadVariables(_ context.Context, _, _, _ string, unrendered bool) (map[string]string, error) {
	c.calls++
	c.unrendered = unrendered
	return map[string]string{"A": "1"}, nil
}

func TestEnvReadPlaceCaches(t *testing.T) {
	api := &countingAPI{}
	env := &Env{API: api}
	p := pl("e1", "s1")
	for range 2 {
		m, err := env.ReadPlace(context.Background(), p)
		if err != nil || m["A"] != "1" {
			t.Fatalf("ReadPlace = %v, %v", m, err)
		}
	}
	if api.calls != 1 || !api.unrendered {
		t.Errorf("calls = %d unrendered = %v", api.calls, api.unrendered)
	}
	env.Forget(p)
	if _, err := env.ReadPlace(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if api.calls != 2 {
		t.Errorf("calls after Forget = %d", api.calls)
	}
}

func before(t *testing.T, g Graph, a, b string) {
	t.Helper()
	i, j := slices.Index(g.Order, a), slices.Index(g.Order, b)
	if i < 0 || j < 0 || i > j {
		t.Errorf("want %s before %s in %v", a, b, g.Order)
	}
}

func TestCompileVerifyAfterRedeploy(t *testing.T) {
	f := newFixture()
	f.ownDeps["o"] = []string{"redeploy:e1/s1"}
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{
		ch("a", "var", pl("e1", "s1")), ch("o", "own", pl("e1", "s1")),
	}})
	wantDeps(t, g, "redeploy:e1/s1", "patch:e1")
	wantDeps(t, g, "own:o", "redeploy:e1/s1")
	before(t, g, "redeploy:e1/s1", "own:o")
}

func TestCompileVerifyAfterRedeployThroughGroup(t *testing.T) {
	f := newFixture()
	f.ownDeps["o"] = []string{"redeploy:e1/s1"}
	a := ch("a", "var", pl("e1", "s1"))
	o := ch("o", "own", pl("e2", "db"))
	a.Group, o.Group = "g", "g"
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{a, o}})
	wantDeps(t, g, "redeploy:e1/s1", "patch:e1")
	wantDeps(t, g, "own:o", "redeploy:e1/s1")
	before(t, g, "redeploy:e1/s1", "own:o")
}

func TestCompilePlacesComparedByKey(t *testing.T) {
	f := newFixture()
	f.vars.redeploys["a"] = []domain.Place{{EnvID: "e1", ServiceID: "s2"}} // no project id
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{
		ch("a", "var", pl("e1", "s1")), ch("o", "own", pl("e1", "s2")),
	}})
	wantDeps(t, g, "redeploy:e1/s2", "own:o", "patch:e1")
}

func TestCompileKeepsDeclaredDepsOfActions(t *testing.T) {
	f := newFixture()
	f.ownDeps["r"] = []string{"own:o"}
	f.ownDeps["x"] = []string{"own:missing"}
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{
		ch("a", "var", pl("e1", "s1")), ch("r", "restart", pl("e1", "s1")),
		ch("x", "redeploy", pl("e1", "s1")), ch("o", "own", pl("e9", "s9")),
	}})
	if _, ok := g.Steps["restart:e1/s1"]; ok {
		t.Errorf("restart not collapsed: %v", keys(g))
	}
	wantServes(t, g, "redeploy:e1/s1", "a", "r", "x")
	wantDeps(t, g, "redeploy:e1/s1", "own:o", "patch:e1")
}

func TestCompileExplicitRedeployWrongPlace(t *testing.T) {
	f := newFixture()
	f.reg["badredeploy"] = &fakeKind{name: "badredeploy", own: func(c changeset.Change) []Step {
		return []Step{&fakeStep{key: "redeploy:e1/other", serves: []string{c.ID}}}
	}}
	_, err := f.compileErr(changeset.ChangeSet{Changes: []changeset.Change{ch("x", "badredeploy", pl("e1", "s1"))}})
	if err == nil || !strings.Contains(err.Error(), "not at the place of change x") {
		t.Errorf("err = %v", err)
	}
}

func TestCompileRequiresFallsBackToActions(t *testing.T) {
	f := newFixture()
	b := ch("B", "own", pl("e1", "s2"))
	b.Requires = []string{"R"}
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{ch("R", "restart", pl("e1", "s1")), b}})
	wantDeps(t, g, "own:B", "restart:e1/s1")
	before(t, g, "restart:e1/s1", "own:B")
}

func TestCompileGroupBlocksRestart(t *testing.T) {
	f := newFixture()
	x := ch("x", "var", pl("e2", "s1"))
	r := ch("r", "restart", pl("e1", "s1"))
	x.Group, r.Group = "g", "g"
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{x, r}})
	wantDeps(t, g, "restart:e1/s1", "patch:e2")
}

func TestCompileGroupTwoPlacesOneEnv(t *testing.T) {
	f := newFixture()
	x := ch("x", "var", pl("e1", "s1"))
	y := ch("y", "var", pl("e1", "s2"))
	o := ch("o", "own", pl("e1", "s3"))
	x.Group, y.Group, o.Group = "g", "g", "g"
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{x, y, o}})
	wantServes(t, g, "patch:e1", "x", "y")
	wantDeps(t, g, "redeploy:e1/s1", "own:o", "patch:e1")
	wantDeps(t, g, "redeploy:e1/s2", "own:o", "patch:e1")
}

func TestCompileDepOnCollapsedRestartMeansRedeploy(t *testing.T) {
	f := newFixture()
	f.ownDeps["o"] = []string{"restart:e1/s1"}
	g := f.compile(t, changeset.ChangeSet{Changes: []changeset.Change{
		ch("a", "var", pl("e1", "s1")), ch("r", "restart", pl("e1", "s1")), ch("o", "own", pl("e1", "s1")),
	}})
	if _, ok := g.Steps["restart:e1/s1"]; ok {
		t.Errorf("restart not collapsed: %v", keys(g))
	}
	wantDeps(t, g, "own:o", "redeploy:e1/s1")
	wantDeps(t, g, "redeploy:e1/s1", "patch:e1")
	before(t, g, "redeploy:e1/s1", "own:o")
}

// passwordSet is a db.password-like change D at the database and one URL
// change per env requiring it, all in group.
func passwordSet(group string) changeset.ChangeSet {
	d := ch("D", "dbpw", pl("e0", "db"))
	x := ch("x", "var", pl("e1", "s1"))
	y := ch("y", "var", pl("e2", "s1"))
	x.Requires, y.Requires = []string{"D"}, []string{"D"}
	d.Group, x.Group, y.Group = group, group, group
	return changeset.ChangeSet{Changes: []changeset.Change{d, x, y}}
}

func TestCompileDBGroupNoCrossWiring(t *testing.T) {
	f := newFixture()
	g := f.compile(t, passwordSet("rotate-db:m:root"))
	wantDeps(t, g, "redeploy:e1/s1", "own:D", "patch:e1")
	wantDeps(t, g, "redeploy:e2/s1", "own:D", "patch:e2")
	for _, k := range []string{"redeploy:e1/s1", "redeploy:e2/s1"} {
		if !waits(g.Steps[k]) {
			t.Errorf("%s does not wait for SUCCESS", k)
		}
	}

	// A failed staging patch leaves the prod redeploy unblocked.
	env := newEnv(t)
	st := g.Steps["patch:e2"]
	g.Steps["patch:e2"] = &script{key: st.Key(), deps: st.Deps(), err: errors.New("boom")}
	if err := (&Executor{}).Run(context.Background(), g, env, nil); err == nil {
		t.Fatal("Run: no error")
	}
	wantStatus(t, env, map[string]string{
		"own:D": StatusDone, "patch:e1": StatusDone, "redeploy:e1/s1": StatusDone,
		"patch:e2": StatusFailed, "redeploy:e2/s1": StatusBlocked,
	})
}

func TestCompileRotateGroupWiringUnchanged(t *testing.T) {
	f := newFixture()
	g := f.compile(t, passwordSet("rotate:abc"))
	wantDeps(t, g, "redeploy:e1/s1", "own:D", "patch:e1", "patch:e2")
	wantDeps(t, g, "redeploy:e2/s1", "own:D", "patch:e1", "patch:e2")
	for _, k := range []string{"redeploy:e1/s1", "redeploy:e2/s1"} {
		if waits(g.Steps[k]) {
			t.Errorf("%s waits for SUCCESS outside a password group", k)
		}
	}
}

func TestCompileReopen(t *testing.T) {
	f := newFixture()
	g := f.compile(t, passwordSet("rotate-db:m:root"))
	if want := map[string][]string{"own:D": {"patch:e1", "patch:e2"}}; !reflect.DeepEqual(g.Reopen, want) {
		t.Errorf("Reopen = %v, want %v", g.Reopen, want)
	}

	cs := passwordSet("rotate-db:m:root")
	cs.Changes[0].Kind = "own" // not a Reopener
	if g := f.compile(t, cs); len(g.Reopen) != 0 {
		t.Errorf("Reopen = %v, want none", g.Reopen)
	}
}

// Only a step that opts in with Reopenable is reopened.
func TestCompileReopenPerStep(t *testing.T) {
	f := newFixture()
	cs := passwordSet("rotate-db:m:root")
	cs.Changes[0].Kind = "dbpw2"
	g := f.compile(t, cs)
	if want := map[string][]string{"own:D": {"patch:e1", "patch:e2"}}; !reflect.DeepEqual(g.Reopen, want) {
		t.Errorf("Reopen = %v, want %v", g.Reopen, want)
	}
}

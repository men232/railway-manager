package kinds

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/store"
)

func runPatch(t *testing.T, f *railwaytest.Fake, s *store.Store, reg apply.Registry, prior apply.StepRecord, cs ...changeset.Change) (apply.Result, *apply.Env, *sleeps, error) {
	t.Helper()
	env, sl := newEnv(t, f, s)
	res, err := runPatchIn(t, env, reg, prior, cs...)
	return res, env, sl, err
}

func runPatchIn(t *testing.T, env *apply.Env, reg apply.Registry, prior apply.StepRecord, cs ...changeset.Change) (apply.Result, error) {
	t.Helper()
	st := NewPatchStep("e1", cs, reg, "p1")
	if st.Key() != "patch:e1" {
		t.Errorf("Key = %q", st.Key())
	}
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	if !reflect.DeepEqual(st.Serves(), ids) {
		t.Errorf("Serves = %v, want %v", st.Serves(), ids)
	}
	return st.Run(context.Background(), env, prior)
}

// commitBefore commits the changes' patch as an earlier attempt would have
// and returns its id.
func commitBefore(t *testing.T, env *apply.Env, cs ...changeset.Change) string {
	t.Helper()
	patch := map[string]any{}
	for _, c := range cs {
		frag, _ := NewRegistry()[c.Kind].Patch(c)
		merge(patch, frag)
	}
	id, err := env.API.CommitPatch(context.Background(), "e1", patch, "railway-manager r1: earlier")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func moreKeys(res apply.Result) []string {
	var keys []string
	for _, st := range res.More {
		keys = append(keys, st.Key())
	}
	return keys
}

func commits(f *railwaytest.Fake) []railwaytest.Mutation {
	var out []railwaytest.Mutation
	for _, m := range f.Mutations() {
		if m.Op == "CommitPatch" {
			out = append(out, m)
		}
	}
	return out
}

// scriptKind is a non-variable kind with a scripted Check.
type scriptKind struct {
	checks []apply.Drift
	calls  int
}

func (k *scriptKind) Name() string                                  { return "volume.size" }
func (k *scriptKind) Validate(*store.Store, changeset.Change) error { return nil }
func (k *scriptKind) IsNoop(changeset.Change) bool                  { return false }
func (k *scriptKind) Overlay(changeset.Change) apply.Overlay        { return apply.Overlay{} }
func (k *scriptKind) Secret(changeset.Change) bool                  { return false }
func (k *scriptKind) Writes(c changeset.Change) []string            { return []string{"volume:v1"} }
func (k *scriptKind) Reads(changeset.Change) []string               { return nil }
func (k *scriptKind) Check(context.Context, *apply.Env, changeset.Change) (apply.Drift, error) {
	d := k.checks[min(k.calls, len(k.checks)-1)]
	k.calls++
	return d, nil
}
func (k *scriptKind) Patch(changeset.Change) (map[string]any, bool) {
	return map[string]any{"volumes": map[string]any{"v1": map[string]any{"sizeMB": 2048}}}, true
}
func (k *scriptKind) Redeploys(*store.Store, changeset.Change) []domain.Place { return nil }
func (k *scriptKind) OwnSteps(changeset.Change) []apply.Step                  { return nil }

func TestPatchStepUsesKindCheck(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	k := &scriptKind{checks: []apply.Drift{apply.DriftNone, apply.DriftAlreadyNew}}
	reg := apply.Registry{"volume.size": k}
	c := changeset.Change{ID: "v", Kind: "volume.size", Place: place("db"), Target: "v1"}
	res, _, _, err := runPatch(t, f, s, reg, apply.StepRecord{}, c)
	if err != nil {
		t.Fatal(err)
	}
	cm := commits(f)
	want := map[string]any{"volumes": map[string]any{"v1": map[string]any{"sizeMB": float64(2048)}}}
	if len(cm) != 1 || !reflect.DeepEqual(cm[0].Vars["patch"], want) {
		t.Fatalf("commits = %+v", cm)
	}
	if k.calls != 2 {
		t.Errorf("Check calls = %d, want 2 (before and after)", k.calls)
	}
	if res.Outcomes["v"].Status != "done" {
		t.Errorf("outcomes = %+v", res.Outcomes)
	}

	// Not confirmed by the kind's Check after the commit.
	k2 := &scriptKind{checks: []apply.Drift{apply.DriftNone}}
	_, _, _, err = runPatch(t, newFake(t), s, apply.Registry{"volume.size": k2}, apply.StepRecord{}, c)
	if err == nil || err.Error() != "not in Railway after commit: v1 at e1/db" {
		t.Errorf("unconfirmed error = %v", err)
	}
}

func TestPatchStepWritesAndConfirms(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	a := mustSet(t, s, place("api"), "PLAIN", "two")
	b := mustDelete(t, s, place("web"), "API_DB")
	res, env, sl, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, a, b)
	if err != nil {
		t.Fatal(err)
	}
	cm := commits(f)
	if len(cm) != 1 {
		t.Fatalf("commits = %d, want 1", len(cm))
	}
	want := map[string]any{"services": map[string]any{
		"api": map[string]any{"variables": map[string]any{"PLAIN": map[string]any{"value": "two"}}},
		"web": map[string]any{"variables": map[string]any{"API_DB": nil}},
	}}
	if !reflect.DeepEqual(cm[0].Vars["patch"], want) {
		t.Errorf("patch = %v", cm[0].Vars["patch"])
	}
	if msg, _ := cm[0].Vars["commitMessage"].(string); !strings.HasPrefix(msg, "railway-manager r1:") || msg != "railway-manager r1: 2 changes" {
		t.Errorf("message = %q", msg)
	}
	if cm[0].Vars["skipDeploys"] != true {
		t.Error("skipDeploys should be true")
	}
	for _, id := range []string{a.ID, b.ID} {
		if res.Outcomes[id].Status != "done" {
			t.Errorf("outcome %s = %+v", id, res.Outcomes[id])
		}
	}
	if rec := env.Journal.Step("patch:e1"); rec.PatchID == "" || rec.Status != "started" {
		t.Errorf("journal record = %+v", rec)
	}
	if v, _ := f.Var("e1", "api", "PLAIN"); v != "two" {
		t.Errorf("fake api.PLAIN = %q", v)
	}
	if _, ok := f.Var("e1", "web", "API_DB"); ok {
		t.Error("web.API_DB should be deleted")
	}
	if len(sl.got) != 0 {
		t.Errorf("sleeps = %v, want none", sl.got)
	}
}

func TestPatchStepPolls(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.PatchStatusAfter(2)
	_, _, sl, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, mustSet(t, s, place("api"), "PLAIN", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sl.got, []time.Duration{2 * time.Second, 2 * time.Second}) {
		t.Errorf("sleeps = %v", sl.got)
	}

	f = newFake(t)
	f.PatchStatusAfter(1000)
	_, _, sl, err = runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, mustSet(t, s, place("api"), "PLAIN", "two"))
	if err == nil || err.Error() != "patch not committed after 60s" {
		t.Errorf("timeout error = %v", err)
	}
	if len(sl.got) != 30 {
		t.Errorf("sleeps = %d, want 30", len(sl.got))
	}
}

func TestPatchStepAlreadyNew(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.SetVar("e1", "api", "PLAIN", "two", false)
	c := mustSet(t, s, place("api"), "PLAIN", "two")
	res, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Mutations()) != 0 {
		t.Errorf("mutations = %+v, want none", f.Mutations())
	}
	if res.Outcomes[c.ID].Status != "done" {
		t.Errorf("outcome = %+v", res.Outcomes[c.ID])
	}
}

func TestPatchStepDriftUntouchedSkips(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.SetVar("e1", "api", "PLAIN", "drifted-three", false)
	a := mustSet(t, s, place("api"), "PLAIN", "two")
	b := mustSet(t, s, place("db"), "URL", "pg-two")
	res, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{Status: "pending"}, a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := apply.Outcome{Status: "skipped", Reason: "changed in Railway since the change was made"}
	if res.Outcomes[a.ID] != want {
		t.Errorf("drifted outcome = %+v", res.Outcomes[a.ID])
	}
	if res.Outcomes[b.ID].Status != "done" {
		t.Errorf("other outcome = %+v", res.Outcomes[b.ID])
	}
	cm := commits(f)
	wantPatch := map[string]any{"services": map[string]any{"db": map[string]any{"variables": map[string]any{"URL": map[string]any{"value": "pg-two"}}}}}
	if len(cm) != 1 || !reflect.DeepEqual(cm[0].Vars["patch"], wantPatch) {
		t.Errorf("commits = %+v", cm)
	}
	if cm[0].Vars["commitMessage"] != "railway-manager r1: 1 changes" {
		t.Errorf("message = %v", cm[0].Vars["commitMessage"])
	}
	if v, _ := f.Var("e1", "api", "PLAIN"); v != "drifted-three" {
		t.Error("the drifted variable must stay as it is")
	}
}

func TestPatchStepDriftAfterTouchStops(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	env, _ := newEnv(t, f, s)
	a := mustSet(t, s, place("api"), "PLAIN", "two")
	b := mustSet(t, s, place("db"), "URL", "pg-two")
	// An earlier attempt committed a; someone changed it afterwards.
	id := commitBefore(t, env, a)
	f.SetVar("e1", "api", "PLAIN", "drifted-three", false)
	_, err := runPatchIn(t, env, NewRegistry(), apply.StepRecord{Status: "failed", PatchID: id}, a, b)
	if err == nil || err.Error() != "changed in Railway after this apply already started on it: PLAIN at e1/api" {
		t.Fatalf("error = %v", err)
	}
	var stop interface{ StopRun() bool }
	if !errors.As(err, &stop) || !stop.StopRun() {
		t.Error("the error should stop the run")
	}
	var se *StopRunError
	if !errors.As(err, &se) {
		t.Error("the error should be a *StopRunError")
	}
	if strings.Contains(err.Error(), "drifted-three") || strings.Contains(err.Error(), "two") {
		t.Error("the error must not hold a value")
	}
	if n := len(commits(f)); n != 1 {
		t.Errorf("commits = %d, want only the earlier one", n)
	}
}

func TestPatchStepDriftWithoutPriorPatchSkips(t *testing.T) {
	for name, prior := range map[string]func(env *apply.Env, f *railwaytest.Fake, a changeset.Change) apply.StepRecord{
		// Started, but no patch was ever committed.
		"no patch": func(*apply.Env, *railwaytest.Fake, changeset.Change) apply.StepRecord {
			return apply.StepRecord{Status: "started"}
		},
		// The earlier patch failed: it wrote nothing.
		"failed patch": func(env *apply.Env, f *railwaytest.Fake, a changeset.Change) apply.StepRecord {
			f.FailPatches("boom")
			id := commitBefore(t, env, a)
			f.FailPatches("")
			return apply.StepRecord{Status: "failed", PatchID: id}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := fixtureStore(t)
			f := newFake(t)
			env, _ := newEnv(t, f, s)
			a := mustSet(t, s, place("api"), "PLAIN", "two")
			b := mustSet(t, s, place("db"), "URL", "pg-two")
			p := prior(env, f, a)
			f.SetVar("e1", "api", "PLAIN", "drifted-three", false)
			before := len(commits(f))
			res, err := runPatchIn(t, env, NewRegistry(), p, a, b)
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcomes[a.ID].Status != "skipped" || res.Outcomes[b.ID].Status != "done" {
				t.Errorf("outcomes = %+v", res.Outcomes)
			}
			if n := len(commits(f)) - before; n != 1 {
				t.Errorf("new commits = %d, want 1", n)
			}
		})
	}
}

func TestPatchStepWaitsOnPriorPatch(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	env, sl := newEnv(t, f, s)
	a := mustSet(t, s, place("api"), "PLAIN", "two")
	f.PatchStatusAfter(2)
	id := commitBefore(t, env, a)
	if v, _ := f.Var("e1", "api", "PLAIN"); v != "one" {
		t.Fatal("the earlier patch should still be applying")
	}
	res, err := runPatchIn(t, env, NewRegistry(), apply.StepRecord{Status: "started", PatchID: id}, a)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sl.got, []time.Duration{2 * time.Second, 2 * time.Second}) {
		t.Errorf("sleeps = %v", sl.got)
	}
	if n := len(commits(f)); n != 1 {
		t.Errorf("commits = %d, want 1 (no second patch)", n)
	}
	if res.Outcomes[a.ID].Status != "done" {
		t.Errorf("outcome = %+v", res.Outcomes[a.ID])
	}
}

// patchOtherKind deletes another variable than its change names, so the
// patch commits but the change's own target stays.
type patchOtherKind struct{ variableKind }

func (patchOtherKind) Patch(c changeset.Change) (map[string]any, bool) {
	return map[string]any{"services": map[string]any{c.Place.ServiceID: map[string]any{"variables": map[string]any{"UNRELATED": nil}}}}, true
}

func TestPatchStepSealedDeleteConfirmed(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	c := mustDelete(t, s, place("db"), "KEY")
	if !c.Base.Sealed {
		t.Fatal("base should be sealed")
	}
	reg := apply.Registry{"variable.delete": patchOtherKind{variableKind{del: true}}}
	_, _, _, err := runPatch(t, f, s, reg, apply.StepRecord{}, c)
	if err == nil || err.Error() != "not in Railway after commit: KEY at e1/db" {
		t.Errorf("error = %v", err)
	}

	// Really deleted: confirmed.
	res, _, _, err := runPatch(t, newFake(t), s, NewRegistry(), apply.StepRecord{}, c)
	if err != nil || res.Outcomes[c.ID].Status != "done" {
		t.Errorf("sealed delete: %+v, %v", res.Outcomes, err)
	}
}

func TestPatchStepRediscoversOnRetry(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.SetVar("e1", "web", "API", "${{api.PLAIN}}", false)
	env, _ := newEnv(t, f, s)
	c := mustSet(t, s, place("api"), "PLAIN", "two")
	f.FailNext("ProjectEnvironment", "flaky")
	if _, err := runPatchIn(t, env, NewRegistry(), apply.StepRecord{}, c); err == nil {
		t.Fatal("the re-read should fail")
	}
	prior := env.Journal.Step("patch:e1")
	if prior.PatchID == "" {
		t.Fatal("no patch id in the journal")
	}
	prior.Status = "failed"
	res, err := runPatchIn(t, env, NewRegistry(), prior, c)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(moreKeys(res), "redeploy:e1/web") {
		t.Errorf("More = %v", moreKeys(res))
	}
	if n := len(commits(f)); n != 1 {
		t.Errorf("commits = %d, want 1", n)
	}
}

func TestPatchStepRedeployOff(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.SetVar("e1", "web", "API", "${{api.PLAIN}}", false)
	env, _ := newEnv(t, f, s)
	env.RedeployOff = map[string]bool{"e1/web": true}
	res, err := runPatchIn(t, env, NewRegistry(), apply.StepRecord{}, mustSet(t, s, place("api"), "PLAIN", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(moreKeys(res), "redeploy:e1/web") {
		t.Errorf("More = %v, web is switched off", moreKeys(res))
	}
}

func TestPatchStepMergesSharedAndService(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	a := mustSet(t, s, place(""), "S", "shared-two")
	b := mustSet(t, s, place("api"), "PLAIN", "two")
	if _, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, a, b); err != nil {
		t.Fatal(err)
	}
	cm := commits(f)
	want := map[string]any{
		"sharedVariables": map[string]any{"S": map[string]any{"value": "shared-two"}},
		"services":        map[string]any{"api": map[string]any{"variables": map[string]any{"PLAIN": map[string]any{"value": "two"}}}},
	}
	if len(cm) != 1 || !reflect.DeepEqual(cm[0].Vars["patch"], want) {
		t.Errorf("commits = %+v", cm)
	}
}

func TestPatchStepUnknownKind(t *testing.T) {
	s := fixtureStore(t)
	c := changeset.Change{ID: "x", Kind: "nope.kind", Place: place("api"), Target: "T"}
	_, _, _, err := runPatch(t, newFake(t), s, NewRegistry(), apply.StepRecord{}, c)
	if err == nil || err.Error() != "unknown kind nope.kind" {
		t.Errorf("error = %v", err)
	}
}

func TestVariableSetSealedNoDrift(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	c := mustSet(t, s, place("db"), "KEY", "k2")
	res, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits(f)) != 1 || res.Outcomes[c.ID].Status != "done" {
		t.Errorf("commits = %d, outcome = %+v", len(commits(f)), res.Outcomes[c.ID])
	}
	if v, _ := f.Var("e1", "db", "KEY"); v != "k2" {
		t.Error("sealed variable not written")
	}
}

func TestPatchStepFailed(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	f.FailPatches("boom")
	_, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, mustSet(t, s, place("api"), "PLAIN", "two"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v", err)
	}

	f = newFake(t)
	f.FailNext("CommitPatch", "nope")
	_, _, _, err = runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, mustSet(t, s, place("api"), "PLAIN", "two"))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("commit error = %v", err)
	}
}

func TestPatchStepAddsFreshDependents(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	// Added in the dashboard after the store was loaded: web → api.PLAIN,
	// and shared.T → web.API (transitively), which is no place.
	f.SetVar("e1", "web", "API", "${{api.PLAIN}}", false)
	f.SetVar("e1", "", "T", "${{web.API}}", false)
	c := mustSet(t, s, place("api"), "PLAIN", "two")
	res, _, _, err := runPatch(t, f, s, NewRegistry(), apply.StepRecord{}, c)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, st := range res.More {
		keys = append(keys, st.Key())
		if st.Key() == "redeploy:e1/web" {
			if !reflect.DeepEqual(st.Serves(), []string{c.ID}) || !reflect.DeepEqual(st.Deps(), []string{"patch:e1"}) {
				t.Errorf("redeploy:e1/web serves %v deps %v", st.Serves(), st.Deps())
			}
		}
	}
	if !slices.Contains(keys, "redeploy:e1/web") || slices.Contains(keys, "redeploy:e1/shared") {
		t.Errorf("More = %v", keys)
	}
}

func TestPatchStepRebuildWhenDone(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	env, _ := newEnv(t, f, s)
	f.SetVar("e1", "web", "API", "${{api.PLAIN}}", false)
	c := mustSet(t, s, place("api"), "PLAIN", "two")
	a := mustSet(t, s, place("db"), "URL", "pg-two")
	// The first attempt committed c and skipped a, which had drifted; a is
	// still drifted on resume.
	id := commitBefore(t, env, c)
	f.SetVar("e1", "db", "URL", "drifted-three", false)
	if err := env.Journal.SetOutcome(c.ID, apply.Outcome{Status: "done"}); err != nil {
		t.Fatal(err)
	}
	skipped := apply.Outcome{Status: "skipped", Reason: "changed in Railway since the change was made"}
	if err := env.Journal.SetOutcome(a.ID, skipped); err != nil {
		t.Fatal(err)
	}
	res, err := runPatchIn(t, env, NewRegistry(), apply.StepRecord{Status: "done", PatchID: id}, c, a)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if n := len(commits(f)); n != 1 {
		t.Errorf("commits = %d, want only the earlier one", n)
	}
	if len(res.Outcomes) != 0 {
		t.Errorf("rebuild changed outcomes: %+v", res.Outcomes)
	}
	if keys := moreKeys(res); !slices.Equal(keys, []string{"redeploy:e1/web"}) {
		t.Errorf("More = %v, want [redeploy:e1/web]", keys)
	}
	if o := env.Journal.Outcome(a.ID); o != skipped {
		t.Errorf("a outcome = %+v", o)
	}
}

func TestPatchStepRetryIgnoresSkippedDrift(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	env, _ := newEnv(t, f, s)
	c := mustSet(t, s, place("api"), "PLAIN", "two")
	a := mustSet(t, s, place("db"), "URL", "pg-two")
	// The first attempt committed c, skipped a as drifted, then failed
	// later (e.g. re-reading the env for redeploys).
	id := commitBefore(t, env, c)
	f.SetVar("e1", "db", "URL", "drifted-three", false)
	skipped := apply.Outcome{Status: "skipped", Reason: "changed in Railway since the change was made"}
	if err := env.Journal.SetOutcome(a.ID, skipped); err != nil {
		t.Fatal(err)
	}
	res, err := runPatchIn(t, env, NewRegistry(), apply.StepRecord{Status: "failed", PatchID: id}, c, a)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := len(commits(f)); n != 1 {
		t.Errorf("commits = %d, want only the earlier one", n)
	}
	if res.Outcomes[a.ID] != skipped || res.Outcomes[c.ID].Status != "done" {
		t.Errorf("outcomes = %+v", res.Outcomes)
	}
	if v, _ := f.Var("e1", "db", "URL"); v != "drifted-three" {
		t.Error("the drifted variable must stay as it is")
	}
}

// A change an earlier attempt skipped as drifted, which a later attempt then
// committed, counts as touched: when it drifts again the run stops instead
// of skipping it on the stale skipped outcome.
func TestPatchStepCommittedAfterSkipStops(t *testing.T) {
	s := fixtureStore(t)
	f := newFake(t)
	env, _ := newEnv(t, f, s)
	a := mustSet(t, s, place("api"), "PLAIN", "two")
	journal := func(res apply.Result) {
		t.Helper()
		for id, o := range res.Outcomes {
			if err := env.Journal.SetOutcome(id, o); err != nil {
				t.Fatal(err)
			}
		}
	}
	start := func() apply.StepRecord {
		t.Helper()
		rec := env.Journal.Step("patch:e1")
		prior := rec
		rec.Status = "started"
		if err := env.Journal.Set("patch:e1", rec); err != nil {
			t.Fatal(err)
		}
		return prior
	}

	// Attempt 1: a drifted, skipped.
	f.SetVar("e1", "api", "PLAIN", "drifted-three", false)
	res, err := runPatchIn(t, env, NewRegistry(), start(), a)
	if err != nil {
		t.Fatal(err)
	}
	journal(res)
	if env.Journal.Outcome(a.ID).Status != "skipped" {
		t.Fatalf("attempt 1 outcome = %+v", env.Journal.Outcome(a.ID))
	}
	// Reverted by hand; attempt 2 commits a, then fails reading the status.
	f.SetVar("e1", "api", "PLAIN", "one", false)
	env.Forget(a.Place) // each attempt reads fresh, as a new Execute would
	f.FailNext("PatchStatus", "lost")
	res, err = runPatchIn(t, env, NewRegistry(), start(), a)
	if err == nil {
		t.Fatalf("attempt 2 should fail after its commit: outcomes %+v commits %d", res.Outcomes, len(commits(f)))
	}
	journal(res)
	if n := len(commits(f)); n != 1 {
		t.Fatalf("commits = %d, want 1", n)
	}
	rec := env.Journal.Step("patch:e1")
	rec.Status = "failed"
	if err := env.Journal.Set("patch:e1", rec); err != nil {
		t.Fatal(err)
	}
	// a drifts again after the commit: the run must stop.
	f.SetVar("e1", "api", "PLAIN", "drifted-four", false)
	env.Forget(a.Place)
	_, err = runPatchIn(t, env, NewRegistry(), env.Journal.Step("patch:e1"), a)
	if !apply.IsStopRun(err) {
		t.Fatalf("attempt 3 error = %v, want a stop-run", err)
	}
	if strings.Contains(err.Error(), "drifted") {
		t.Error("the error must not hold a value")
	}
	if strings.Contains(string(env.Journal.Step("patch:e1").Data), "two") {
		t.Error("step data must not hold a value")
	}
}

// A redeploy the patch adds for a database password's change waits for
// SUCCESS; one for any other change does not.
func TestPatchStepRedeployWaitsForDBGroup(t *testing.T) {
	for _, tc := range []struct {
		group string
		wait  bool
	}{{"rotate-db:m:root", true}, {"rotate:abc", false}, {"", false}} {
		s := fixtureStore(t)
		f := newFake(t)
		f.SetVar("e1", "web", "API", "${{api.PLAIN}}", false)
		env, _ := newEnv(t, f, s)
		c := mustSet(t, s, place("api"), "PLAIN", "two")
		c.Group = tc.group
		res, err := runPatchIn(t, env, NewRegistry(), apply.StepRecord{}, c)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, st := range res.More {
			if st.Key() == "redeploy:e1/web" {
				found = true
				if got := st.(apply.SuccessWaiter).WaitsForSuccess(); got != tc.wait {
					t.Errorf("group %q: wait = %v, want %v", tc.group, got, tc.wait)
				}
			}
		}
		if !found {
			t.Errorf("group %q: More = %v", tc.group, moreKeys(res))
		}
	}
}

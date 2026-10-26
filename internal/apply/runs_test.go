package apply_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/workdir"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func catalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{{ID: "e1", Name: "dev", CanAccess: true, ServiceIDs: []string{"api", "db", "web"}}},
		Services:     []railway.Service{{ID: "api", Name: "api"}, {ID: "db", Name: "db"}, {ID: "web", Name: "web"}},
	}}}
}

func fixtureVars() []railway.VarRecord {
	return []railway.VarRecord{
		{EnvID: "e1", ServiceID: "db", Name: "URL", Unrendered: "pg-one"},
		{EnvID: "e1", ServiceID: "api", Name: "PLAIN", Unrendered: "one"},
		{EnvID: "e1", ServiceID: "web", Name: "OTHER", Unrendered: "one"},
	}
}

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

func place(svc string) domain.Place {
	return domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: svc}
}

type fixture struct {
	runs *apply.Runs
	fake *railwaytest.Fake
	st   *store.Store
	dir  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := store.New()
	st.ReplaceCatalog(catalog())
	st.ReplaceVariables("p1", fixtureVars())
	f := railwaytest.New(t)
	f.SetCatalog(catalog())
	for _, r := range fixtureVars() {
		f.SetVar(r.EnvID, r.ServiceID, r.Name, r.Unrendered, r.Sealed)
	}
	reg := kinds.NewRegistry()
	api := &railway.Client{
		T:            &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}},
		WriteAllowed: func(string) bool { return true },
	}
	dir := t.TempDir()
	r := &apply.Runs{
		Dir:  dir,
		Reg:  reg,
		Exec: &apply.Executor{},
		NewEnv: func(runID string, j *apply.Journal) *apply.Env {
			return &apply.Env{
				API: api, Store: st, Journal: j, RunID: runID,
				Sleep: func(context.Context, time.Duration) error { return nil },
				Now:   func() time.Time { return now },
			}
		},
		NewRedeploy: kinds.NewRedeployStep,
		Compile: func(_ *store.Store, cs changeset.ChangeSet) (apply.Graph, error) {
			return apply.Compile(cs, st, reg,
				func(envID string, changes []changeset.Change) apply.Step {
					return kinds.NewPatchStep(envID, changes, reg, "p1")
				},
				kinds.NewRedeployStep)
		},
	}
	return &fixture{runs: r, fake: f, st: st, dir: dir}
}

func (fx *fixture) set(t *testing.T, pending []changeset.Change, svc, name, value string) changeset.Change {
	t.Helper()
	c, err := kinds.SetVariable(fx.st, pending, place(svc), name, value, now)
	if err != nil {
		t.Fatalf("SetVariable(%s): %v", name, err)
	}
	return c
}

func liveOf(t *testing.T, cs ...changeset.Change) *changeset.ChangeSet {
	t.Helper()
	live := &changeset.ChangeSet{}
	for _, c := range cs {
		if err := live.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	return live
}

func journalOf(t *testing.T, dir, runID string) *apply.Journal {
	t.Helper()
	j, err := apply.OpenJournal(filepath.Join(dir, "runs", runID, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func countOps(f *railwaytest.Fake, op string) int {
	n := 0
	for _, m := range f.Mutations() {
		if m.Op == op {
			n++
		}
	}
	return n
}

func mustPrepare(t *testing.T, r *apply.Runs, live *changeset.ChangeSet, ids []string) string {
	t.Helper()
	id, conflicts, err := r.Prepare(live, ids)
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("Prepare: %v, conflicts %v", err, conflicts)
	}
	return id
}

var runIDRE = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{12}$`)

func TestPrepareLocksSelection(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	b := fx.set(t, nil, "web", "OTHER", "two")
	c := fx.set(t, nil, "db", "NEW", "x")
	live := liveOf(t, a, b, c)
	live.ToggleRedeploy(place("web").Key())

	id := mustPrepare(t, fx.runs, live, []string{a.ID, b.ID})
	if !runIDRE.MatchString(id) {
		t.Errorf("run id %q does not match %s", id, runIDRE)
	}
	path := filepath.Join(fx.dir, "runs", id, "changeset.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("frozen set mode = %v, want 0600", fi.Mode().Perm())
	}
	frozen, err := changeset.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen.Changes) != 2 || frozen.Changes[0].ID != a.ID || frozen.Changes[1].ID != b.ID {
		t.Errorf("frozen changes = %+v, want %s and %s", frozen.Changes, a.ID, b.ID)
	}
	if len(frozen.RedeployOff) != 1 || frozen.RedeployOff[0] != place("web").Key() {
		t.Errorf("frozen RedeployOff = %v", frozen.RedeployOff)
	}
	for _, want := range []struct {
		id, run string
	}{{a.ID, id}, {b.ID, id}, {c.ID, ""}} {
		got, _ := live.Get(want.id)
		if got.RunID != want.run {
			t.Errorf("live %s RunID = %q, want %q", got.Target, got.RunID, want.run)
		}
	}
	if s := journalOf(t, fx.dir, id).State; s != "running" {
		t.Errorf("journal state = %q, want running", s)
	}

	// The locked changes cannot be selected again.
	if _, _, err := fx.runs.Prepare(live, []string{a.ID}); !errors.Is(err, changeset.ErrLocked) {
		t.Errorf("Prepare of a locked change: err = %v, want ErrLocked", err)
	}
	// No ids: every unlocked change, here only c.
	id2 := mustPrepare(t, fx.runs, live, nil)
	if got, _ := live.Get(c.ID); got.RunID != id2 {
		t.Errorf("Prepare(nil) did not take the unlocked change: RunID = %q", got.RunID)
	}
}

func TestPrepareRefusesConflictWithActiveRun(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "db", "NEW", "x")
	live := liveOf(t, a)
	r1 := mustPrepare(t, fx.runs, live, []string{a.ID})

	ref := fx.set(t, live.Changes, "api", "REF", "${{db.NEW}}")
	if err := live.Put(ref, nil); err != nil {
		t.Fatal(err)
	}
	id, conflicts, err := fx.runs.Prepare(live, []string{ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Errorf("run id = %q, want none", id)
	}
	if len(conflicts) != 1 || conflicts[0].OtherRun != r1 || conflicts[0].Other.ID != a.ID || conflicts[0].Candidate.ID != ref.ID {
		t.Fatalf("conflicts = %+v, want one with run %s", conflicts, r1)
	}
	if got, _ := live.Get(ref.ID); got.RunID != "" {
		t.Errorf("refused change locked by %q", got.RunID)
	}
	entries, _ := os.ReadDir(filepath.Join(fx.dir, "runs"))
	if len(entries) != 1 {
		t.Errorf("runs dir has %d entries, want 1", len(entries))
	}
}

func TestPrepareRefusesReadOfUnselected(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "db", "NEW", "x")
	ref := fx.set(t, []changeset.Change{a}, "api", "REF", "${{db.NEW}}")
	live := liveOf(t, a, ref)

	id, conflicts, err := fx.runs.Prepare(live, []string{ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	if id != "" || len(conflicts) != 1 || conflicts[0].OtherRun != "" || conflicts[0].Other.ID != a.ID {
		t.Fatalf("id %q, conflicts = %+v, want one with the unselected rest", id, conflicts)
	}
}

func TestPrepareImmediateChecksRuns(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	r1 := mustPrepare(t, fx.runs, live, nil)

	same := fx.set(t, nil, "api", "PLAIN", "three")
	if id, conflicts, err := fx.runs.PrepareImmediate(same); err != nil || id != "" || len(conflicts) != 1 || conflicts[0].OtherRun != r1 {
		t.Fatalf("PrepareImmediate = %q, %+v, %v; want a conflict with %s", id, conflicts, err, r1)
	}
	id, conflicts, err := fx.runs.PrepareImmediate(kinds.Restart(place("web"), now))
	if err != nil || len(conflicts) > 0 || id == "" {
		t.Fatalf("PrepareImmediate(restart) = %q, %+v, %v", id, conflicts, err)
	}
	frozen, err := changeset.Load(filepath.Join(fx.dir, "runs", id, "changeset.json"))
	if err != nil || len(frozen.Changes) != 1 {
		t.Fatalf("frozen = %+v, %v", frozen, err)
	}
}

func TestExecuteFinishedReconciles(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	b := fx.set(t, nil, "web", "OTHER", "two")
	live := liveOf(t, a, b)
	id := mustPrepare(t, fx.runs, live, nil)

	var events []apply.Event
	var mu sync.Mutex
	rep, err := fx.runs.Execute(context.Background(), id, func(e apply.Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rep.Done) != 2 || len(rep.Kept) != 0 {
		t.Errorf("report = %+v, want both done", rep)
	}
	if len(events) == 0 {
		t.Error("no events")
	}
	apply.Reconcile(live, id, rep)
	if len(live.Changes) != 0 {
		t.Errorf("live set after reconcile = %+v, want empty", live.Changes)
	}
	if s := journalOf(t, fx.dir, id).State; s != "finished" {
		t.Errorf("journal state = %q, want finished", s)
	}
	if v, _ := fx.fake.Var("e1", "api", "PLAIN"); v != "two" {
		t.Errorf("api.PLAIN = %q", v)
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "runs", id, "lock")); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
	unf, errs := fx.runs.Unfinished()
	if len(errs) != 0 || len(unf) != 0 {
		t.Errorf("Unfinished = %v, %v; want none", unf, errs)
	}
	list, errs := fx.runs.List()
	if len(errs) != 0 || len(list) != 1 || list[0].ID != id || list[0].State != "finished" || list[0].Changes != 2 || len(list[0].Steps) == 0 {
		t.Errorf("List = %+v, %v", list, errs)
	}
	if _, err := fx.runs.Execute(context.Background(), id, nil); err == nil {
		t.Error("Execute of a finished run: no error")
	}
}

func TestExecuteSkippedKept(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)
	fx.fake.SetVar("e1", "api", "PLAIN", "someone-else", false)

	rep, err := fx.runs.Execute(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f := rep.Kept[a.ID]; !strings.HasPrefix(f, "skipped: ") || len(rep.Done) != 0 {
		t.Errorf("report = %+v, want %s kept as skipped", rep, a.ID)
	}
	apply.Reconcile(live, id, rep)
	if got, ok := live.Get(a.ID); !ok || got.RunID != "" || got.Flag != rep.Kept[a.ID] {
		t.Errorf("live change = %+v, %v", got, ok)
	}
	if s := journalOf(t, fx.dir, id).State; s != "finished" {
		t.Errorf("journal state = %q, want finished", s)
	}
	if n := countOps(fx.fake, "Redeploy"); n != 0 {
		t.Errorf("a redeploy serving only a skipped change ran %d times", n)
	}
}

func TestExecuteWrittenNotDeployed(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)
	fx.fake.FailNext("Redeploy", "boom")

	rep, err := fx.runs.Execute(context.Background(), id, nil)
	if err == nil {
		t.Error("Execute with a failed redeploy: no error")
	}
	if rep.Kept[a.ID] != "written, not deployed" || len(rep.Done) != 0 {
		t.Errorf("report = %+v, want written, not deployed", rep)
	}
	apply.Reconcile(live, id, rep)
	if got, _ := live.Get(a.ID); got.RunID != id || got.Flag != "written, not deployed" {
		t.Errorf("live change after a failed run = %+v, want flagged and still locked", got)
	}
	if s := journalOf(t, fx.dir, id).State; s != "failed" {
		t.Errorf("journal state = %q, want failed", s)
	}
	unf, errs := fx.runs.Unfinished()
	if len(errs) != 0 || len(unf[id].Changes) != 1 {
		t.Errorf("Unfinished = %v, %v; want the failed run", unf, errs)
	}
	commits, redeploys := countOps(fx.fake, "CommitPatch"), countOps(fx.fake, "Redeploy")

	// Resume: only the redeploy runs.
	rep, err = fx.runs.Execute(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := countOps(fx.fake, "CommitPatch"); got != commits {
		t.Errorf("resume committed again: %d patches, want %d", got, commits)
	}
	if got := countOps(fx.fake, "Redeploy"); got != redeploys+1 {
		t.Errorf("resume redeploys = %d, want %d", got, redeploys+1)
	}
	if _, ok := rep.Done[a.ID]; !ok {
		t.Errorf("resume report = %+v, want done", rep)
	}
	if s := journalOf(t, fx.dir, id).State; s != "finished" {
		t.Errorf("journal state = %q, want finished", s)
	}
	apply.Reconcile(live, id, rep)
	if len(live.Changes) != 0 {
		t.Errorf("live after the resumed run = %+v, want empty", live.Changes)
	}
}

func TestExecuteFailedFlag(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)
	fx.fake.FailNext("CommitPatch", "first line\nsecond line")

	rep, err := fx.runs.Execute(context.Background(), id, nil)
	if err == nil {
		t.Error("no error")
	}
	f := rep.Kept[a.ID]
	if !strings.HasPrefix(f, "failed: ") || strings.Contains(f, "\n") || strings.Contains(f, "second line") {
		t.Errorf("flag = %q, want the first error line", f)
	}
}

func TestExecuteCancelKeepsRunning(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := fx.runs.Execute(ctx, id, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if s := journalOf(t, fx.dir, id).State; s != "running" {
		t.Errorf("journal state = %q, want running", s)
	}
}

func TestTwoRunsConcurrently(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	b := fx.set(t, nil, "web", "OTHER", "two")
	live := liveOf(t, a, b)
	r1 := mustPrepare(t, fx.runs, live, []string{a.ID})
	r2 := mustPrepare(t, fx.runs, live, []string{b.ID})

	reps := make([]apply.Report, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []string{r1, r2} {
		wg.Go(func() { reps[i], errs[i] = fx.runs.Execute(context.Background(), id, nil) })
	}
	wg.Wait()
	for i, id := range []string{r1, r2} {
		if errs[i] != nil {
			t.Errorf("run %s: %v", id, errs[i])
		}
		if len(reps[i].Done) != 1 {
			t.Errorf("run %s report = %+v", id, reps[i])
		}
		if s := journalOf(t, fx.dir, id).State; s != "finished" {
			t.Errorf("run %s state = %q", id, s)
		}
		apply.Reconcile(live, id, reps[i])
	}
	if n := countOps(fx.fake, "CommitPatch"); n != 2 {
		t.Errorf("CommitPatch mutations = %d, want 2", n)
	}
	if len(live.Changes) != 0 {
		t.Errorf("live = %+v", live.Changes)
	}
}

func TestAbandonUnlocks(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)

	if err := fx.runs.Abandon(id); err != nil {
		t.Fatal(err)
	}
	live.Unlock(id, nil)
	if got, _ := live.Get(a.ID); got.RunID != "" {
		t.Errorf("RunID = %q after abandon", got.RunID)
	}
	if s := journalOf(t, fx.dir, id).State; s != "abandoned" {
		t.Errorf("state = %q, want abandoned", s)
	}
	unf, errs := fx.runs.Unfinished()
	if len(errs) != 0 || len(unf) != 0 {
		t.Errorf("Unfinished = %v, %v", unf, errs)
	}
	if _, err := fx.runs.Execute(context.Background(), id, nil); err == nil {
		t.Error("Execute of an abandoned run: no error")
	}
	if countOps(fx.fake, "CommitPatch") != 0 {
		t.Error("abandoned run wrote")
	}
	// The change can go into a new run now.
	mustPrepare(t, fx.runs, live, nil)
}

func TestExecuteLockedByLiveProcess(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)
	lock := filepath.Join(fx.dir, "runs", id, "lock")
	if err := os.WriteFile(lock, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := fx.runs.Execute(context.Background(), id, nil)
	var le *workdir.LockedError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *workdir.LockedError", err)
	}
	if countOps(fx.fake, "CommitPatch") != 0 {
		t.Error("locked run wrote")
	}
	if err := fx.runs.Abandon(id); !errors.As(err, &le) {
		t.Errorf("Abandon of a held run: err = %v, want *workdir.LockedError", err)
	}
	if s := journalOf(t, fx.dir, id).State; s != "running" {
		t.Errorf("state = %q, want running", s)
	}
}

func TestRunsUnfinishedFromOtherInstance(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)

	// Another instance's Runs over the same work dir sees the run.
	other := &apply.Runs{Dir: fx.dir, Reg: fx.runs.Reg}
	unf, errs := other.Unfinished()
	if len(errs) != 0 || len(unf[id].Changes) != 1 || unf[id].Changes[0].ID != a.ID {
		t.Fatalf("Unfinished = %+v, %v", unf, errs)
	}
	same := fx.set(t, nil, "api", "PLAIN", "three")
	if _, conflicts, err := other.Prepare(liveOf(t, same), nil); err != nil || len(conflicts) != 1 || conflicts[0].OtherRun != id {
		t.Errorf("conflicts = %+v, %v", conflicts, err)
	}
}

func TestExecuteFailedRestartNotWritten(t *testing.T) {
	fx := newFixture(t)
	fx.fake.SetActiveDeployment("e1", "api", "d1")
	x := kinds.Restart(place("api"), now)
	id, conflicts, err := fx.runs.PrepareImmediate(x)
	if err != nil || len(conflicts) > 0 {
		t.Fatal(err, conflicts)
	}
	fx.fake.FailNext("RestartDeployment", "restart refused")

	rep, err := fx.runs.Execute(context.Background(), id, nil)
	if err == nil {
		t.Error("no error")
	}
	if f := rep.Kept[x.ID]; !strings.HasPrefix(f, "failed: ") || !strings.Contains(f, "restart refused") {
		t.Errorf("flag = %q, want failed with the error", f)
	}
}

// freshReferrer makes web reference api.PLAIN in Railway only, so the
// redeploy of web is added by the patch step at run time, not compiled.
func freshReferrer(fx *fixture) {
	fx.fake.SetVar("e1", "web", "REF", "${{api.PLAIN}}", false)
}

func TestExecuteResumeRebuildFailsKeepsChange(t *testing.T) {
	fx := newFixture(t)
	freshReferrer(fx)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	live.ToggleRedeploy(place("api").Key()) // only the added redeploy of web
	id := mustPrepare(t, fx.runs, live, nil)
	fx.fake.FailNext("Redeploy", "boom")

	rep, err := fx.runs.Execute(context.Background(), id, nil)
	if err == nil || rep.Kept[a.ID] != "written, not deployed" {
		t.Fatalf("first run: %+v, %v; want written, not deployed", rep, err)
	}
	if got := journalOf(t, fx.dir, id).Step("redeploy:e1/web").Status; got != "failed" {
		t.Fatalf("redeploy:e1/web = %q, want failed", got)
	}

	// The resume's rebuild of the done patch fails: redeploy:e1/web is not
	// added again, but it is still failed in the journal.
	fx.fake.FailNext("ProjectEnvironment", "re-read refused")
	rep, err = fx.runs.Execute(context.Background(), id, nil)
	if err == nil {
		t.Error("resume: no error")
	}
	if _, done := rep.Done[a.ID]; done || rep.Kept[a.ID] != "written, not deployed" {
		t.Errorf("resume report = %+v, want kept written, not deployed", rep)
	}
	if s := journalOf(t, fx.dir, id).State; s != "failed" {
		t.Errorf("state = %q, want failed", s)
	}
}

func TestExecuteFrozenRedeployOff(t *testing.T) {
	fx := newFixture(t)
	freshReferrer(fx)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	live.ToggleRedeploy(place("api").Key())
	live.ToggleRedeploy(place("web").Key())
	id := mustPrepare(t, fx.runs, live, nil)

	rep, err := fx.runs.Execute(context.Background(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := countOps(fx.fake, "Redeploy"); n != 0 {
		t.Errorf("redeploys = %d, want none: both places are switched off", n)
	}
	if _, ok := rep.Done[a.ID]; !ok || rep.State != "finished" {
		t.Errorf("report = %+v", rep)
	}
}

func TestExecuteCompileErrorFails(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	id := mustPrepare(t, fx.runs, liveOf(t, a), nil)
	fx.runs.Compile = func(*store.Store, changeset.ChangeSet) (apply.Graph, error) {
		return apply.Graph{}, errors.New("cannot compile")
	}

	if _, err := fx.runs.Execute(context.Background(), id, nil); err == nil || !strings.Contains(err.Error(), "cannot compile") {
		t.Errorf("err = %v", err)
	}
	if s := journalOf(t, fx.dir, id).State; s != "failed" {
		t.Errorf("state = %q, want failed", s)
	}
}

func TestExecuteCancelResumeKeepsFailed(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	id := mustPrepare(t, fx.runs, liveOf(t, a), nil)
	fx.fake.FailNext("Redeploy", "boom")
	if _, err := fx.runs.Execute(context.Background(), id, nil); err == nil {
		t.Fatal("no error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fx.runs.Execute(ctx, id, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if s := journalOf(t, fx.dir, id).State; s != "failed" {
		t.Errorf("state = %q, want failed", s)
	}
}

func TestPrepareWaitsForOtherInstancesLock(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	lock, err := workdir.AcquireLock(filepath.Join(fx.dir, "prepare.lock"))
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(released)
		lock.Release()
	}()
	id := mustPrepare(t, fx.runs, live, nil)
	select {
	case <-released:
	default:
		t.Error("Prepare did not wait for the held prepare lock")
	}
	if id == "" {
		t.Error("no run")
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "prepare.lock")); !os.IsNotExist(err) {
		t.Errorf("prepare.lock left behind: %v", err)
	}
}

func TestExecuteStaleAddedStepNotJudged(t *testing.T) {
	fx := newFixture(t)
	freshReferrer(fx)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	live.ToggleRedeploy(place("api").Key())
	id := mustPrepare(t, fx.runs, live, nil)
	fx.fake.FailNext("Redeploy", "boom")
	if _, err := fx.runs.Execute(context.Background(), id, nil); err == nil {
		t.Fatal("no error")
	}

	// web no longer references api.PLAIN: the rebuild drops its redeploy.
	fx.fake.SetVar("e1", "web", "REF", "plain", false)
	rep, err := fx.runs.Execute(context.Background(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.State != "finished" || journalOf(t, fx.dir, id).State != "finished" {
		t.Errorf("state = %q, want finished", rep.State)
	}
	if _, ok := rep.Done[a.ID]; !ok {
		t.Errorf("report = %+v", rep)
	}
	if got := journalOf(t, fx.dir, id).Step("redeploy:e1/web").Status; got != "failed" {
		t.Errorf("stale step = %q, still failed in the journal", got)
	}
}

// redeployed lists the services redeployed, in order.
func redeployed(f *railwaytest.Fake) []string {
	var out []string
	for _, m := range f.Mutations() {
		if m.Op == "Redeploy" {
			out = append(out, m.Vars["serviceId"].(string))
		}
	}
	return out
}

// A resume whose store no longer knows a derived redeploy still runs it:
// the actions compiled at Prepare are frozen in actions.json.
func TestExecuteResumeKeepsFrozenRedeploys(t *testing.T) {
	fx := newFixture(t)
	vars := append(fixtureVars(), railway.VarRecord{EnvID: "e1", ServiceID: "web", Name: "REF", Unrendered: "${{db.URL}}"})
	fx.st.ReplaceVariables("p1", vars)
	fx.fake.SetVar("e1", "web", "REF", "${{db.URL}}", false)
	a := fx.set(t, nil, "db", "URL", "pg-two")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)

	path := filepath.Join(fx.dir, "runs", id, "actions.json")
	if raw, err := os.ReadFile(path); err != nil {
		t.Errorf("actions.json: %v", err)
	} else {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("actions.json mode = %v, want 0600", fi.Mode().Perm())
		}
		if strings.Contains(string(raw), "pg-") || !strings.Contains(string(raw), "redeploy:e1/web") {
			t.Errorf("actions.json = %s", raw)
		}
	}

	fx.fake.FailNext("Redeploy", "boom")
	fx.fake.FailNext("Redeploy", "boom")
	if _, err := fx.runs.Execute(context.Background(), id, nil); err == nil {
		t.Fatal("first run: no error")
	}
	if j := journalOf(t, fx.dir, id); j.Step("redeploy:e1/web").Status != "failed" || j.Step("redeploy:e1/db").Status != "failed" {
		t.Fatalf("redeploys = %+v", j.Steps)
	}

	// Resume against an empty store: it no longer knows web reads db.URL.
	empty := store.New()
	reg := fx.runs.Reg
	fx.runs.Compile = func(_ *store.Store, cs changeset.ChangeSet) (apply.Graph, error) {
		return apply.Compile(cs, empty, reg,
			func(envID string, changes []changeset.Change) apply.Step {
				return kinds.NewPatchStep(envID, changes, reg, "p1")
			},
			kinds.NewRedeployStep)
	}
	before := len(redeployed(fx.fake))
	rep, err := fx.runs.Execute(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	got := redeployed(fx.fake)[before:]
	slices.Sort(got)
	if !slices.Equal(got, []string{"db", "web"}) {
		t.Errorf("resume redeployed %v, want db and web", got)
	}
	if _, ok := rep.Done[a.ID]; !ok || rep.State != "finished" {
		t.Errorf("report = %+v, want done and finished", rep)
	}
}

// An invalid change (unknown kind, data its kind refuses) is never frozen
// into a run: Prepare(nil) leaves it out, and naming it is refused.
func TestPrepareSkipsAndRefusesInvalid(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	live.Changes = append(live.Changes,
		changeset.Change{ID: "bad-kind", Kind: "service.teleport", Place: place("web"), Target: "teleport", Created: now},
		changeset.Change{ID: "bad-limits", Kind: "limits.set", Place: place("db"), Target: "limits", New: []byte(`{"cpu":2}`), Created: now},
	)

	_, _, err := fx.runs.Prepare(live, []string{a.ID, "bad-limits"})
	if err == nil || !strings.Contains(err.Error(), "limits is invalid: limits.set: bad change data") {
		t.Fatalf("Prepare naming an invalid change: err = %v", err)
	}
	if runs, _ := fx.runs.List(); len(runs) != 0 {
		t.Fatalf("a refused Prepare wrote a run: %+v", runs)
	}

	id := mustPrepare(t, fx.runs, live, nil)
	for _, want := range []struct{ id, run string }{{a.ID, id}, {"bad-kind", ""}, {"bad-limits", ""}} {
		if got, _ := live.Get(want.id); got.RunID != want.run {
			t.Errorf("%s RunID = %q, want %q", want.id, got.RunID, want.run)
		}
	}
	if _, _, err := fx.runs.Prepare(live, nil); err == nil || err.Error() != "nothing to apply: every unlocked change is invalid" {
		t.Errorf("Prepare(nil) with only invalid changes left: err = %v", err)
	}
}

// TestCorruptFrozenRunErrorHasNoSecret: a frozen change set holds secret
// values. The decoder's error can quote part of one, so the error a corrupt
// file gives names the file and nothing of its content.
func TestCorruptFrozenRunErrorHasNoSecret(t *testing.T) {
	fx := newFixture(t)
	id := mustPrepare(t, fx.runs, liveOf(t, fx.set(t, nil, "api", "PLAIN", "two")), nil)
	dir := filepath.Join(fx.dir, "runs", id)
	for _, f := range []string{"changeset.json", "actions.json"} {
		t.Run(f, func(t *testing.T) {
			path := filepath.Join(dir, f)
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(path, orig, 0o600) })
			bad := `{"changes":[{"new":{"value":"zx\qSECRETzx"}}]}`
			if f == "actions.json" {
				bad = `[{"key":"zx\qSECRETzx"}]`
			}
			if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
				t.Fatal(err)
			}
			var errs []error
			if f == "changeset.json" {
				list, lerrs := fx.runs.List()
				unf, uerrs := fx.runs.Unfinished()
				if len(list) != 0 || len(lerrs) != 1 || len(unf) != 0 || len(uerrs) != 1 {
					t.Errorf("List = %d runs, %d errors; Unfinished = %d, %d errors; want the run skipped and reported", len(list), len(lerrs), len(unf), len(uerrs))
				}
				errs = append(errs, slices.Concat(lerrs, uerrs)...)
			}
			_, err = fx.runs.Execute(context.Background(), id, func(apply.Event) {})
			errs = append(errs, err)
			for i, err := range errs {
				if err == nil {
					t.Errorf("error %d: nil, want one", i)
					continue
				}
				for _, frag := range []string{"zx", "SECRET", `\q`, "'q'"} {
					if strings.Contains(err.Error(), frag) {
						t.Errorf("error %d holds %q: %q", i, frag, err)
					}
				}
			}
		})
	}
}

// A valid change that reads a location an invalid change writes cannot go
// without it: Prepare(nil) says so by name instead of a generic conflict.
func TestPrepareNamesDependencyOnInvalid(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "db", "NEW", "x")
	ref := fx.set(t, []changeset.Change{a}, "api", "REF", "${{db.NEW}}")
	a.New = []byte(`{"value":1}`) // invalid: its kind refuses it
	live := liveOf(t, a, ref)

	id, conflicts, err := fx.runs.Prepare(live, nil)
	if id != "" || len(conflicts) != 0 {
		t.Fatalf("id %q, conflicts %+v: want a refusal", id, conflicts)
	}
	if err == nil || err.Error() != "REF depends on an invalid change · x removes it" {
		t.Fatalf("err = %v", err)
	}
	if runs, _ := fx.runs.List(); len(runs) != 0 {
		t.Fatalf("a refused Prepare wrote a run: %+v", runs)
	}
}

// PrepareImmediate refuses an invalid change as Prepare does: it is never
// frozen into a run.
func TestPrepareImmediateRefusesInvalid(t *testing.T) {
	fx := newFixture(t)
	for _, c := range []changeset.Change{
		{Kind: "service.teleport", Place: place("web"), Target: "teleport", Created: now},
		{Kind: "limits.set", Place: place("db"), Target: "limits", New: []byte(`{"cpu":2}`), Created: now},
	} {
		id, conflicts, err := fx.runs.PrepareImmediate(c)
		if err == nil || id != "" || len(conflicts) != 0 || !strings.HasPrefix(err.Error(), c.Target+" is invalid: ") {
			t.Errorf("%s: PrepareImmediate = %q, %+v, %v; want a refusal", c.Kind, id, conflicts, err)
		}
	}
	if runs, _ := fx.runs.List(); len(runs) != 0 {
		t.Fatalf("a refused PrepareImmediate wrote a run: %+v", runs)
	}
}

// corruptRuns writes two corrupt run dirs next to the good ones: one whose
// journal does not parse and one whose frozen change set does not. Both
// files hold a secret-looking fragment the reports must not quote.
func corruptRuns(t *testing.T, dir string) {
	t.Helper()
	for name, files := range map[string]map[string]string{
		"bad-journal":   {"journal.json": `{"state":"running","steps":zxSECRETzx`, "changeset.json": `{}`},
		"bad-changeset": {"journal.json": `{"runId":"bad-changeset","state":"failed"}`, "changeset.json": `{"changes":[{"new":"zx\qSECRETzx"}]}`},
	} {
		d := filepath.Join(dir, "runs", name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for f, body := range files {
			if err := os.WriteFile(filepath.Join(d, f), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func noContent(t *testing.T, errs []error) {
	t.Helper()
	for _, err := range errs {
		for _, frag := range []string{"zx", "SECRET", `\q`, "'q'", "invalid character"} {
			if strings.Contains(err.Error(), frag) {
				t.Errorf("report holds %q: %q", frag, err)
			}
		}
	}
}

func TestListSkipsCorruptRun(t *testing.T) {
	fx := newFixture(t)
	id := mustPrepare(t, fx.runs, liveOf(t, fx.set(t, nil, "api", "PLAIN", "two")), nil)
	corruptRuns(t, fx.dir)

	list, errs := fx.runs.List()
	if len(list) != 1 || list[0].ID != id {
		t.Errorf("List = %+v, want only %s", list, id)
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %v, want one per corrupt run", errs)
	}
	noContent(t, errs)
	got := []string{errs[0].Error(), errs[1].Error()}
	slices.Sort(got)
	want := []string{"run bad-changeset: frozen change set is corrupt", "run bad-journal: journal is corrupt"}
	if !slices.Equal(got, want) {
		t.Errorf("errs = %q, want %q", got, want)
	}
	var re *apply.RunError
	if !errors.As(errs[0], &re) || re.RunID == "" {
		t.Errorf("errs[0] = %#v, want a *RunError naming the run", errs[0])
	}
}

func TestUnfinishedSkipsCorruptRun(t *testing.T) {
	fx := newFixture(t)
	id := mustPrepare(t, fx.runs, liveOf(t, fx.set(t, nil, "api", "PLAIN", "two")), nil)
	corruptRuns(t, fx.dir)

	unf, errs := fx.runs.Unfinished()
	if len(unf) != 1 || len(unf[id].Changes) != 1 {
		t.Errorf("Unfinished = %+v, want only %s", unf, id)
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %v, want one per corrupt run", errs)
	}
	noContent(t, errs)

	// A corrupt run no longer blocks Prepare.
	if _, conflicts, err := fx.runs.Prepare(liveOf(t, fx.set(t, nil, "web", "OTHER", "x")), nil); err != nil || len(conflicts) != 0 {
		t.Errorf("Prepare beside a corrupt run: %v, %v", conflicts, err)
	}
}

func TestRunInfoPatchIDs(t *testing.T) {
	fx := newFixture(t)
	id := mustPrepare(t, fx.runs, liveOf(t, fx.set(t, nil, "api", "PLAIN", "two")), nil)
	j := journalOf(t, fx.dir, id)
	for k, rec := range map[string]apply.StepRecord{
		"patch:e1":        {Status: apply.StatusDone, PatchID: "patch-1"},
		"patch:e2":        {Status: apply.StatusStarted, PatchID: "patch-2"},
		"patch:e3":        {Status: apply.StatusPending},
		"redeploy:e1/api": {Status: apply.StatusDone},
	} {
		if err := j.Set(k, rec); err != nil {
			t.Fatal(err)
		}
	}
	list, errs := fx.runs.List()
	if len(list) != 1 || len(errs) != 0 {
		t.Fatalf("List = %+v, %v", list, errs)
	}
	want := map[string]string{"patch-1": "e1", "patch-2": "e2"}
	if got := list[0].PatchIDs; !maps.Equal(got, want) {
		t.Errorf("PatchIDs = %v, want %v", got, want)
	}
}

// LockPrepare holds the prepare lock until Prepare on it runs, so the wait
// for another instance's lock can happen on another goroutine.
func TestLockPrepareThenPrepare(t *testing.T) {
	fx := newFixture(t)
	live := liveOf(t, fx.set(t, nil, "api", "PLAIN", "two"))
	pl, err := fx.runs.LockPrepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "prepare.lock")); err != nil {
		t.Fatalf("prepare lock not held: %v", err)
	}
	id, conflicts, err := pl.Prepare(live, nil)
	if err != nil || len(conflicts) != 0 || id == "" {
		t.Fatalf("Prepare = %q, %v, %v", id, conflicts, err)
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "prepare.lock")); !os.IsNotExist(err) {
		t.Errorf("prepare.lock left behind: %v", err)
	}
	// The process lock is released too: a second prepare does not hang.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if pl, err := fx.runs.LockPrepare(); err == nil {
			_ = pl.Release()
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second LockPrepare blocked")
	}
}

// An unreadable runs dir is reported with ErrRunsUnreadable, and Prepare
// refuses: it cannot check against the runs it cannot see. (Taking the
// prepare lock restores the dir's mode, so the dir turns unreadable while
// the lock is held.)
func TestUnreadableRunsDirRefusesPrepare(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 dir")
	}
	fx := newFixture(t)
	mustPrepare(t, fx.runs, liveOf(t, fx.set(t, nil, "api", "PLAIN", "two")), nil)
	runs := filepath.Join(fx.dir, "runs")
	unreadable := func() {
		t.Helper()
		if err := os.Chmod(runs, 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(runs, 0o700) })
	unreadable()

	for name, errs := range map[string][]error{
		"List":       func() []error { _, e := fx.runs.List(); return e }(),
		"Unfinished": func() []error { _, e := fx.runs.Unfinished(); return e }(),
	} {
		if len(errs) != 1 || !errors.Is(errs[0], apply.ErrRunsUnreadable) {
			t.Errorf("%s errs = %v, want ErrRunsUnreadable", name, errs)
		}
	}

	pl, err := fx.runs.LockPrepare()
	if err != nil {
		t.Fatal(err)
	}
	unreadable()
	if _, _, err := pl.Prepare(liveOf(t, fx.set(t, nil, "web", "OTHER", "x")), nil); !errors.Is(err, apply.ErrRunsUnreadable) {
		t.Errorf("Prepare err = %v, want ErrRunsUnreadable", err)
	}
	if pl, err = fx.runs.LockPrepare(); err != nil {
		t.Fatal(err)
	}
	unreadable()
	if _, _, err := pl.PrepareImmediate(fx.set(t, nil, "web", "OTHER", "y")); !errors.Is(err, apply.ErrRunsUnreadable) {
		t.Errorf("PrepareImmediate err = %v, want ErrRunsUnreadable", err)
	}
}

// runEntries lists the names under runs/.
func runEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// Check selects, checks and compiles on a copy of the live set, writes
// nothing and keeps the lock; Freeze then writes the run under the id the
// caller chose.
func TestCheckThenFreeze(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	b := fx.set(t, nil, "web", "OTHER", "two")
	live := liveOf(t, a, b)
	live.ToggleRedeploy(place("web").Key())
	snap := live.Clone()

	pl, err := fx.runs.LockPrepare()
	if err != nil {
		t.Fatal(err)
	}
	plan, conflicts, err := pl.Check(snap, nil)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("Check = %v, %v", conflicts, err)
	}
	if !slices.Equal(plan.IDs, []string{a.ID, b.ID}) {
		t.Errorf("plan ids = %v, want %s %s", plan.IDs, a.ID, b.ID)
	}
	if len(plan.Actions) == 0 {
		t.Errorf("plan has no compiled actions")
	}
	if got := runEntries(t, fx.dir); len(got) != 0 {
		t.Errorf("Check wrote runs: %v", got)
	}
	for _, c := range snap.Changes {
		if c.RunID != "" {
			t.Errorf("Check locked the snapshot: %+v", c)
		}
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "prepare.lock")); err != nil {
		t.Fatalf("Check released the prepare lock: %v", err)
	}

	id := apply.NewRunID()
	if !runIDRE.MatchString(id) {
		t.Errorf("run id %q does not match %s", id, runIDRE)
	}
	if err := pl.Freeze(plan, id); err != nil {
		t.Fatal(err)
	}
	if err := pl.Release(); err != nil {
		t.Fatal(err)
	}
	frozen, err := changeset.Load(filepath.Join(fx.dir, "runs", id, "changeset.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen.Changes) != 2 || frozen.Changes[0].ID != a.ID || frozen.Changes[1].ID != b.ID ||
		frozen.Changes[0].RunID != "" || len(frozen.RedeployOff) != 1 {
		t.Errorf("frozen = %+v", frozen)
	}
	if s := journalOf(t, fx.dir, id).State; s != apply.RunRunning {
		t.Errorf("journal state = %q", s)
	}
	if un, _ := fx.runs.Unfinished(); len(un) != 1 {
		t.Errorf("unfinished = %v", un)
	}
}

// Check refuses a selection that needs an unselected change, and one that
// conflicts with an unfinished run, writing nothing.
func TestCheckConflicts(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	mustPrepare(t, fx.runs, live, nil)
	before := runEntries(t, fx.dir)

	same := fx.set(t, nil, "api", "PLAIN", "three")
	pl, err := fx.runs.LockPrepare()
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Release()
	_, conflicts, err := pl.Check(*liveOf(t, same), nil)
	if err != nil || len(conflicts) != 1 || conflicts[0].OtherRun == "" {
		t.Errorf("Check vs a run = %+v, %v", conflicts, err)
	}
	if got := runEntries(t, fx.dir); !slices.Equal(got, before) {
		t.Errorf("runs = %v, want %v", got, before)
	}
}

// Freeze never writes into an existing run's directory: the id is taken.
func TestFreezeRefusesTakenRunID(t *testing.T) {
	fx := newFixture(t)
	id := mustPrepare(t, fx.runs, liveOf(t, fx.set(t, nil, "api", "PLAIN", "two")), nil)
	frozenPath := filepath.Join(fx.dir, "runs", id, "changeset.json")
	before, err := os.ReadFile(frozenPath)
	if err != nil {
		t.Fatal(err)
	}

	pl, err := fx.runs.LockPrepare()
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Release()
	plan, _, err := pl.Check(*liveOf(t, fx.set(t, nil, "web", "OTHER", "x")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pl.Freeze(plan, id); !errors.Is(err, os.ErrExist) {
		t.Errorf("Freeze on a taken id = %v, want ErrExist", err)
	}
	after, err := os.ReadFile(frozenPath)
	if err != nil || string(after) != string(before) {
		t.Errorf("existing run touched: %v", err)
	}
	if s := journalOf(t, fx.dir, id).State; s != apply.RunRunning {
		t.Errorf("existing run state = %q", s)
	}
	if err := pl.Freeze(plan, "../escape"); err == nil {
		t.Errorf("Freeze accepted an invalid run id")
	}
}

// Check loads the snapshot once, compiles the selection against it and runs
// Vet with it; Vet's error refuses before anything is frozen.
func TestCheckVet(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	live := liveOf(t, a)
	loads := 0
	fx.runs.Snapshot = func() *store.Store { loads++; return fx.st }
	compile := fx.runs.Compile
	var compiledOn *store.Store
	fx.runs.Compile = func(s *store.Store, cs changeset.ChangeSet) (apply.Graph, error) {
		compiledOn = s
		return compile(s, cs)
	}
	var got changeset.ChangeSet
	var vetOn *store.Store
	var acts []apply.FrozenAction
	fx.runs.Vet = func(s *store.Store, sel, _ changeset.ChangeSet, actions []apply.FrozenAction) error {
		vetOn, got, acts = s, sel, actions
		return errors.New("dev · api is read only · Settings w or k keeps it")
	}
	runID, conflicts, err := fx.runs.Prepare(live, nil)
	if err == nil || err.Error() != "dev · api is read only · Settings w or k keeps it" || runID != "" || len(conflicts) != 0 {
		t.Fatalf("Prepare = %q, %v, %v", runID, conflicts, err)
	}
	if loads != 1 || compiledOn != fx.st || vetOn != fx.st {
		t.Errorf("snapshot loaded %d times; compiled on %p, vetted on %p, want %p", loads, compiledOn, vetOn, fx.st)
	}
	if len(got.Changes) != 1 || got.Changes[0].ID != a.ID || len(acts) == 0 {
		t.Errorf("Vet saw %d changes, %d actions", len(got.Changes), len(acts))
	}
	if entries := runEntries(t, fx.dir); len(entries) != 0 {
		t.Errorf("a vetted selection was frozen: %v", entries)
	}
	if live.Changes[0].RunID != "" {
		t.Error("a vetted selection was locked")
	}

	// A selection that does not compile: Vet is asked with no actions, and
	// its reason wins.
	fx.runs.Compile = func(*store.Store, changeset.ChangeSet) (apply.Graph, error) {
		return apply.Graph{}, errors.New("requires a change outside this run")
	}
	acts = []apply.FrozenAction{{}}
	fx.runs.Vet = func(_ *store.Store, _, _ changeset.ChangeSet, actions []apply.FrozenAction) error {
		acts = actions
		return errors.New("follows a removed password change")
	}
	if _, _, err := fx.runs.Prepare(live, nil); err == nil || err.Error() != "follows a removed password change" || acts != nil {
		t.Errorf("uncompiled: %v, actions %v", err, acts)
	}
	fx.runs.Vet = func(*store.Store, changeset.ChangeSet, changeset.ChangeSet, []apply.FrozenAction) error { return nil }
	if _, _, err := fx.runs.Prepare(live, nil); err == nil || !strings.Contains(err.Error(), "outside this run") {
		t.Errorf("compile error lost: %v", err)
	}

	fx.runs.Compile = compile
	if _, _, err := fx.runs.Prepare(live, nil); err != nil {
		t.Errorf("Prepare after vet passes: %v", err)
	}
}

// PrepareImmediate is vetted too.
func TestPrepareImmediateVet(t *testing.T) {
	fx := newFixture(t)
	fx.runs.Vet = func(*store.Store, changeset.ChangeSet, changeset.ChangeSet, []apply.FrozenAction) error {
		return errors.New("refused")
	}
	if id, _, err := fx.runs.PrepareImmediate(kinds.Redeploy(place("api"), now)); err == nil || id != "" {
		t.Errorf("PrepareImmediate = %q, %v", id, err)
	}
	if entries := runEntries(t, fx.dir); len(entries) != 0 {
		t.Errorf("frozen: %v", entries)
	}
}

// List carries what the stalled banner and revert read off the UI
// goroutine: the frozen changes without values (with the Fingerprint
// hook's digest), the frozen actions, the outcomes, and whether another
// live process executes the run.
func TestListFrozenSummary(t *testing.T) {
	fx := newFixture(t)
	a := fx.set(t, nil, "api", "PLAIN", "two")
	a.Group = "g1"
	fx.runs.Fingerprint = func(c changeset.Change) string {
		if c.ID == a.ID {
			return "digest"
		}
		return ""
	}
	id := mustPrepare(t, fx.runs, liveOf(t, a), nil)
	if err := journalOf(t, fx.dir, id).SetOutcome(a.ID, apply.Outcome{Status: "done", Reason: "repaired: node 2"}); err != nil {
		t.Fatal(err)
	}
	list, errs := fx.runs.List()
	if len(errs) != 0 || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, errs)
	}
	in := list[0]
	want := apply.FrozenChange{ID: a.ID, Kind: a.Kind, Group: "g1", Target: "PLAIN", Place: place("api"), Fingerprint: "digest",
		Overlay: apply.Overlay{Short: "→ 3 chars", Tone: "new"}}
	if len(in.Frozen) != 1 || in.Frozen[0] != want {
		t.Errorf("Frozen = %+v, want %+v", in.Frozen, want)
	}
	if !slices.ContainsFunc(in.Actions, func(fa apply.FrozenAction) bool {
		return fa.Key == "redeploy:"+place("api").Key() && slices.Contains(fa.Serves, a.ID)
	}) {
		t.Errorf("Actions = %+v, want the api redeploy serving the change", in.Actions)
	}
	if in.Outcomes[a.ID].Reason != "repaired: node 2" {
		t.Errorf("Outcomes = %+v", in.Outcomes)
	}
	if in.Elsewhere {
		t.Errorf("an unlocked run reads as executing elsewhere")
	}

	// A lock held by another live process: executing elsewhere. Our own
	// pid's lock is not elsewhere.
	lock := filepath.Join(fx.dir, "runs", id, "lock")
	for _, c := range []struct {
		pid  int
		want bool
	}{{os.Getppid(), true}, {os.Getpid(), false}} {
		if err := os.WriteFile(lock, []byte(strconv.Itoa(c.pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		list, _ = fx.runs.List()
		if list[0].Elsewhere != c.want || c.want && list[0].HolderPID != c.pid {
			t.Errorf("lock of pid %d: Elsewhere = %v (pid %d), want %v", c.pid, list[0].Elsewhere, list[0].HolderPID, c.want)
		}
	}
	// a failed run is not executing anywhere, whatever its lock says
	if err := journalOf(t, fx.dir, id).SetState(apply.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if list, _ = fx.runs.List(); list[0].Elsewhere {
		t.Errorf("failed run reads as executing elsewhere")
	}

	// Frozen returns the run's frozen set, values included, for the root's
	// revert (read off the UI goroutine).
	cs, err := fx.runs.Frozen(id)
	if err != nil || len(cs.Changes) != 1 || cs.Changes[0].ID != a.ID {
		t.Errorf("Frozen = %+v, %v", cs, err)
	}
	if _, err := fx.runs.Frozen("../x"); err == nil {
		t.Errorf("Frozen of a bad id: no error")
	}
}

// frozenHolds reports whether a run's frozen change set holds value.
func frozenHolds(t *testing.T, dir, runID, value string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "runs", runID, "changeset.json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), value)
}

// A finished run keeps no value on disk; its fingerprints stay for List.
func TestFinishedRunScrubbed(t *testing.T) {
	fx := newFixture(t)
	fx.runs.Fingerprint = func(c changeset.Change) string { return "fp-" + c.Target }
	a := fx.set(t, nil, "api", "PLAIN", "fixture-secret-value")
	live := liveOf(t, a)
	id := mustPrepare(t, fx.runs, live, nil)
	if !frozenHolds(t, fx.dir, id, "fixture-secret-value") {
		t.Fatal("a prepared run lacks its value")
	}
	if _, err := fx.runs.Execute(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	if frozenHolds(t, fx.dir, id, "fixture-secret-value") {
		t.Error("a finished run keeps its value")
	}
	fx.runs.Fingerprint = func(changeset.Change) string { return "" } // as for a scrubbed change
	runs, errs := fx.runs.List()
	if len(errs) != 0 || len(runs) != 1 || runs[0].Frozen[0].Fingerprint != "fp-PLAIN" {
		t.Errorf("List = %+v, %v", runs, errs)
	}
	if o := runs[0].Frozen[0].Overlay; o.Short != "→ 20 chars" {
		t.Errorf("overlay after scrubbing = %+v, want → 20 chars", o)
	}
}

// An abandoned run keeps only what KeepAbandoned names.
func TestAbandonedRunScrubbed(t *testing.T) {
	fx := newFixture(t)
	fx.runs.KeepAbandoned = func(c changeset.Change) bool { return c.Target == "OTHER" }
	a := fx.set(t, nil, "api", "PLAIN", "fixture-dropped-value")
	b := fx.set(t, nil, "web", "OTHER", "fixture-kept-value")
	live := liveOf(t, a, b)
	id := mustPrepare(t, fx.runs, live, nil)
	if err := fx.runs.Abandon(id); err != nil {
		t.Fatal(err)
	}
	if frozenHolds(t, fx.dir, id, "fixture-dropped-value") || !frozenHolds(t, fx.dir, id, "fixture-kept-value") {
		t.Error("abandon scrubbed the wrong values")
	}
}

// Prune removes old finished and abandoned runs, never a failed one.
// A run that stopped between its final state and its scrub (a crash, a
// failed write) still holds its values: Prune scrubs it on the next start,
// long before it is old enough to go.
func TestPruneScrubsLeftValues(t *testing.T) {
	fx := newFixture(t)
	fx.runs.KeepAbandoned = func(c changeset.Change) bool { return c.Target == "KEPT" }
	finished := mustPrepare(t, fx.runs, liveOf(t, fx.set(t, nil, "api", "PLAIN", "fixture-left-value")), nil)
	abandoned := mustPrepare(t, fx.runs, liveOf(t, fx.set(t, nil, "web", "KEPT", "fixture-kept-value"), fx.set(t, nil, "web", "GONE", "fixture-gone-value")), nil)
	for id, state := range map[string]string{finished: apply.RunFinished, abandoned: apply.RunAbandoned} {
		if err := journalOf(t, fx.dir, id).SetState(state); err != nil { // as if the scrub never ran
			t.Fatal(err)
		}
	}
	if _, errs := fx.runs.Prune(30*24*time.Hour, time.Now()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if frozenHolds(t, fx.dir, finished, "fixture-left-value") || frozenHolds(t, fx.dir, abandoned, "fixture-gone-value") {
		t.Error("a finished or abandoned run keeps a value after Prune")
	}
	if !frozenHolds(t, fx.dir, abandoned, "fixture-kept-value") {
		t.Error("Prune scrubbed a value KeepAbandoned keeps")
	}
}

func TestPrune(t *testing.T) {
	fx := newFixture(t)
	runOf := func(name string) string {
		c := fx.set(t, nil, "api", name, "v-"+name)
		return mustPrepare(t, fx.runs, liveOf(t, c), nil)
	}
	finished, abandoned, failed := runOf("A_ONE"), runOf("B_TWO"), runOf("C_THREE")
	if _, err := fx.runs.Execute(context.Background(), finished, nil); err != nil {
		t.Fatal(err)
	}
	if err := fx.runs.Abandon(abandoned); err != nil {
		t.Fatal(err)
	}
	if err := journalOf(t, fx.dir, failed).SetState(apply.RunFailed); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if n, errs := fx.runs.Prune(30*24*time.Hour, now); n != 0 || len(errs) != 0 {
		t.Errorf("fresh runs pruned: %d %v", n, errs)
	}
	if n, errs := fx.runs.Prune(30*24*time.Hour, now.Add(31*24*time.Hour)); n != 2 || len(errs) != 0 {
		t.Errorf("Prune = %d, %v", n, errs)
	}
	for id, want := range map[string]bool{finished: false, abandoned: false, failed: true} {
		if _, err := os.Stat(filepath.Join(fx.dir, "runs", id)); (err == nil) != want {
			t.Errorf("run %s present = %v, want %v", id, err == nil, want)
		}
	}
}

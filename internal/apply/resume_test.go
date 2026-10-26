package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

// hooked gives a compiled step a Run of the test's own.
type hooked struct {
	Step
	run func() error
}

func (h hooked) Unwrap() Step                                          { return h.Step }
func (h hooked) Run(context.Context, *Env, StepRecord) (Result, error) { return Result{}, h.run() }

// resumeFixture is Runs over the compile fixture's fake kinds; every step
// logs its key and fails while fail holds it.
type resumeFixture struct {
	f    *fixture
	runs *Runs
	mu   sync.Mutex
	log  []string
	fail map[string]bool
	// noWait builds redeploys without the flag (a later compile).
	noWait bool
}

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	rf := &resumeFixture{f: newFixture(), fail: map[string]bool{}}
	hook := func(st Step) Step {
		k := st.Key()
		return hooked{Step: st, run: func() error {
			rf.mu.Lock()
			defer rf.mu.Unlock()
			rf.log = append(rf.log, k)
			if rf.fail[k] {
				return errors.New("boom")
			}
			return nil
		}}
	}
	newRedeploy := func(p domain.Place, serves, deps []string, wait bool) Step {
		return hook(&fakeStep{key: "redeploy:" + p.Key(), serves: serves, deps: deps, wait: wait})
	}
	rf.runs = &Runs{
		Dir:  t.TempDir(),
		Reg:  rf.f.reg,
		Exec: &Executor{},
		NewEnv: func(string, *Journal) *Env {
			return &Env{}
		},
		NewRedeploy: newRedeploy,
		Compile: func(_ *store.Store, cs changeset.ChangeSet) (Graph, error) {
			g, err := Compile(cs, store.New(), rf.f.reg,
				func(envID string, cs []changeset.Change) Step {
					var ids []string
					for _, c := range cs {
						ids = append(ids, c.ID)
					}
					return &fakeStep{key: "patch:" + envID, serves: ids}
				},
				func(p domain.Place, serves, deps []string, wait bool) Step {
					return newRedeploy(p, serves, deps, wait && !rf.noWait)
				})
			if err != nil {
				return g, err
			}
			for k, st := range g.Steps {
				if _, ok := st.(hooked); !ok {
					g.Steps[k] = hook(st)
				}
			}
			return g, nil
		},
	}
	return rf
}

func (rf *resumeFixture) prepare(t *testing.T, cs changeset.ChangeSet) string {
	t.Helper()
	live := &changeset.ChangeSet{}
	for _, c := range cs.Changes {
		if err := live.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	id, conflicts, err := rf.runs.Prepare(live, nil)
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("Prepare: %v, conflicts %v", err, conflicts)
	}
	return id
}

func (rf *resumeFixture) ran(k string) int {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	n := 0
	for _, x := range rf.log {
		if x == k {
			n++
		}
	}
	return n
}

func TestReopenSurvivesResume(t *testing.T) {
	rf := newResumeFixture(t)
	id := rf.prepare(t, passwordSet("rotate-db:m:root"))

	path := filepath.Join(rf.runs.Dir, "runs", id, "reopen.json")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("reopen.json: %v %v", fi, err)
	}

	rf.fail["patch:e2"] = true
	if _, err := rf.runs.Execute(context.Background(), id, nil); err == nil {
		t.Fatal("first run: no error")
	}
	if n := rf.ran("own:D"); n != 1 {
		t.Fatalf("own:D ran %d times, want 1", n)
	}

	// A later compile no longer knows the kind reopens: the frozen list
	// still does.
	rf.f.reg["dbpw"].(*fakeKind).reopen = false
	rf.fail["patch:e2"] = false
	rep, err := rf.runs.Execute(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := rf.ran("own:D"); n != 2 {
		t.Errorf("own:D ran %d times, want 2 (reopened on resume)", n)
	}
	if n := rf.ran("patch:e1"); n != 1 {
		t.Errorf("patch:e1 ran %d times, want 1", n)
	}
	if rep.State != RunFinished {
		t.Errorf("state = %s", rep.State)
	}
}

func TestRedeployWaitFrozen(t *testing.T) {
	rf := newResumeFixture(t)
	id := rf.prepare(t, passwordSet("rotate-db:m:root"))

	raw, err := os.ReadFile(filepath.Join(rf.runs.Dir, "runs", id, "actions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"wait":true`) {
		t.Errorf("actions.json = %s, want the wait flag", raw)
	}

	rf.fail["redeploy:e1/s1"] = true
	if _, err := rf.runs.Execute(context.Background(), id, nil); err == nil {
		t.Fatal("first run: no error")
	}

	// The resume compiles redeploys without the flag; the frozen one wins.
	rf.noWait = true
	rf.fail["redeploy:e1/s1"] = false
	g, err := rf.runs.Compile(nil, passwordSet("rotate-db:m:root"))
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := loadActions(filepath.Join(rf.runs.Dir, "runs", id))
	if err != nil {
		t.Fatal(err)
	}
	g, err = rf.runs.restoreActions(g, passwordSet("rotate-db:m:root"), frozen)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"redeploy:e1/s1", "redeploy:e2/s1"} {
		if !waits(g.Steps[k]) {
			t.Errorf("restored %s does not wait for SUCCESS", k)
		}
	}
	if _, err := rf.runs.Execute(context.Background(), id, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := rf.ran("redeploy:e1/s1"); n != 2 {
		t.Errorf("redeploy:e1/s1 ran %d times, want 2", n)
	}
}

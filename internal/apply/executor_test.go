package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// script is a scripted fake step. run, when set, decides the result;
// otherwise the step succeeds with res.
type script struct {
	key, lock string
	deps      []string
	res       Result
	err       error
	run       func(ctx context.Context, prior StepRecord) (Result, error)
	log       *callLog
}

func (s *script) Key() string      { return s.key }
func (s *script) Deps() []string   { return s.deps }
func (s *script) Serves() []string { return nil }
func (s *script) Lock() string     { return s.lock }
func (s *script) Label() string    { return s.key }
func (s *script) Run(ctx context.Context, _ *Env, prior StepRecord) (Result, error) {
	if s.log != nil {
		s.log.add(s.key)
	}
	if s.run != nil {
		return s.run(ctx, prior)
	}
	return s.res, s.err
}

type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(k string) {
	l.mu.Lock()
	l.calls = append(l.calls, k)
	l.mu.Unlock()
}

func (l *callLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

type stopErr struct{}

func (stopErr) Error() string { return "drifted after touch" }
func (stopErr) StopRun() bool { return true }

func graphOf(t *testing.T, steps ...Step) Graph {
	t.Helper()
	g := Graph{Steps: map[string]Step{}}
	for _, s := range steps {
		g.Steps[s.Key()] = s
	}
	order, err := topoOrder(g.Steps)
	if err != nil {
		t.Fatal(err)
	}
	g.Order = order
	return g
}

func newEnv(t *testing.T) *Env {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runs", "r1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Env{Journal: j, RunID: "r1"}
}

func wantStatus(t *testing.T, env *Env, want map[string]string) {
	t.Helper()
	for k, st := range want {
		if got := env.Journal.Step(k).Status; got != st {
			t.Errorf("step %s status = %q, want %q", k, got, st)
		}
	}
}

// events collects events safely.
type events struct {
	mu  sync.Mutex
	all []Event
}

func (e *events) on(ev Event) {
	e.mu.Lock()
	e.all = append(e.all, ev)
	e.mu.Unlock()
}

func (e *events) has(key, status string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.all {
		if ev.StepKey == key && ev.Status == status {
			return true
		}
	}
	return false
}

func TestExecutorOrder(t *testing.T) {
	log := &callLog{}
	env := newEnv(t)
	g := graphOf(t,
		&script{key: "c", deps: []string{"b"}, log: log},
		&script{key: "a", log: log, res: Result{Outcomes: map[string]Outcome{"ch1": {Status: StatusDone}}}},
		&script{key: "b", deps: []string{"a"}, log: log},
	)
	ev := &events{}
	x := &Executor{Concurrency: 6}
	if err := x.Run(context.Background(), g, env, ev.on); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("calls = %v", got)
	}
	wantStatus(t, env, map[string]string{"a": StatusDone, "b": StatusDone, "c": StatusDone})
	if o := env.Journal.Outcome("ch1"); o.Status != StatusDone {
		t.Errorf("outcome ch1 = %+v", o)
	}
	for _, k := range []string{"a", "b", "c"} {
		if !ev.has(k, StatusStarted) || !ev.has(k, StatusDone) {
			t.Errorf("events for %s missing: %+v", k, ev.all)
		}
	}
	if ev.all[0].RunID != "r1" {
		t.Errorf("event run id = %q", ev.all[0].RunID)
	}
}

func TestExecutorParallelBounded(t *testing.T) {
	env := newEnv(t)
	var running, peak atomic.Int32
	entered := make(chan string, 10)
	release := make(chan struct{})
	var steps []Step
	for i := range 10 {
		k := string(rune('a' + i))
		steps = append(steps, &script{key: k, run: func(context.Context, StepRecord) (Result, error) {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			entered <- k
			<-release
			running.Add(-1)
			return Result{}, nil
		}})
	}
	x := &Executor{} // zero Concurrency means the default, 6
	done := make(chan error, 1)
	go func() { done <- x.Run(context.Background(), graphOf(t, steps...), env, nil) }()
	for range 6 {
		<-entered
	}
	select {
	case k := <-entered:
		t.Errorf("step %s started beyond the limit", k)
	case <-time.After(5 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p != 6 {
		t.Errorf("peak concurrency = %d, want 6", p)
	}
	for _, s := range steps {
		wantStatus(t, env, map[string]string{s.Key(): StatusDone})
	}
}

func TestExecutorFailedBlocksDependents(t *testing.T) {
	log := &callLog{}
	env := newEnv(t)
	ev := &events{}
	g := graphOf(t,
		&script{key: "a", log: log},
		&script{key: "b", deps: []string{"a"}, err: errors.New("boom"), log: log},
		&script{key: "c", deps: []string{"b"}, log: log},
		&script{key: "e", deps: []string{"c"}, log: log},
		&script{key: "d", log: log},
	)
	err := (&Executor{Concurrency: 6}).Run(context.Background(), g, env, ev.on)
	if err == nil || !strings.Contains(err.Error(), "b") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want one naming b", err)
	}
	if errors.Is(err, ErrStopped) {
		t.Error("an ordinary failure is not ErrStopped")
	}
	wantStatus(t, env, map[string]string{"a": StatusDone, "b": StatusFailed, "c": StatusBlocked, "e": StatusBlocked, "d": StatusDone})
	if rec := env.Journal.Step("b"); rec.Error != "boom" {
		t.Errorf("b error = %q", rec.Error)
	}
	if slices.Contains(log.get(), "c") || slices.Contains(log.get(), "e") {
		t.Errorf("blocked steps ran: %v", log.get())
	}
	if !ev.has("b", StatusFailed) || !ev.has("c", StatusBlocked) || !ev.has("e", StatusBlocked) {
		t.Errorf("events = %+v", ev.all)
	}
}

func TestExecutorResumeSkipsDone(t *testing.T) {
	log := &callLog{}
	env := newEnv(t)
	for k, st := range map[string]string{"a": StatusDone, "b": StatusFailed, "c": StatusBlocked} {
		if err := env.Journal.Set(k, StepRecord{Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	g := graphOf(t,
		&script{key: "a", log: log},
		&script{key: "b", deps: []string{"a"}, log: log},
		&script{key: "c", deps: []string{"b"}, log: log},
	)
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"b", "c"}) {
		t.Errorf("calls = %v, want [b c]", got)
	}
	wantStatus(t, env, map[string]string{"a": StatusDone, "b": StatusDone, "c": StatusDone})
}

func TestExecutorCancelThenResume(t *testing.T) {
	log := &callLog{}
	env := newEnv(t)
	entered := make(chan struct{})
	var priors []string
	var mu sync.Mutex
	first := true
	b := &script{key: "b", deps: []string{"a"}, log: log, run: func(ctx context.Context, prior StepRecord) (Result, error) {
		mu.Lock()
		priors = append(priors, prior.Status)
		wait := first
		first = false
		mu.Unlock()
		if wait {
			close(entered)
			<-ctx.Done()
			return Result{}, ctx.Err()
		}
		return Result{}, nil
	}}
	g := graphOf(t, &script{key: "a", log: log}, b, &script{key: "c", deps: []string{"b"}, log: log})
	x := &Executor{Concurrency: 6}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- x.Run(ctx, g, env, nil) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	wantStatus(t, env, map[string]string{"a": StatusDone, "b": StatusStarted, "c": StatusPending})

	if err := x.Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"a", "b", "b", "c"}) {
		t.Errorf("calls = %v, want [a b b c]", got)
	}
	if !slices.Equal(priors, []string{StatusPending, StatusStarted}) {
		t.Errorf("b saw priors %v, want [pending started]", priors)
	}
	wantStatus(t, env, map[string]string{"a": StatusDone, "b": StatusDone, "c": StatusDone})
}

func TestResumeTrustsSealedDone(t *testing.T) {
	env := newEnv(t)
	if err := env.Journal.Set("patch:e1", StepRecord{Status: StatusDone, PatchID: "p1"}); err != nil {
		t.Fatal(err)
	}
	ran := false
	patch := &script{key: "patch:e1", run: func(context.Context, StepRecord) (Result, error) {
		ran = true // a sealed value reads back empty: the step would wrongly redo it
		return Result{}, errors.New("sealed value reads empty")
	}}
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), graphOf(t, patch), env, nil); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("a done patch step ran again")
	}
	if rec := env.Journal.Step("patch:e1"); rec.Status != StatusDone || rec.PatchID != "p1" {
		t.Errorf("record = %+v", rec)
	}
}

func TestExecutorStopRun(t *testing.T) {
	log := &callLog{}
	env := newEnv(t)
	dEntered := make(chan struct{})
	aFailed := make(chan struct{})
	var once sync.Once
	onEvent := func(ev Event) {
		if ev.StepKey == "a" && ev.Status == StatusFailed {
			once.Do(func() { close(aFailed) })
		}
	}
	g := graphOf(t,
		&script{key: "a", log: log, run: func(context.Context, StepRecord) (Result, error) {
			<-dEntered
			return Result{}, &wrapped{stopErr{}}
		}},
		&script{key: "b", deps: []string{"a"}, log: log},
		&script{key: "d", log: log, run: func(context.Context, StepRecord) (Result, error) {
			close(dEntered)
			<-aFailed // still running when the run stops
			return Result{}, nil
		}},
		&script{key: "e", deps: []string{"d"}, log: log},
	)
	err := (&Executor{Concurrency: 6}).Run(context.Background(), g, env, onEvent)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
	var se stopErr
	if !errors.As(err, &se) {
		t.Errorf("ErrStopped should wrap the step error: %v", err)
	}
	if !strings.Contains(err.Error(), "a") {
		t.Errorf("err = %v, want it to name a", err)
	}
	wantStatus(t, env, map[string]string{"a": StatusFailed, "b": StatusPending, "d": StatusDone, "e": StatusPending})
	for _, k := range []string{"b", "e"} {
		if slices.Contains(log.get(), k) {
			t.Errorf("%s ran after the stop", k)
		}
	}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "step: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

func TestIsStopRun(t *testing.T) {
	if !IsStopRun(&wrapped{stopErr{}}) || IsStopRun(errors.New("x")) || IsStopRun(nil) {
		t.Error("IsStopRun")
	}
}

func TestExecutorPerPlaceLock(t *testing.T) {
	x := &Executor{Concurrency: 6}
	var inside, overlaps atomic.Int32
	aEntered := make(chan struct{})
	release := make(chan struct{})
	enter := func() {
		if inside.Add(1) > 1 {
			overlaps.Add(1)
		}
	}
	a := &script{key: "redeploy:s1", lock: "e1/s1", run: func(context.Context, StepRecord) (Result, error) {
		enter()
		close(aEntered)
		<-release
		inside.Add(-1)
		return Result{}, nil
	}}
	b := &script{key: "redeploy:s1", lock: "e1/s1", run: func(context.Context, StepRecord) (Result, error) {
		enter()
		inside.Add(-1)
		return Result{}, nil
	}}
	envA, envB := newEnv(t), newEnv(t)
	errs := make(chan error, 2)
	go func() { errs <- x.Run(context.Background(), graphOf(t, a), envA, nil) }()
	<-aEntered
	bStarted := make(chan struct{})
	go func() {
		errs <- x.Run(context.Background(), graphOf(t, b), envB, func(ev Event) {
			if ev.Status == StatusStarted {
				close(bStarted)
			}
		})
	}()
	select {
	case <-bStarted:
		t.Error("b started while a held the place lock")
	case <-time.After(5 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := overlaps.Load(); n != 0 {
		t.Errorf("%d overlaps", n)
	}
	wantStatus(t, envB, map[string]string{"redeploy:s1": StatusDone})
}

func TestExecutorMoreSteps(t *testing.T) {
	log := &callLog{}
	env := newEnv(t)
	x1 := &script{key: "x", log: log}
	x2 := &script{key: "x", log: log}
	again := &script{key: "b", log: log} // already in the graph
	g := graphOf(t,
		&script{key: "a", log: log, res: Result{More: []Step{x1, x2, again}}},
		&script{key: "b", deps: []string{"a"}, log: log},
	)
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	got := log.get()
	if len(got) != 3 || got[0] != "a" || !slices.Contains(got, "x") || !slices.Contains(got, "b") {
		t.Errorf("calls = %v, want a then b and x once each", got)
	}
	wantStatus(t, env, map[string]string{"a": StatusDone, "b": StatusDone, "x": StatusDone})
	if got := env.Journal.Step("a").Expanded; !slices.Equal(got, []string{"x"}) {
		t.Errorf("a.Expanded = %v, want only the newly added [x]", got)
	}
}

func TestExecutorKeepsStepFields(t *testing.T) {
	env := newEnv(t)
	st := &script{key: "patch:e1", run: func(context.Context, StepRecord) (Result, error) {
		rec := env.Journal.Step("patch:e1")
		rec.PatchID, rec.Data = "p9", []byte(`{"n":1}`)
		if err := env.Journal.Set("patch:e1", rec); err != nil {
			return Result{}, err
		}
		return Result{}, errors.New("patch failed")
	}}
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), graphOf(t, st), env, nil); err == nil {
		t.Fatal("want an error")
	}
	rec := env.Journal.Step("patch:e1")
	if rec.Status != StatusFailed || rec.PatchID != "p9" || string(rec.Data) != `{"n":1}` || rec.Error != "patch failed" {
		t.Errorf("record = %+v", rec)
	}
}

func TestExecutorStopRunDropsQueued(t *testing.T) {
	env := newEnv(t)
	log := &callLog{}
	var first atomic.Bool
	mk := func(k string) Step {
		return &script{key: k, log: log, run: func(context.Context, StepRecord) (Result, error) {
			if first.CompareAndSwap(false, true) {
				return Result{}, stopErr{}
			}
			return Result{}, nil
		}}
	}
	g := graphOf(t, mk("a"), mk("b"), mk("c"))
	err := (&Executor{Concurrency: 1}).Run(context.Background(), g, env, nil)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
	calls := log.get()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want only the stopping step", calls)
	}
	for _, k := range []string{"a", "b", "c"} {
		want := StatusPending
		if k == calls[0] {
			want = StatusFailed
		}
		wantStatus(t, env, map[string]string{k: want})
	}
}

func TestExecutorResumeRebuildsMoreSteps(t *testing.T) {
	env := newEnv(t)
	log := &callLog{}
	xFails := true
	x := &script{key: "x", log: log, run: func(context.Context, StepRecord) (Result, error) {
		if xFails {
			return Result{}, errors.New("x failed")
		}
		return Result{}, nil
	}}
	var priors []string
	p := &script{key: "p", log: log, run: func(_ context.Context, prior StepRecord) (Result, error) {
		priors = append(priors, prior.Status)
		return Result{More: []Step{x}}, nil
	}}
	g := graphOf(t, p)
	ex := &Executor{Concurrency: 6}
	if err := ex.Run(context.Background(), g, env, nil); err == nil {
		t.Fatal("want x's failure")
	}
	if rec := env.Journal.Step("p"); rec.Status != StatusDone || !slices.Equal(rec.Expanded, []string{"x"}) {
		t.Fatalf("p record = %+v", rec)
	}
	xFails = false
	if err := ex.Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"p", "x", "p", "x"}) {
		t.Errorf("calls = %v, want [p x p x]", got)
	}
	if !slices.Equal(priors, []string{StatusPending, StatusDone}) {
		t.Errorf("p saw priors %v, want [pending done]: the re-run is a rebuild", priors)
	}
	wantStatus(t, env, map[string]string{"p": StatusDone, "x": StatusDone})

	// Everything added is done now: a third Run runs nothing.
	if err := ex.Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(log.get()); n != 4 {
		t.Errorf("third Run ran steps: %v", log.get())
	}
}

func TestExecutorResumeRebuildsNestedMoreSteps(t *testing.T) {
	env := newEnv(t)
	for k, rec := range map[string]StepRecord{
		"p": {Status: StatusDone, Expanded: []string{"x"}},
		"x": {Status: StatusDone, Expanded: []string{"y"}},
		"y": {Status: StatusFailed},
	} {
		if err := env.Journal.Set(k, rec); err != nil {
			t.Fatal(err)
		}
	}
	log := &callLog{}
	y := &script{key: "y", log: log}
	x := &script{key: "x", log: log, res: Result{More: []Step{y}}}
	p := &script{key: "p", log: log, res: Result{More: []Step{x}}}
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), graphOf(t, p), env, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"p", "x", "y"}) {
		t.Errorf("calls = %v, want [p x y]", got)
	}
}

func TestExecutorMoreStepAlreadyDone(t *testing.T) {
	env := newEnv(t)
	if err := env.Journal.Set("x", StepRecord{Status: StatusDone}); err != nil {
		t.Fatal(err)
	}
	log := &callLog{}
	x := &script{key: "x", log: log}
	g := graphOf(t,
		&script{key: "p", log: log, res: Result{More: []Step{x}}},
		&script{key: "q", deps: []string{"p"}, log: log},
	)
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"p", "q"}) {
		t.Errorf("calls = %v, want [p q]", got)
	}
}

func TestExecutorMoreStepOnFailedDepIsBlocked(t *testing.T) {
	env := newEnv(t)
	log := &callLog{}
	ev := &events{}
	aFailed := make(chan struct{})
	var once sync.Once
	onEvent := func(e Event) {
		ev.on(e)
		if e.StepKey == "a" && e.Status == StatusFailed {
			once.Do(func() { close(aFailed) })
		}
	}
	x := &script{key: "x", deps: []string{"a"}, log: log}
	g := graphOf(t,
		&script{key: "a", log: log, err: errors.New("a broke")},
		&script{key: "p", log: log, run: func(context.Context, StepRecord) (Result, error) {
			<-aFailed
			return Result{More: []Step{x}}, nil
		}},
	)
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), g, env, onEvent); err == nil {
		t.Fatal("want a's failure")
	}
	if slices.Contains(log.get(), "x") {
		t.Error("x ran though a failed")
	}
	wantStatus(t, env, map[string]string{"a": StatusFailed, "p": StatusDone, "x": StatusBlocked})
	if !ev.has("x", StatusBlocked) {
		t.Errorf("no blocked event for x: %+v", ev.all)
	}
}

func TestExecutorSkippedIsFinal(t *testing.T) {
	env := newEnv(t)
	if err := env.Journal.Set("a", StepRecord{Status: StatusSkipped}); err != nil {
		t.Fatal(err)
	}
	log := &callLog{}
	g := graphOf(t, &script{key: "a", log: log}, &script{key: "b", deps: []string{"a"}, log: log})
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("calls = %v, want [b]", got)
	}
	wantStatus(t, env, map[string]string{"a": StatusSkipped, "b": StatusDone})
}

func TestExecutorOutcomesOnError(t *testing.T) {
	env := newEnv(t)
	st := &script{key: "patch:e1", res: Result{Outcomes: map[string]Outcome{
		"c1": {Status: StatusDone}, "c2": {Status: StatusSkipped, Reason: "drifted"},
	}}, err: errors.New("c3 failed")}
	if err := (&Executor{Concurrency: 6}).Run(context.Background(), graphOf(t, st), env, nil); err == nil {
		t.Fatal("want an error")
	}
	if o := env.Journal.Outcome("c1"); o.Status != StatusDone {
		t.Errorf("c1 = %+v", o)
	}
	if o := env.Journal.Outcome("c2"); o.Status != StatusSkipped || o.Reason != "drifted" {
		t.Errorf("c2 = %+v", o)
	}
}

func TestExecutorRunsShareLimit(t *testing.T) {
	x := &Executor{}
	var running, peak atomic.Int32
	entered := make(chan string, 10)
	release := make(chan struct{})
	mk := func(prefix string) Graph {
		var steps []Step
		for i := range 5 {
			k := prefix + string(rune('a'+i))
			steps = append(steps, &script{key: k, run: func(context.Context, StepRecord) (Result, error) {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				entered <- k
				<-release
				running.Add(-1)
				return Result{}, nil
			}})
		}
		return graphOf(t, steps...)
	}
	errs := make(chan error, 2)
	ga, gb := mk("a"), mk("b")
	envA, envB := newEnv(t), newEnv(t)
	go func() { errs <- x.Run(context.Background(), ga, envA, nil) }()
	go func() { errs <- x.Run(context.Background(), gb, envB, nil) }()
	for range 6 {
		<-entered
	}
	select {
	case k := <-entered:
		t.Errorf("step %s started beyond the shared limit", k)
	case <-time.After(5 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if p := peak.Load(); p != 6 {
		t.Errorf("peak = %d, want 6", p)
	}
}

func TestExecutorRebuildErrorKeepsDone(t *testing.T) {
	env := newEnv(t)
	doneRec := StepRecord{Status: StatusDone, PatchID: "p1", Data: []byte(`{"n":1}`), Expanded: []string{"x"}}
	for k, rec := range map[string]StepRecord{"p": doneRec, "x": {Status: StatusFailed}} {
		if err := env.Journal.Set(k, rec); err != nil {
			t.Fatal(err)
		}
	}
	log := &callLog{}
	x := &script{key: "x", log: log}
	rebuildFails := true
	var priors []string
	p := &script{key: "p", log: log, run: func(_ context.Context, prior StepRecord) (Result, error) {
		priors = append(priors, prior.Status)
		if rebuildFails {
			return Result{}, errors.New("re-read failed")
		}
		return Result{More: []Step{x}}, nil
	}}
	q := &script{key: "q", deps: []string{"p"}, log: log}
	g := graphOf(t, p, q)
	ev := &events{}
	ex := &Executor{Concurrency: 6}
	err := ex.Run(context.Background(), g, env, ev.on)
	if err == nil || !strings.Contains(err.Error(), "p") || !strings.Contains(err.Error(), "re-read failed") {
		t.Fatalf("err = %v, want p's rebuild error", err)
	}
	rec := env.Journal.Step("p")
	if rec.Status != StatusDone || rec.PatchID != "p1" || string(rec.Data) != `{"n":1}` || !slices.Equal(rec.Expanded, []string{"x"}) || rec.Error != "" {
		t.Errorf("p record = %+v, want the done record untouched", rec)
	}
	found := false
	for _, e := range ev.all {
		if e.StepKey == "p" && e.Error == "re-read failed" {
			found = true
		}
		if e.StepKey == "p" && (e.Status == StatusFailed || e.Status == StatusStarted) {
			t.Errorf("rebuild event %+v", e)
		}
	}
	if !found {
		t.Errorf("no event carries the rebuild error: %+v", ev.all)
	}
	wantStatus(t, env, map[string]string{"q": StatusDone, "x": StatusFailed})

	rebuildFails = false
	if err := ex.Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(priors, []string{StatusDone, StatusDone}) {
		t.Errorf("p priors = %v, want two rebuilds", priors)
	}
	if got := log.get(); !slices.Equal(got, []string{"p", "q", "p", "x"}) {
		t.Errorf("calls = %v, want [p q p x]", got)
	}
	wantStatus(t, env, map[string]string{"p": StatusDone, "x": StatusDone})
}

// reopenGraph is a db step and the patch that requires it, the patch
// listed to reopen the db step.
func reopenGraph(t *testing.T, db, patch *script) Graph {
	t.Helper()
	g := graphOf(t, db, patch)
	g.Reopen = map[string][]string{db.key: {patch.key}}
	return g
}

func TestReopenWhileListedPending(t *testing.T) {
	for _, tc := range []struct {
		patch  string
		wantDB bool
	}{
		{StatusFailed, true},
		{StatusBlocked, true},
		{StatusPending, true},
		{StatusDone, false},
		{StatusSkipped, false},
	} {
		t.Run(tc.patch, func(t *testing.T) {
			log := &callLog{}
			env := newEnv(t)
			_ = env.Journal.Set("db", StepRecord{Status: StatusDone, Acted: true})
			_ = env.Journal.Set("patch", StepRecord{Status: tc.patch})
			g := reopenGraph(t,
				&script{key: "db", log: log},
				&script{key: "patch", deps: []string{"db"}, log: log})
			if err := (&Executor{}).Run(context.Background(), g, env, nil); err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(log.get(), "db"); got != tc.wantDB {
				t.Errorf("db ran = %v, want %v (calls %v)", got, tc.wantDB, log.get())
			}
			if tc.wantDB && !slices.Equal(log.get(), []string{"db", "patch"}) {
				t.Errorf("calls = %v, want db then patch", log.get())
			}
			wantStatus(t, env, map[string]string{"db": StatusDone, "patch": map[bool]string{true: StatusDone, false: tc.patch}[tc.wantDB]})
		})
	}
}

// A listed key that is not in the graph never reopens a step.
func TestReopenIgnoresAbsentKey(t *testing.T) {
	log := &callLog{}
	env := newEnv(t)
	_ = env.Journal.Set("db", StepRecord{Status: StatusDone})
	g := graphOf(t, &script{key: "db", log: log})
	g.Reopen = map[string][]string{"db": {"gone"}}
	if err := (&Executor{}).Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if len(log.get()) != 0 {
		t.Errorf("calls = %v, want none", log.get())
	}
}

func TestReopenedStepActs(t *testing.T) {
	env := newEnv(t)
	_ = env.Journal.Set("db", StepRecord{Status: StatusDone, Data: []byte(`{"pid":7}`)})
	_ = env.Journal.Set("patch", StepRecord{Status: StatusFailed, Error: "boom"})
	var seen StepRecord
	g := reopenGraph(t,
		&script{key: "db", run: func(_ context.Context, prior StepRecord) (Result, error) {
			seen = prior
			if got := env.Journal.Step("db").Status; got != StatusStarted {
				t.Errorf("db journaled %q while running, want started", got)
			}
			return Result{}, errors.New("login refused")
		}},
		&script{key: "patch", deps: []string{"db"}})
	ev := &events{}
	if err := (&Executor{}).Run(context.Background(), g, env, ev.on); err == nil {
		t.Fatal("Run: no error")
	}
	if seen.Status != StatusPending || string(seen.Data) != `{"pid":7}` {
		t.Errorf("prior = %+v, want pending with the old data", seen)
	}
	if !ev.has("db", StatusStarted) || !ev.has("db", StatusFailed) {
		t.Errorf("events = %+v, want db started and failed", ev.all)
	}
	wantStatus(t, env, map[string]string{"db": StatusFailed, "patch": StatusBlocked})
	if e := env.Journal.Step("db").Error; e != "login refused" {
		t.Errorf("db error = %q", e)
	}
}

// A skipped record is final too: it is reopened and acts like a done one.
func TestReopenSkippedActs(t *testing.T) {
	env := newEnv(t)
	_ = env.Journal.Set("db", StepRecord{Status: StatusSkipped})
	_ = env.Journal.Set("patch", StepRecord{Status: StatusFailed})
	var prior StepRecord
	g := reopenGraph(t,
		&script{key: "db", run: func(_ context.Context, p StepRecord) (Result, error) {
			prior = p
			return Result{}, nil
		}},
		&script{key: "patch", deps: []string{"db"}})
	if err := (&Executor{}).Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if prior.Status != StatusPending {
		t.Errorf("prior = %+v, want pending", prior)
	}
	wantStatus(t, env, map[string]string{"db": StatusDone, "patch": StatusDone})
}

// Acted is set when a step's record becomes done and never cleared.
func TestActedSticky(t *testing.T) {
	env := newEnv(t)
	g := graphOf(t, &script{key: "a"}, &script{key: "b", err: errors.New("boom")})
	_ = (&Executor{}).Run(context.Background(), g, env, nil)
	if !env.Journal.Step("a").Acted || env.Journal.Step("b").Acted {
		t.Errorf("a = %+v, b = %+v: want a acted, b not", env.Journal.Step("a"), env.Journal.Step("b"))
	}

	for _, tc := range []struct {
		name       string
		preFails   bool
		dbErr      bool
		cancel     bool
		wantStatus string
	}{
		{name: "fails", dbErr: true, wantStatus: StatusFailed},
		{name: "blocked", preFails: true, wantStatus: StatusDone},
		{name: "interrupted", cancel: true, wantStatus: StatusStarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			_ = env.Journal.Set("db", StepRecord{Status: StatusDone, Acted: true, Data: []byte(`{"pid":7}`)})
			_ = env.Journal.Set("patch", StepRecord{Status: StatusFailed})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pre := &script{key: "pre"}
			if tc.preFails {
				pre.err = errors.New("pre-flight refused")
			}
			db := &script{key: "db", deps: []string{"pre"}, run: func(ctx context.Context, _ StepRecord) (Result, error) {
				switch {
				case tc.cancel:
					cancel()
					return Result{}, ctx.Err()
				case tc.dbErr:
					return Result{}, errors.New("login refused")
				}
				return Result{}, nil
			}}
			g := graphOf(t, pre, db, &script{key: "patch", deps: []string{"db"}})
			g.Reopen = map[string][]string{"db": {"patch"}}
			ev := &events{}
			_ = (&Executor{}).Run(ctx, g, env, ev.on)
			rec := env.Journal.Step("db")
			if rec.Status != tc.wantStatus || !rec.Acted || string(rec.Data) != `{"pid":7}` {
				t.Errorf("db = %+v, want %s, acted, old data", rec, tc.wantStatus)
			}
			if tc.preFails && !ev.has("db", StatusBlocked) {
				t.Errorf("events = %+v, want db blocked", ev.all)
			}
		})
	}
}

// The next Run reopens a step left done by a blocked reopen.
func TestReopenAfterBlocked(t *testing.T) {
	env := newEnv(t)
	_ = env.Journal.Set("db", StepRecord{Status: StatusDone, Acted: true})
	_ = env.Journal.Set("patch", StepRecord{Status: StatusFailed})
	log := &callLog{}
	failPre := true
	pre := &script{key: "pre", run: func(context.Context, StepRecord) (Result, error) {
		if failPre {
			return Result{}, errors.New("refused")
		}
		return Result{}, nil
	}}
	g := graphOf(t, pre, &script{key: "db", deps: []string{"pre"}, log: log}, &script{key: "patch", deps: []string{"db"}, log: log})
	g.Reopen = map[string][]string{"db": {"patch"}}
	_ = (&Executor{}).Run(context.Background(), g, env, nil)
	failPre = false
	if err := (&Executor{}).Run(context.Background(), g, env, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"db", "patch"}) {
		t.Errorf("calls = %v, want db then patch", got)
	}
}

// A step marks itself acted before its state-changing command; a failure
// after that keeps the mark, one before it leaves none.
func TestMarkActed(t *testing.T) {
	env := newEnv(t)
	g := graphOf(t,
		&script{key: "after", run: func(context.Context, StepRecord) (Result, error) {
			if err := env.MarkActed("after"); err != nil {
				t.Fatal(err)
			}
			return Result{}, errors.New("output cut")
		}},
		&script{key: "before", err: errors.New("tunnel refused")},
	)
	_ = (&Executor{}).Run(context.Background(), g, env, nil)
	if rec := env.Journal.Step("after"); rec.Status != StatusFailed || !rec.Acted {
		t.Errorf("after = %+v, want failed and acted", rec)
	}
	if rec := env.Journal.Step("before"); rec.Status != StatusFailed || rec.Acted {
		t.Errorf("before = %+v, want failed, not acted", rec)
	}
}

// No writer clears Acted: a step's own write built from prior, after it
// marked itself acted, keeps the mark.
func TestActedSurvivesWriteFromPrior(t *testing.T) {
	env := newEnv(t)
	g := graphOf(t, &script{key: "s", run: func(_ context.Context, prior StepRecord) (Result, error) {
		if err := env.MarkActed("s"); err != nil {
			t.Fatal(err)
		}
		rec := prior // Acted false
		rec.Status, rec.PatchID = StatusStarted, "p1"
		if err := env.Journal.Set("s", rec); err != nil {
			t.Fatal(err)
		}
		return Result{}, errors.New("output cut")
	}})
	_ = (&Executor{}).Run(context.Background(), g, env, nil)
	if rec := env.Journal.Step("s"); rec.Status != StatusFailed || !rec.Acted || rec.PatchID != "p1" {
		t.Errorf("s = %+v, want failed, acted, patch p1", rec)
	}
}

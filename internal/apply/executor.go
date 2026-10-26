package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Step statuses in the journal.
const (
	StatusPending = "pending"
	StatusStarted = "started"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusBlocked = "blocked"
	StatusSkipped = "skipped"
)

// DefaultConcurrency is the executor's limit when Concurrency is not set.
const DefaultConcurrency = 6

// ErrStopped means a step found drift on a target the run had already
// touched; the run stops for a human to look at. It wraps the step error.
var ErrStopped = errors.New("run stopped")

// IsStopRun reports whether err asks to stop the whole run. The concrete
// error lives with the kinds; it is recognised by its StopRun method.
func IsStopRun(err error) bool {
	var s interface{ StopRun() bool }
	return errors.As(err, &s) && s.StopRun()
}

// Event reports a step's new status to the caller of Run.
type Event struct {
	RunID, StepKey, Status, Error string
}

// Executor runs compiled graphs. One executor serves every run of the
// process: Concurrency bounds the steps running across all its runs, and
// steps with the same Lock never run at the same time.
type Executor struct {
	Concurrency int      // DefaultConcurrency when zero
	locks       sync.Map // place key → chan struct{} (capacity 1)

	semOnce sync.Once
	sem     chan struct{}
}

func (x *Executor) slots() chan struct{} {
	x.semOnce.Do(func() {
		n := x.Concurrency
		if n <= 0 {
			n = DefaultConcurrency
		}
		x.sem = make(chan struct{}, n)
	})
	return x.sem
}

// lock takes the place lock key, or gives up when ctx ends or halt closes.
func (x *Executor) lock(ctx context.Context, halt <-chan struct{}, key string) (unlock func(), ok bool) {
	v, _ := x.locks.LoadOrStore(key, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	case <-ctx.Done():
	case <-halt:
	}
	return nil, false
}

// final reports whether a step status means the step never runs again.
func final(status string) bool { return status == StatusDone || status == StatusSkipped }

// completion is what a worker hands back to the scheduler.
type completion struct {
	key string
	res Result
	err error
	// ran: the step was started and its result journaled (unless
	// interrupted); otherwise the worker gave up before starting it and
	// the step stays pending.
	ran         bool
	interrupted bool // cancelled mid-run: the journal keeps started
	// rebuild: the step was done or skipped and ran only to add its More
	// steps again; its record is never set to started or failed.
	rebuild    bool
	journalErr error // the journal could not be written
}

// run is the scheduler state of one Run. Only the scheduler goroutine
// touches it, except halt and stop, which workers also use.
type run struct {
	x    *Executor
	ctx  context.Context
	env  *Env
	j    *Journal
	emit func(key, status, msg string)
	now  func() time.Time

	steps map[string]Step
	deps  map[string][]string
	order []string
	done  map[string]bool // done or skipped, nothing to rebuild
	// launched holds steps handed to a worker or blocked in this Run.
	launched map[string]bool
	bad      map[string]bool // failed or blocked in this Run
	// reopen is Graph.Reopen; inGraph holds the keys of the graph's steps.
	reopen  map[string][]string
	inGraph map[string]bool
	// reopened holds the done or skipped steps that act again in this Run.
	reopened map[string]bool

	halt     chan struct{} // closed on stop-run or journal error
	haltOnce sync.Once
}

// Run executes g: every step whose journal status is not done or skipped
// runs once its dependencies are done or skipped, at most Concurrency at a
// time across the executor's runs. Each step is journaled started before it
// runs and with its result after. A failed step blocks its transitive
// dependents. A step returning More adds those steps, after itself, unless
// their key is known; a done step whose added steps are not all done runs
// again as a rebuild (see Step.Run) so that it adds them again. A rebuild
// is not journaled started, and when it errors its record stays as it was:
// the error goes in an event with the unchanged status and in Run's error.
// A done or skipped step that g.Reopen lists keys for, any of them a step of
// g whose record is not done or skipped, is reopened instead: it runs as a
// new attempt, with its old record as prior but Status pending, and is
// journaled started, done or failed like any step (StepRecord.Acted stays).
// A reopened step blocked by a failed dependency keeps its record, so the
// next Run reopens it again; only the blocked event is sent.
//
// onEvent may be nil. It is called for one Run at a time, from the
// executor's goroutines, and must not block: a slow callback holds up the
// scheduler and the steps waiting to report.
//
// Run returns after every started step has returned: ctx.Err() when ctx
// ended first, an error wrapping ErrStopped when a step asked to stop the
// run, or an error naming the failed steps. After a stop or a journal error
// no further step starts, steps still waiting for a slot stay pending.
func (x *Executor) Run(ctx context.Context, g Graph, env *Env, onEvent func(Event)) error {
	var evMu sync.Mutex
	r := &run{
		x: x, ctx: ctx, env: env, j: env.Journal,
		emit: func(key, status, msg string) {
			if onEvent == nil {
				return
			}
			evMu.Lock()
			defer evMu.Unlock()
			onEvent(Event{RunID: env.RunID, StepKey: key, Status: status, Error: msg})
		},
		now: func() time.Time {
			if env.Now != nil {
				return env.Now()
			}
			return time.Now().UTC()
		},
		steps:    make(map[string]Step, len(g.Steps)),
		deps:     make(map[string][]string, len(g.Steps)),
		done:     map[string]bool{},
		launched: map[string]bool{},
		bad:      map[string]bool{},
		reopen:   g.Reopen,
		inGraph:  make(map[string]bool, len(g.Steps)),
		reopened: map[string]bool{},
		halt:     make(chan struct{}),
	}
	for k := range g.Steps {
		r.inGraph[k] = true
	}

	keys := slices.Clone(g.Order)
	var rest []string
	for k := range g.Steps {
		if !slices.Contains(keys, k) {
			rest = append(rest, k)
		}
	}
	slices.Sort(rest)
	for _, k := range slices.Concat(keys, rest) {
		if st, ok := g.Steps[k]; ok && r.steps[k] == nil {
			if err := r.add(st); err != nil {
				return err
			}
		}
	}

	out := make(chan completion)
	running := 0
	var (
		stopErr, journalErr error
		failed              []error
	)
	fail := func(err error) {
		journalErr = errors.Join(journalErr, err)
		r.stop()
	}
	for {
		if !r.halted() && ctx.Err() == nil {
			for _, k := range r.order {
				if r.done[k] || r.launched[k] || !r.ready(k) {
					continue
				}
				r.launched[k] = true
				running++
				go x.work(ctx, r.halt, r.stop, k, r.reopened[k], r.steps[k], env, r.emit, r.now, out)
			}
		}
		if running == 0 {
			break
		}
		c := <-out
		running--
		switch {
		case c.journalErr != nil:
			fail(fmt.Errorf("journal %s: %w", c.key, c.journalErr))
		case !c.ran || c.interrupted:
			// Left for the next Run: pending, or started (it checks first).
		case c.rebuild && c.err != nil:
			// The record stays done: the step acted in an earlier run and a
			// failed or started record would make it act again. Its
			// dependents may run; the next Run rebuilds it again.
			r.done[c.key] = true
			r.emit(c.key, r.j.Step(c.key).Status, c.err.Error())
			if IsStopRun(c.err) {
				stopErr = fmt.Errorf("%w at %s: %w", ErrStopped, c.key, c.err)
				r.stop()
			} else {
				failed = append(failed, fmt.Errorf("step %s rebuild failed: %w", c.key, c.err))
			}
		case c.err != nil && IsStopRun(c.err):
			stopErr = fmt.Errorf("%w at %s: %w", ErrStopped, c.key, c.err)
			r.stop()
			if err := r.finish(c.key, StatusFailed, c.err.Error(), nil); err != nil {
				fail(err)
			}
		case c.err != nil:
			failed = append(failed, fmt.Errorf("step %s failed: %w", c.key, c.err))
			if err := r.finish(c.key, StatusFailed, c.err.Error(), nil); err != nil {
				fail(err)
				break
			}
			if err := r.block(c.key); err != nil {
				fail(err)
			}
		default:
			// Only the steps this one newly adds are its own to rebuild.
			var added []Step
			var keys []string
			for _, m := range c.res.More {
				if r.steps[m.Key()] == nil && !slices.Contains(keys, m.Key()) {
					added = append(added, m)
					keys = append(keys, m.Key())
				}
			}
			status := StatusDone
			if c.rebuild {
				status = r.j.Step(c.key).Status // done or skipped stays so
			}
			if err := r.finish(c.key, status, "", keys); err != nil {
				fail(err)
				break
			}
			r.done[c.key] = true
			for _, m := range added {
				if err := r.add(m, c.key); err != nil {
					fail(err)
				}
			}
		}
	}

	switch {
	case journalErr != nil:
		return journalErr
	case stopErr != nil:
		return stopErr
	}
	var left []string
	for _, k := range r.order {
		if !r.done[k] && !r.launched[k] {
			left = append(left, k)
		}
	}
	unfinished := slices.ContainsFunc(r.order, func(k string) bool { return !r.done[k] })
	switch {
	case ctx.Err() != nil && unfinished:
		return ctx.Err()
	case len(failed) > 0:
		return errors.Join(failed...)
	case len(left) > 0:
		return fmt.Errorf("steps never became ready: %s", strings.Join(left, ", "))
	}
	return nil
}

// finish journals a step's result and reports it. It re-reads the record,
// keeping what the step wrote itself (a patch id, data), and sets Expanded
// to the keys a done or skipped step newly added.
func (r *run) finish(key, status, msg string, expanded []string) error {
	rec := r.j.Step(key)
	rec.Status, rec.Error, rec.At = status, msg, r.now()
	if final(status) {
		rec.Expanded = expanded
	}
	if status == StatusDone {
		rec.Acted = true // never cleared
	}
	if err := r.j.Set(key, rec); err != nil {
		return fmt.Errorf("journal %s: %w", key, err)
	}
	r.emit(key, status, msg)
	return nil
}

// stop closes halt once: queued workers give up and nothing new starts.
// The worker of a stopping step calls it before it frees its slot, so no
// queued step slips in before the scheduler hears of the stop.
func (r *run) stop() { r.haltOnce.Do(func() { close(r.halt) }) }

func (r *run) halted() bool {
	select {
	case <-r.halt:
		return true
	default:
		return false
	}
}

// add puts a step in the run, journaling it pending when it has no record.
// A step whose record is done or skipped is reopened while a key Reopen
// lists for it is pending (reopens); otherwise, with nothing it added left
// to do, it counts as done. One that depends on a step failed or blocked in
// this Run is blocked at once.
func (r *run) add(st Step, extra ...string) error {
	k := st.Key()
	r.steps[k] = st
	r.deps[k] = slices.Concat(st.Deps(), extra)
	r.order = append(r.order, k)
	if r.j.Step(k).Status == "" {
		if err := r.j.Set(k, StepRecord{Status: StatusPending, At: r.now()}); err != nil {
			return fmt.Errorf("journal %s: %w", k, err)
		}
	}
	if final(r.j.Step(k).Status) {
		switch {
		case r.reopens(k):
			r.reopened[k] = true
		case !r.rebuild(k, map[string]bool{k: true}):
			r.done[k] = true
			return nil
		}
	}
	if slices.ContainsFunc(r.deps[k], func(d string) bool { return r.bad[d] }) {
		return r.markBlocked(k)
	}
	return nil
}

// reopens reports whether a key Reopen lists for k is a step of the graph
// whose journal record is not done or skipped.
func (r *run) reopens(k string) bool {
	return slices.ContainsFunc(r.reopen[k], func(l string) bool {
		return l != k && r.inGraph[l] && !final(r.j.Step(l).Status)
	})
}

// rebuild reports whether a step added, directly or through the steps it
// added, a step that is not done or skipped in the journal.
func (r *run) rebuild(k string, seen map[string]bool) bool {
	for _, e := range r.j.Step(k).Expanded {
		if seen[e] {
			continue
		}
		seen[e] = true
		if !final(r.j.Step(e).Status) || r.rebuild(e, seen) {
			return true
		}
	}
	return false
}

func (r *run) ready(k string) bool {
	for _, d := range r.deps[k] {
		if r.steps[d] != nil && !r.done[d] {
			return false
		}
	}
	return true
}

// block marks every transitive dependent of failed that is not done or
// already handled as blocked, so it does not run.
func (r *run) block(failed string) error {
	r.bad[failed] = true
	var errs []error
	for changed := true; changed; {
		changed = false
		for _, k := range r.order {
			if r.bad[k] || r.done[k] || r.launched[k] {
				continue
			}
			if slices.ContainsFunc(r.deps[k], func(d string) bool { return r.bad[d] }) {
				changed = true
				if err := r.markBlocked(k); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (r *run) markBlocked(k string) error {
	r.bad[k], r.launched[k] = true, true
	if r.reopened[k] {
		// Its record stays done or skipped, so the next Run reopens it again.
		r.emit(k, StatusBlocked, "")
		return nil
	}
	rec := r.j.Step(k)
	rec.Status, rec.Error, rec.At = StatusBlocked, "", r.now()
	err := r.j.Set(k, rec)
	r.emit(k, StatusBlocked, "")
	if err != nil {
		return fmt.Errorf("journal %s: %w", k, err)
	}
	return nil
}

// work runs one step under its place lock and a concurrency slot, and
// journals started before Run and the outcomes after; the scheduler
// journals the result (finish). It gives
// up without starting the step when ctx ends or halt closes first. On a
// stop-run or journal error it calls stop before it frees its slot and
// lock, so no queued step slips in before the scheduler hears of it.
func (x *Executor) work(ctx context.Context, halt <-chan struct{}, stop func(), key string, reopened bool, st Step, env *Env,
	emit func(key, status, msg string), now func() time.Time, out chan<- completion,
) {
	c := completion{key: key}
	defer func() { out <- c }()

	if lk := st.Lock(); lk != "" {
		unlock, ok := x.lock(ctx, halt, lk)
		if !ok {
			return
		}
		defer unlock()
	}
	sem := x.slots()
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return
	case <-halt:
		return
	}
	defer func() { <-sem }()
	select {
	case <-ctx.Done():
		return
	case <-halt:
		return
	default:
	}

	x.attempt(ctx, &c, reopened, st, env, emit, now)
	if c.journalErr != nil || c.err != nil && IsStopRun(c.err) {
		stop()
	}
}

// attempt journals started, runs the step and journals its outcomes; the
// scheduler journals the result. A reopened step gets its old record with
// Status pending, so it acts again rather than rebuild.
func (x *Executor) attempt(ctx context.Context, c *completion, reopened bool, st Step, env *Env,
	emit func(key, status, msg string), now func() time.Time,
) {
	j := env.Journal
	prior := j.Step(c.key)
	if reopened {
		prior.Status = StatusPending
	}
	// A done or skipped step runs again only as a rebuild (see Step.Run).
	// It does not act, so its record stays as it is until it succeeds.
	c.rebuild = final(prior.Status)
	if !c.rebuild {
		rec := prior
		rec.Status, rec.Error, rec.At = StatusStarted, "", now()
		if err := j.Set(c.key, rec); err != nil {
			c.journalErr = err
			return
		}
		emit(c.key, StatusStarted, "")
	}
	c.ran = true
	c.res, c.err = st.Run(ctx, env, prior)

	for id, o := range c.res.Outcomes {
		if err := j.SetOutcome(id, o); err != nil {
			c.journalErr = err
			return
		}
	}
	if c.err != nil && ctx.Err() != nil && !IsStopRun(c.err) {
		c.interrupted = true
	}
}

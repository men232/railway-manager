package apply

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

// RailwayAPI is the part of *railway.Client that steps use.
type RailwayAPI interface {
	ReadVariables(ctx context.Context, projectID, envID, serviceID string, unrendered bool) (map[string]string, error)
	CommitPatch(ctx context.Context, envID string, patch map[string]any, message string) (string, error)
	PatchStatus(ctx context.Context, patchID string) (status, lastError string, err error)
	Redeploy(ctx context.Context, envID, serviceID string) error
	Deployments(ctx context.Context, envID, serviceID string) ([]railway.Deployment, error)
	ActiveDeployment(ctx context.Context, envID, serviceID string) (string, error)
	RestartDeployment(ctx context.Context, envID, deploymentID string) error
	DiscoverEnv(ctx context.Context, projectID, envID string) ([]railway.VarRecord, error)
	EnvironmentConfig(ctx context.Context, envID string) (railway.EnvConfig, error)
	Domains(ctx context.Context, projectID, envID, serviceID string) (railway.Domains, error)
	ServiceDomainCreate(ctx context.Context, envID, serviceID string, port int) (railway.ServiceDomain, error)
	ServiceDomainDelete(ctx context.Context, envID, id string) error
	CustomDomainCreate(ctx context.Context, projectID, envID, serviceID, host string, port int) (railway.CustomDomain, error)
	CustomDomainDelete(ctx context.Context, envID, id string) error
	ServiceVolumes(ctx context.Context, projectID string) ([]railway.VolumeMount, error)
	// CheckWrite is the client's write guard, for a write that does not go
	// through the API (a database password).
	CheckWrite(envID string) error
}

var _ RailwayAPI = (*railway.Client)(nil)

// StoreView is the read-only store a step uses for labels; fresh data
// always comes from the API.
type StoreView interface {
	Variable(locID string) *store.Variable
	PlaceOf(envID, serviceID string) domain.Place
	ServiceIDByName(envID, name string) string
	EnvName(envID string) string
	ServiceName(serviceID string) string
}

var _ StoreView = (*store.Store)(nil)

// Env is what a step runs with. Later stages add fields.
type Env struct {
	API     RailwayAPI
	Store   StoreView
	Journal *Journal
	RunID   string
	// Proc runs the database clients and ssh tunnels.
	Proc proc.Runner
	// KnownHosts is the known hosts file ssh tunnels trust
	// (dbops.KnownHostsFile); a host it does not hold gets no tunnel.
	KnownHosts string
	Sleep      func(ctx context.Context, d time.Duration) error
	Now        func() time.Time
	// RedeployOff holds the place keys whose redeploy the user switched off;
	// steps never add a redeploy there.
	RedeployOff map[string]bool
	// Changes are the run's frozen changes (Runs.Execute sets them): a step
	// may read another change of its run, e.g. a database user's step the
	// new root password of the same run. They hold
	// values: never log or journal them.
	Changes []changeset.Change
	// Actions are the run's redeploy and restart actions frozen with it
	// (Runs.Execute sets them): a password pre-flight checks its group's
	// redeploy targets from them, never recomputing them.
	Actions []FrozenAction

	mu    sync.Mutex
	cache map[string]map[string]string
}

// ReadPlace returns the unrendered variables of a place, read once per Env
// and cached until Forget. The returned map is the caller's to keep.
func (e *Env) ReadPlace(ctx context.Context, p domain.Place) (map[string]string, error) {
	e.mu.Lock()
	m, ok := e.cache[p.Key()]
	e.mu.Unlock()
	if ok {
		return maps.Clone(m), nil
	}
	m, err := e.API.ReadVariables(ctx, p.ProjectID, p.EnvID, p.ServiceID, true)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.cache == nil {
		e.cache = map[string]map[string]string{}
	}
	e.cache[p.Key()] = m
	e.mu.Unlock()
	return maps.Clone(m), nil
}

// MarkActed records in the journal, before returning, that the step key may
// have acted (StepRecord.Acted). A step calls it right before it sends a
// state-changing command, so a failure after the command still leaves the
// mark. Without a journal it does nothing.
func (e *Env) MarkActed(key string) error {
	if e.Journal == nil {
		return nil
	}
	rec := e.Journal.Step(key)
	if rec.Acted {
		return nil
	}
	rec.Acted = true
	return e.Journal.Set(key, rec)
}

// Forget drops the cached variables of a place, e.g. after a commit.
func (e *Env) Forget(p domain.Place) {
	e.mu.Lock()
	delete(e.cache, p.Key())
	e.mu.Unlock()
}

// Outcome is what a run did to one change.
type Outcome struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Result is what one Run of a step produced.
type Result struct {
	Outcomes map[string]Outcome // change id → outcome
	More     []Step             // steps to add after this one
}

// Step is one node of the DAG. Keys are stable across re-runs of a run.
type Step interface {
	Key() string
	Deps() []string
	Serves() []string // change ids
	Lock() string     // place key for per-service serialisation, "" for none
	Label() string
	// Run acts; prior is the journal record from before this attempt, so a
	// step knows whether it acted in an earlier run.
	//
	// prior.Status done means a rebuild: the step finished in an earlier run
	// but steps it added through More are not all done, so the executor runs
	// it again only to get them back. A rebuild must not act, check or
	// change outcomes; it returns the same More it would add after its
	// effect, derived from the journal and fresh reads.
	//
	// A done step the graph reopens (Graph.Reopen) is not a rebuild: it gets
	// its old record with Status pending and acts again.
	Run(ctx context.Context, env *Env, prior StepRecord) (Result, error)
}

// Reopenable is implemented by a step that opts in to Graph.Reopen: Compile
// lists reopen keys only for a step serving a Reopener kind's change whose
// Reopenable is true (the dbpw step, not its pre-flight).
type Reopenable interface {
	Reopenable() bool
}

// SuccessWaiter is implemented by a redeploy step that is done only once
// the new deployment reports SUCCESS. Runs freezes the flag
// with the run's actions.
type SuccessWaiter interface {
	WaitsForSuccess() bool
}

// unwrapper is a step wrapping another (the compiler's and the tracker's).
// Every Step wrapper must implement it, or the optional step interfaces
// (SuccessWaiter, Reopenable) of the wrapped step are not seen.
type unwrapper interface {
	Unwrap() Step
}

// waits reports whether st, or the step it wraps, waits for SUCCESS.
func waits(st Step) bool {
	w, ok := as[SuccessWaiter](st)
	return ok && w.WaitsForSuccess()
}

// reopenable reports whether st, or the step it wraps, opts in to reopen.
func reopenable(st Step) bool {
	r, ok := as[Reopenable](st)
	return ok && r.Reopenable()
}

// as finds T on st or on the steps it wraps.
func as[T any](st Step) (T, bool) {
	for {
		if t, ok := st.(T); ok {
			return t, true
		}
		u, ok := st.(unwrapper)
		if !ok {
			var zero T
			return zero, false
		}
		st = u.Unwrap()
	}
}

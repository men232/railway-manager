package apply_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

// Live tests run against a Railway environment you name; none is built in:
//
//	LIVE_RAILWAY=1    opt in
//	RM_LIVE_PROJECT   the project, by name or id
//	RM_LIVE_ENV       an environment of that project, by name or id
//	RM_LIVE_SERVICE   an app service (name or id) the tests may give RM_TEST_
//	                  variables, redeploy, restart, resize and add test domains to
//	RM_LIVE_WORKER    a second app service, for the database password tests only
//
// The environment must also hold the shared variable RM_LIVE_TEST_TARGET=1:
// the tests refuse any environment not marked as a test target. They change
// only RM_TEST_ variables, rm-test-*.example.com domains, and (the database
// password tests) the Redis password of that environment.
const (
	livePrefix = "RM_TEST_"
	liveMarker = "RM_LIVE_TEST_TARGET"
)

// The live target, resolved once by liveSkip: the project and environment
// ids, the service and worker names (liveWorker may be empty).
var (
	liveProject, liveEnv, liveService, liveWorker string

	liveOnce sync.Once
	liveErr  error
)

type liveRig struct {
	api       *railway.Client
	tokens    railway.TokenSource
	st        *store.Store
	runs      *apply.Runs
	serviceID string
	place     domain.Place
	// dbs are the databases of liveEnv a db password test may change (set
	// by liveDBRig); empty for every other live test, which then fail
	// closed on a db.password change.
	dbs []liveDB
	// extraApps are further services (worker) whose RM_TEST_ variables a db
	// password test may rewrite.
	extraApps map[string]bool
}

// liveSkip skips the test unless LIVE_RAILWAY=1, then resolves the target
// from RM_LIVE_* and fails the test if it is incomplete or not marked.
func liveSkip(t *testing.T) {
	t.Helper()
	if os.Getenv("LIVE_RAILWAY") != "1" {
		t.Skip("set LIVE_RAILWAY=1 and RM_LIVE_PROJECT, RM_LIVE_ENV, RM_LIVE_SERVICE to run against the real Railway API")
	}
	liveOnce.Do(func() { liveErr = resolveLiveTarget() })
	if liveErr != nil {
		t.Fatal(liveErr)
	}
}

// resolveLiveTarget fills liveProject, liveEnv, liveService and liveWorker
// from RM_LIVE_*, matching names or ids exactly, and checks the marker.
func resolveLiveTarget() error {
	want := map[string]string{}
	var missing []string
	for _, k := range []string{"RM_LIVE_PROJECT", "RM_LIVE_ENV", "RM_LIVE_SERVICE"} {
		if want[k] = strings.TrimSpace(os.Getenv(k)); want[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("live tests need %s", strings.Join(missing, ", "))
	}
	worker := strings.TrimSpace(os.Getenv("RM_LIVE_WORKER"))
	api, err := newReadClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cat, err := api.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("ListProjects: %w", err)
	}
	var p *railway.Project
	for i := range cat.Projects {
		if q := &cat.Projects[i]; q.ID == want["RM_LIVE_PROJECT"] || q.Name == want["RM_LIVE_PROJECT"] {
			if p != nil {
				return fmt.Errorf("RM_LIVE_PROJECT %q matches more than one project; use its id", want["RM_LIVE_PROJECT"])
			}
			p = q
		}
	}
	if p == nil {
		return fmt.Errorf("RM_LIVE_PROJECT %q: no such project", want["RM_LIVE_PROJECT"])
	}
	envID := ""
	for _, e := range p.Environments {
		if e.ID == want["RM_LIVE_ENV"] || e.Name == want["RM_LIVE_ENV"] {
			if envID != "" {
				return fmt.Errorf("RM_LIVE_ENV %q matches more than one environment; use its id", want["RM_LIVE_ENV"])
			}
			envID = e.ID
		}
	}
	if envID == "" {
		return fmt.Errorf("RM_LIVE_ENV %q: no such environment in the project", want["RM_LIVE_ENV"])
	}
	service := func(key, v string) (string, error) {
		name := ""
		for _, s := range p.Services {
			if s.ID == v || s.Name == v {
				if name != "" {
					return "", fmt.Errorf("%s %q matches more than one service; use its id", key, v)
				}
				name = s.Name
			}
		}
		if name == "" {
			return "", fmt.Errorf("%s %q: no such service in the project", key, v)
		}
		return name, nil
	}
	svc, err := service("RM_LIVE_SERVICE", want["RM_LIVE_SERVICE"])
	if err != nil {
		return err
	}
	if worker != "" {
		if worker, err = service("RM_LIVE_WORKER", worker); err != nil {
			return err
		}
		if worker == svc {
			return errors.New("RM_LIVE_WORKER must name a different service than RM_LIVE_SERVICE")
		}
	}
	shared, err := api.ReadVariables(ctx, p.ID, envID, "", true)
	if err != nil {
		return fmt.Errorf("reading the shared variables of the environment: %w", err)
	}
	if shared[liveMarker] != "1" {
		return fmt.Errorf("the environment is not marked as a test target: add the shared variable %s=1 to it, and only to an environment the tests may change", liveMarker)
	}
	liveProject, liveEnv, liveService, liveWorker = p.ID, envID, svc, worker
	return nil
}

func randomValue(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b) // 24 chars
}

// liveHostRE is the only custom domain host a live test may add or delete.
var liveHostRE = regexp.MustCompile(`^rm-test-[0-9]+\.example\.com$`)

// assertLiveSafe fails the test unless every change targets liveEnv in
// liveProject, the rig's service and a known kind with a safe target:
// variables (and redeploy, which fails closed) only RM_TEST_ names, restart
// and limits their fixed targets, custom domains only liveHostRE hosts. It
// runs before any mutation.
//
// Database passwords: only when the rig holds liveDBs (liveDBRig),
// and only these RM_TEST_-free targets are allowed on top of the above:
// db.password on the owner of one of those databases with Target its root
// user; variable.set on that owner with Target its root-password variable
// (REDIS_PASSWORD and the like, the database's own variable); and
// variable.set of RM_TEST_ names on the rig's service and on extraApps. Any
// other database user, variable or service still fails the test.
func (r *liveRig) assertLiveSafe(t *testing.T, cs ...changeset.Change) {
	t.Helper()
	if err := r.liveUnsafe(cs...); err != nil {
		t.Fatal(err)
	}
}

// liveUnsafe is assertLiveSafe's judgement: nil when every change is safe.
func (r *liveRig) liveUnsafe(cs ...changeset.Change) error {
	for _, c := range cs {
		if c.Place.EnvID != liveEnv || c.Place.ProjectID != liveProject {
			return fmt.Errorf("unsafe live change: project %q env %q", c.Place.ProjectID, c.Place.EnvID)
		}
		var db *liveDB
		for i := range r.dbs {
			if r.dbs[i].kind == "redis" && r.dbs[i].owner.ServiceID == c.Place.ServiceID {
				db = &r.dbs[i]
			}
		}
		if c.Place.ServiceID != r.serviceID && db == nil && !(r.extraApps[c.Place.ServiceID] && c.Kind == "variable.set") {
			return fmt.Errorf("unsafe live change: kind %q service %q", c.Kind, c.Place.ServiceID)
		}
		if c.Kind != "db.password" && c.Kind != "variable.set" && c.Place.ServiceID != r.serviceID {
			return fmt.Errorf("unsafe live change: kind %q service %q", c.Kind, c.Place.ServiceID)
		}
		ok := false
		switch c.Kind {
		case "db.password":
			ok = db != nil && c.Target == db.rootUser
		case "variable.set", "variable.delete", "service.redeploy":
			app := c.Place.ServiceID == r.serviceID || (r.extraApps[c.Place.ServiceID] && c.Kind == "variable.set")
			ok = app && strings.HasPrefix(c.Target, livePrefix)
			if c.Kind == "variable.set" && db != nil && !app {
				ok = c.Target == db.rootVar
			}
		case "service.restart":
			ok = c.Target == "restart"
		case "limits.set":
			ok = c.Target == "limits"
		case "domain.custom.add", "domain.custom.delete":
			host, found := strings.CutPrefix(c.Target, "custom-domain:")
			ok = found && liveHostRE.MatchString(host)
		default:
			return fmt.Errorf("unsafe live change: kind %q", c.Kind)
		}
		if !ok {
			return fmt.Errorf("unsafe live change: kind %q target %q", c.Kind, c.Target)
		}
	}
	return nil
}

func newLiveRig(t *testing.T) *liveRig {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	tokens := railway.NewTokenSource(os.Getenv, filepath.Join(home, ".railway", "config.json"), proc.RailwayWhoami, time.Now)
	api := &railway.Client{
		T:            &railway.Transport{Endpoint: railway.DefaultEndpoint, HTTP: &http.Client{Timeout: 60 * time.Second}, Tokens: tokens},
		WriteAllowed: func(envID string) bool { return envID == liveEnv },
	}
	ctx := context.Background()
	cat, err := api.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	var serviceID string
	for _, p := range cat.Projects {
		if p.ID != liveProject {
			continue
		}
		for _, s := range p.Services {
			if s.Name != liveService {
				continue
			}
			if serviceID != "" {
				t.Fatalf("service name %s is ambiguous in project %s", liveService, liveProject)
			}
			serviceID = s.ID
		}
	}
	if serviceID == "" {
		t.Fatalf("service %s not found in project %s", liveService, liveProject)
	}
	st := store.New()
	st.ReplaceCatalog(cat)
	rig := &liveRig{api: api, tokens: tokens, st: st, serviceID: serviceID, place: st.PlaceOf(liveEnv, serviceID)}
	if rig.place.EnvID != liveEnv || rig.place.ProjectID != liveProject {
		t.Fatalf("place = %+v, want env %s in project %s", rig.place, liveEnv, liveProject)
	}
	rig.refresh(t)

	reg := kinds.NewRegistry()
	rig.runs = &apply.Runs{
		Dir:  t.TempDir(),
		Reg:  reg,
		Exec: &apply.Executor{},
		NewEnv: func(runID string, j *apply.Journal) *apply.Env {
			return &apply.Env{
				API: api, Store: st, Journal: j, RunID: runID,
				Sleep: func(ctx context.Context, d time.Duration) error {
					select {
					case <-time.After(d):
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
				Now: time.Now,
			}
		},
		NewRedeploy: kinds.NewRedeployStep,
		Compile: func(_ *store.Store, cs changeset.ChangeSet) (apply.Graph, error) {
			return apply.Compile(cs, st, reg,
				func(envID string, changes []changeset.Change) apply.Step {
					return kinds.NewPatchStep(envID, changes, reg, liveProject)
				},
				kinds.NewRedeployStep)
		},
	}
	return rig
}

// refresh re-reads the live environment into the store.
func (r *liveRig) refresh(t *testing.T) {
	t.Helper()
	recs, err := r.api.DiscoverEnv(context.Background(), liveProject, liveEnv)
	if err != nil {
		t.Fatalf("DiscoverEnv: %v", err)
	}
	r.st.ReplaceVariables(liveProject, recs)
}

func (r *liveRig) read(t *testing.T) map[string]string {
	t.Helper()
	vars, err := r.api.ReadVariables(context.Background(), liveProject, liveEnv, r.serviceID, true)
	if err != nil {
		t.Fatalf("ReadVariables: %v", err)
	}
	return vars
}

// cleanup registers a best-effort delete of the named variables, run even if
// the test fails midway.
func (r *liveRig) cleanup(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if !strings.HasPrefix(n, livePrefix) {
			t.Fatalf("refusing to register cleanup of %q", n)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		present, err := r.api.ReadVariables(ctx, liveProject, liveEnv, r.serviceID, true)
		if err != nil {
			t.Errorf("cleanup could not read variables; leftover RM_TEST_ variables possible among %v: %v", names, err)
			return
		}
		entry := map[string]any{}
		var left []string
		for _, n := range names {
			if _, ok := present[n]; ok {
				entry[n] = nil
				left = append(left, n)
			}
		}
		if len(left) == 0 {
			return
		}
		patch := map[string]any{"services": map[string]any{r.serviceID: map[string]any{"variables": entry}}}
		id, err := r.api.CommitPatch(ctx, liveEnv, patch, "railway-manager live test cleanup")
		if err != nil {
			t.Errorf("cleanup failed, leftover RM_TEST_ variables %v: %v", left, err)
			return
		}
		if err := r.waitCommitted(ctx, id); err != nil {
			t.Errorf("cleanup not committed, leftover RM_TEST_ variables %v: %v", left, err)
		}
	})
}

func (r *liveRig) waitCommitted(ctx context.Context, patchID string) error {
	for {
		status, lastErr, err := r.api.PatchStatus(ctx, patchID)
		if err != nil {
			return err
		}
		switch status {
		case "COMMITTED":
			return nil
		case "FAILED":
			return fmt.Errorf("patch %s failed: %s", patchID, lastErr)
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runOne prepares and executes a run of one change and requires it finished.
// With redeployOff the place's redeploy is switched off for the run.
func (r *liveRig) runOne(t *testing.T, c changeset.Change, redeployOff bool) {
	t.Helper()
	r.assertLiveSafe(t, c)
	live := &changeset.ChangeSet{}
	if err := live.Put(c, nil); err != nil {
		t.Fatal(err)
	}
	if redeployOff {
		live.ToggleRedeploy(r.place.Key())
	}
	// The two steps the TUI takes: check and compile a copy of the live
	// set, lock the selection in the live set, then freeze under that id.
	pl, err := r.runs.LockPrepare()
	if err != nil {
		t.Fatal(err)
	}
	plan, conflicts, err := pl.Check(live.Clone(), nil)
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("Check: %v, conflicts %v", errors.Join(err, pl.Release()), conflicts)
	}
	id := apply.NewRunID()
	if err := live.Lock(plan.IDs, id); err != nil {
		t.Fatalf("Lock: %v", errors.Join(err, pl.Release()))
	}
	if err := errors.Join(pl.Freeze(plan, id), pl.Release()); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	r.execute(t, id)
}

// runImmediate is runOne for a change that is not in the live set (restart).
func (r *liveRig) runImmediate(t *testing.T, c changeset.Change) {
	t.Helper()
	r.assertLiveSafe(t, c)
	id, conflicts, err := r.runs.PrepareImmediate(c)
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("PrepareImmediate: %v, conflicts %v", err, conflicts)
	}
	r.execute(t, id)
}

func (r *liveRig) execute(t *testing.T, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rep, err := r.runs.Execute(ctx, id, func(apply.Event) {})
	if err != nil {
		t.Fatalf("Execute %s: %v", id, err)
	}
	if rep.State != apply.RunFinished {
		t.Fatalf("run %s state = %s, want %s (done %v, kept %v)", id, rep.State, apply.RunFinished, rep.Done, rep.Kept)
	}
	r.refresh(t)
}

func TestLiveAddEditDeleteRedeploy(t *testing.T) {
	liveSkip(t)
	rig := newLiveRig(t)
	name := fmt.Sprintf("%s%d", livePrefix, time.Now().Unix())
	rig.cleanup(t, name)
	if _, exists := rig.read(t)[name]; exists {
		t.Fatalf("%s already exists", name)
	}

	// Add, redeploy off.
	v1 := randomValue(t)
	add, err := kinds.SetVariable(rig.st, nil, rig.place, name, v1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, add, true)
	if got, ok := rig.read(t)[name]; !ok || got != v1 {
		t.Fatalf("after add: %s present=%v matches=%v", name, ok, got == v1)
	}

	// Edit, redeploy off.
	v2 := randomValue(t)
	edit, err := kinds.SetVariable(rig.st, nil, rig.place, name, v2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, edit, true)
	if got, ok := rig.read(t)[name]; !ok || got != v2 {
		t.Fatalf("after edit: %s present=%v matches=%v", name, ok, got == v2)
	}

	// Delete, redeploy on.
	del, err := kinds.DeleteVariable(rig.st, rig.place, name, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, del, false)
	if _, ok := rig.read(t)[name]; ok {
		t.Fatalf("after delete: %s still present", name)
	}
}

func TestLiveProbeConcurrentPatches(t *testing.T) {
	liveSkip(t)
	rig := newLiveRig(t)
	stamp := time.Now().Unix()
	names := []string{fmt.Sprintf("%sA_%d", livePrefix, stamp), fmt.Sprintf("%sB_%d", livePrefix, stamp)}
	rig.cleanup(t, names...)
	for _, n := range names {
		rig.assertLiveSafe(t, changeset.Change{Kind: "variable.set", Place: rig.place, Target: n})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	values := []string{randomValue(t), randomValue(t)}
	ids := make([]string, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			patch := map[string]any{"services": map[string]any{rig.serviceID: map[string]any{
				"variables": map[string]any{n: map[string]any{"value": values[i]}},
			}}}
			ids[i], errs[i] = rig.api.CommitPatch(ctx, liveEnv, patch, "railway-manager concurrent patch probe")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("CommitPatch %s: %v", names[i], err)
		}
	}
	for i, id := range ids {
		if err := rig.waitCommitted(ctx, id); err != nil {
			t.Fatalf("%s: %v", names[i], err)
		}
	}

	vars := rig.read(t)
	result := "ok"
	for _, n := range names {
		if _, ok := vars[n]; !ok {
			result = "lost " + n
			break
		}
	}
	t.Logf("PROBE concurrent patches: %s", result)
	if result != "ok" {
		t.Errorf("concurrent patches lost a variable: %s", result)
	}
}

// activeDeployment is the live service's running deployment; the test fails
// when nothing runs.
func (r *liveRig) activeDeployment(t *testing.T) string {
	t.Helper()
	id, err := r.api.ActiveDeployment(context.Background(), liveEnv, r.serviceID)
	if err != nil {
		t.Fatalf("ActiveDeployment: %v", err)
	}
	if id == "" {
		t.Fatalf("%s has no running deployment", liveService)
	}
	return id
}

// liveInstancesDoc is the Services screen's instances subscription.
const liveInstancesDoc = `subscription LiveInstances($input: DeploymentInstanceExecutionInput!) {
  deploymentInstanceExecutions(input: $input) { id status }
}`

type liveInstance struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// decodeInstances takes one instance or a list of them.
func decodeInstances(data json.RawMessage) ([]liveInstance, error) {
	var d struct {
		X json.RawMessage `json:"deploymentInstanceExecutions"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	var many []liveInstance
	if err := json.Unmarshal(d.X, &many); err == nil {
		return many, nil
	}
	var one liveInstance
	if err := json.Unmarshal(d.X, &one); err != nil {
		return nil, err
	}
	return []liveInstance{one}, nil
}

// TestLiveRestart restarts the live service's running deployment through a
// service.restart run and waits for a new instance of it to run.
func TestLiveRestart(t *testing.T) {
	liveSkip(t)
	rig := newLiveRig(t)
	dep := rig.activeDeployment(t)

	live := railway.NewLive(railway.LiveEndpoint, rig.tokens, nil)
	defer live.Close()
	events := make(chan []liveInstance, 256)
	fails := make(chan string, 16)
	ended := make(chan string, 1)
	stop := live.Subscribe(liveInstancesDoc, map[string]any{"input": map[string]any{"deploymentId": dep}},
		func(data json.RawMessage) {
			ins, err := decodeInstances(data)
			if err != nil {
				select {
				case fails <- "undecodable instances payload: " + err.Error():
				default:
				}
				return
			}
			select {
			case events <- ins:
			default:
			}
		},
		func(msg string, why railway.LiveFail) {
			ch := fails
			if why == railway.LiveFailEnded {
				ch = ended
			}
			select {
			case ch <- msg:
			default:
			}
		})
	defer stop()

	// collect feeds every instance seen within d to each until it says done.
	collect := func(d time.Duration, each func(liveInstance) bool) bool {
		timer := time.NewTimer(d)
		defer timer.Stop()
		for {
			select {
			case ins := <-events:
				for _, i := range ins {
					if each(i) {
						return true
					}
				}
			case msg := <-fails:
				t.Logf("instances subscription: %s", msg)
			case msg := <-ended:
				t.Fatalf("instances subscription ended: %s", msg)
			case <-timer.C:
				return false
			}
		}
	}

	before := map[string]string{}
	collect(15*time.Second, func(i liveInstance) bool { before[i.ID] = i.Status; return false })

	rig.runImmediate(t, kinds.Restart(rig.place, time.Now()))

	after := map[string]string{}
	var fresh string
	ok := collect(5*time.Minute, func(i liveInstance) bool {
		after[i.ID] = i.Status
		if _, old := before[i.ID]; !old && i.Status == "RUNNING" {
			fresh = i.ID
			return true
		}
		return false
	})
	if !ok {
		t.Fatalf("no new RUNNING instance of deployment %s within 5 minutes of the restart (before %v, after %v)", dep, before, after)
	}
	t.Logf("restart: deployment %s, instances before %v, new instance %s RUNNING", dep, before, fresh)
	if now := rig.activeDeployment(t); now != dep {
		t.Logf("running deployment is now %s (was %s)", now, dep)
	}
}

// liveLimitsDoc reads the effective limits (Railway answers JSON).
const liveLimitsDoc = `query LiveLimits($environmentId: String!, $serviceId: String!) {
  serviceInstanceLimits(environmentId: $environmentId, serviceId: $serviceId)
}`

const gib = 1 << 30 // Railway's GB

func (r *liveRig) effectiveLimits(t *testing.T) json.RawMessage {
	t.Helper()
	var d struct {
		L json.RawMessage `json:"serviceInstanceLimits"`
	}
	vars := map[string]any{"environmentId": liveEnv, "serviceId": r.serviceID}
	if err := r.api.T.Do(context.Background(), "LiveLimits", liveLimitsDoc, vars, &d); err != nil {
		t.Fatalf("serviceInstanceLimits: %v", err)
	}
	return d.L
}

// limitValues reads cpu and memory bytes from serviceInstanceLimits, as
// {containers: {cpu, memoryBytes}} or {vCPUs, memoryGB}.
func limitValues(raw json.RawMessage) (cpu, memBytes float64, ok bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return 0, 0, false
	}
	if c, has := m["containers"]; has {
		m = nil
		if json.Unmarshal(c, &m) != nil {
			return 0, 0, false
		}
	}
	num := func(k string) (float64, bool) {
		v, has := m[k]
		var f float64
		if !has || json.Unmarshal(v, &f) != nil {
			return 0, false
		}
		return f, true
	}
	cpu, okCPU := num("cpu")
	if !okCPU {
		cpu, okCPU = num("vCPUs")
	}
	memBytes, okMem := num("memoryBytes")
	if !okMem {
		var gb float64
		if gb, okMem = num("memoryGB"); okMem {
			memBytes = gb * gib
		}
	}
	return cpu, memBytes, okCPU && okMem
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// loadConfig reads the environment config into the store and returns the
// live service's override.
func (r *liveRig) loadConfig(t *testing.T) railway.ServiceConfig {
	t.Helper()
	cfg, err := r.api.EnvironmentConfig(context.Background(), liveEnv)
	if err != nil {
		t.Fatalf("EnvironmentConfig: %v", err)
	}
	r.st.SetConfig(liveEnv, cfg)
	return cfg.Services[r.serviceID]
}

// limitsCleanup registers a best-effort clear of any limit override left on
// the live service, with limitOverride {containers: null}.
func (r *liveRig) limitsCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cfg, err := r.api.EnvironmentConfig(ctx, liveEnv)
		if err != nil {
			t.Errorf("cleanup could not read the config; a limit override may be left on %s: %v", liveService, err)
			return
		}
		if sc := cfg.Services[r.serviceID]; sc.CPU == nil && sc.MemoryBytes == nil {
			return
		}
		patch := map[string]any{"services": map[string]any{r.serviceID: map[string]any{
			"deploy": map[string]any{"limitOverride": map[string]any{"containers": nil}},
		}}}
		id, err := r.api.CommitPatch(ctx, liveEnv, patch, "railway-manager live test cleanup")
		if err != nil {
			t.Errorf("cleanup failed, limit override left on %s: %v", liveService, err)
			return
		}
		if err := r.waitCommitted(ctx, id); err != nil {
			t.Errorf("cleanup not committed, limit override left on %s: %v", liveService, err)
			return
		}
		cfg, err = r.api.EnvironmentConfig(ctx, liveEnv)
		if err != nil {
			t.Errorf("cleanup could not read the config back: %v", err)
			return
		}
		if sc := cfg.Services[r.serviceID]; sc.CPU != nil || sc.MemoryBytes != nil {
			t.Errorf("cleanup committed but a limit override is still on %s", liveService)
		}
	})
}

// TestLiveLimitsSetAndClear sets a cpu 16 / memory 16 GB override with the
// redeploy off, then clears it. It refuses to run when the service already
// has an override, so the clear restores what was there.
func TestLiveLimitsSetAndClear(t *testing.T) {
	liveSkip(t)
	rig := newLiveRig(t)
	if sc := rig.loadConfig(t); sc.CPU != nil || sc.MemoryBytes != nil {
		t.Fatalf("%s already has a limit override; refusing to touch it", liveService)
	}
	before := rig.effectiveLimits(t)
	rig.limitsCleanup(t)

	cpu, mem := 16.0, int64(16*gib)
	set, err := kinds.SetLimits(rig.st, rig.place, &cpu, &mem, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, set, true)
	sc := rig.loadConfig(t)
	if sc.CPU == nil || *sc.CPU != cpu || sc.MemoryBytes == nil || *sc.MemoryBytes != mem {
		t.Fatalf("config after set: cpu %v memoryBytes %v, want %v and %d", sc.CPU, sc.MemoryBytes, cpu, mem)
	}
	eff := rig.effectiveLimits(t)
	if c, m, ok := limitValues(eff); !ok || c != cpu || m != float64(mem) {
		t.Fatalf("serviceInstanceLimits after set = %s, want cpu 16 and memory 16 GB", eff)
	}

	unset, err := kinds.SetLimits(rig.st, rig.place, nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, unset, true)
	if sc := rig.loadConfig(t); sc.CPU != nil || sc.MemoryBytes != nil {
		t.Fatalf("config after clear still has an override: cpu %v memoryBytes %v", sc.CPU, sc.MemoryBytes)
	}
	if after := rig.effectiveLimits(t); !sameJSON(before, after) {
		t.Fatalf("serviceInstanceLimits after clear = %s, want %s as before", after, before)
	}
}

// loadDomains reads the live service's domains into the store.
func (r *liveRig) loadDomains(t *testing.T) railway.Domains {
	t.Helper()
	d, err := r.api.Domains(context.Background(), liveProject, liveEnv, r.serviceID)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	r.st.SetDomains(r.place, d)
	return d
}

func hasCustomHost(d railway.Domains, host string) bool {
	for _, c := range d.Custom {
		if strings.EqualFold(c.Domain, host) {
			return true
		}
	}
	return false
}

// testHosts lists the live service's custom domains a test may delete.
func testHosts(d railway.Domains) []railway.CustomDomain {
	var out []railway.CustomDomain
	for _, c := range d.Custom {
		if liveHostRE.MatchString(strings.ToLower(c.Domain)) {
			out = append(out, c)
		}
	}
	return out
}

// domainsCleanup registers a best-effort delete of every rm-test- custom
// domain left on the live service.
func (r *liveRig) domainsCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		d, err := r.api.Domains(ctx, liveProject, liveEnv, r.serviceID)
		if err != nil {
			t.Errorf("cleanup could not read domains; rm-test- domains may be left on %s: %v", liveService, err)
			return
		}
		left := testHosts(d)
		if len(left) == 0 {
			return
		}
		var errs []error
		for _, c := range left {
			if err := r.api.CustomDomainDelete(ctx, liveEnv, c.ID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.Domain, err))
			}
		}
		if err := errors.Join(errs...); err != nil {
			t.Errorf("cleanup could not delete domains: %v", err)
		}
		if d, err = r.api.Domains(ctx, liveProject, liveEnv, r.serviceID); err != nil {
			t.Errorf("cleanup could not read domains back: %v", err)
		} else if left := testHosts(d); len(left) > 0 {
			t.Errorf("cleanup left %d rm-test- domains on %s", len(left), liveService)
		}
	})
}

// TestLiveCustomDomainAddDelete adds rm-test-<unix>.example.com (default
// port) to the live service, then deletes it.
func TestLiveCustomDomainAddDelete(t *testing.T) {
	liveSkip(t)
	rig := newLiveRig(t)
	host := fmt.Sprintf("rm-test-%d.example.com", time.Now().Unix())
	rig.domainsCleanup(t)
	if hasCustomHost(rig.loadDomains(t), host) {
		t.Fatalf("%s already exists", host)
	}

	add, err := kinds.AddCustomDomain(rig.st, rig.place, host, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, add, true)
	if !hasCustomHost(rig.loadDomains(t), host) {
		t.Fatalf("after add: %s not listed", host)
	}

	del, err := kinds.DeleteCustomDomain(rig.st, rig.place, host, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, del, true)
	if hasCustomHost(rig.loadDomains(t), host) {
		t.Fatalf("after delete: %s still listed", host)
	}
}

// TestLiveDeploymentLogsBacklog is a read-only probe for the Logs screen's
// dedupe: does the subscription's first batch repeat the lines just read?
// It logs counts only, never a line (log lines may hold secrets).
func TestLiveDeploymentLogsBacklog(t *testing.T) {
	liveSkip(t)
	rig := newLiveRig(t)
	dep := rig.activeDeployment(t)
	const limit = 50
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	read, err := rig.api.DeploymentLogs(ctx, dep, limit)
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	type key struct {
		at  int64
		msg string
	}
	readSet := map[key]int{}
	var newest time.Time
	for _, l := range read {
		readSet[key{l.Timestamp.UnixNano(), l.Message}]++
		if l.Timestamp.After(newest) {
			newest = l.Timestamp
		}
	}

	live := railway.NewLive(railway.LiveEndpoint, rig.tokens, nil)
	defer live.Close()
	batches := make(chan json.RawMessage, 1024)
	fails := make(chan string, 16)
	ended := make(chan struct{}, 1)
	stop := live.Subscribe(railway.DeploymentLogsSubscription, map[string]any{"deploymentId": dep, "limit": limit},
		func(data json.RawMessage) {
			select {
			case batches <- data:
			default:
			}
		},
		func(msg string, why railway.LiveFail) {
			select {
			case fails <- msg:
			default:
			}
			if why == railway.LiveFailEnded {
				select {
				case ended <- struct{}{}:
				default:
				}
			}
		})
	defer stop()

	const window = 8 * time.Second
	timer := time.NewTimer(window)
	defer timer.Stop()
	var sizes []int
	var first []railway.LogLine
	total, repeats := 0, 0
	remaining := maps.Clone(readSet)
	wait := true
	for wait {
		select {
		case data := <-batches:
			lines, err := railway.DecodeDeploymentLogs(data)
			if err != nil {
				t.Errorf("decode a live batch: %v", err)
				continue
			}
			if first == nil {
				first = lines
			}
			sizes = append(sizes, len(lines))
			total += len(lines)
			for _, l := range lines {
				k := key{l.Timestamp.UnixNano(), l.Message}
				if remaining[k] > 0 {
					remaining[k]--
					repeats++
				}
			}
		case msg := <-fails:
			t.Errorf("logs subscription: %s", msg)
		case <-ended:
			wait = false
		case <-timer.C:
			wait = false
		}
	}

	firstRepeats, firstOlder, firstSame := 0, 0, 0
	seen := maps.Clone(readSet)
	for _, l := range first {
		if k := (key{l.Timestamp.UnixNano(), l.Message}); seen[k] > 0 {
			seen[k]--
			firstRepeats++
		}
		switch {
		case l.Timestamp.Before(newest):
			firstOlder++
		case l.Timestamp.Equal(newest):
			firstSame++
		}
	}
	n := len(sizes)
	if n > 10 {
		sizes = sizes[:10]
	}
	t.Logf("PROBE logs backlog: read %d lines (limit %d); %d live batches in %s, %d live lines, %d of them repeat read lines; first batch sizes %v",
		len(read), limit, n, window, total, repeats, sizes)
	t.Logf("PROBE first live batch: %d lines, %d repeat read lines, %d older than the newest read line, %d at its time",
		len(first), firstRepeats, firstOlder, firstSame)
}

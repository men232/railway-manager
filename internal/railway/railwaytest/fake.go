// Package railwaytest is an in-process fake Railway GraphQL server. It serves
// the documents of package railway by operationName and keeps variables,
// patches and deployments in memory.
package railwaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/railway"
)

// Mutation is one mutation the fake served. Vars are the GraphQL variables
// (JSON-decoded); CommitPatch also carries skipDeploys and commitMessage as
// the document sends them.
type Mutation struct {
	Op   string
	Vars map[string]any
}

type varKey struct{ env, svc, name string }

type varEntry struct {
	value   string
	sealed  bool
	updated time.Time
}

type patchState struct {
	applying int
	envID    string
	patch    map[string]any
	failMsg  string // non-empty: ends FAILED, applies nothing
	done     bool
}

// Fake is the fake server.
type Fake struct {
	URL string
	// WSURL is the websocket endpoint (graphql-transport-ws) of the same server.
	WSURL string

	// PageSize is the project page size of WorkspaceProjects (default 100),
	// TimestampPageSize that of the variable timestamp pages (default 500).
	// Set them before the first request.
	PageSize          int
	TimestampPageSize int
	// Delay is slept per request before it is served (outside the fake's
	// lock), so concurrent requests overlap; MaxInFlight reports the peak.
	Delay time.Duration

	inflight, maxInflight atomic.Int32
	patchFail             string
	envPatchFail          map[string]string

	mu         sync.Mutex
	catalog    railway.Catalog
	vars       map[varKey]varEntry
	system     map[varKey]string
	mutations  []Mutation
	failNext   map[string][]string
	failVars   map[[2]string]string
	patches    map[string]*patchState
	patchAfter int
	nextID     int
	busy       int
	active     map[[2]string]string
	deploys    map[[2]string][]depl
	// nextStatuses script the next redeploy's statuses (DeployStatuses).
	nextStatuses map[[2]string][]string
	monitor      monitorState
	history      historyState
	requests     map[string]Request
	counts       map[string]int
	partial      map[string][]string

	t  testing.TB
	ws wsState
}

type depl struct {
	id, status string
	at         time.Time
	later      []string // statuses of later reads (DeployStatuses)
}

// New starts a fake server, closed when the test ends.
func New(t testing.TB) *Fake {
	f := &Fake{
		PageSize: 100, TimestampPageSize: 500,
		vars: map[varKey]varEntry{}, system: map[varKey]string{}, failNext: map[string][]string{}, failVars: map[[2]string]string{},
		patches: map[string]*patchState{}, requests: map[string]Request{}, counts: map[string]int{}, partial: map[string][]string{}, active: map[[2]string]string{}, deploys: map[[2]string][]depl{}, nextStatuses: map[[2]string][]string{},
		t: t, ws: wsState{conns: map[*wsConn]bool{}},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { f.WSReleaseAcks(); f.WSResumeReads() })
	f.URL = srv.URL
	f.WSURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	return f
}

func (f *Fake) SetCatalog(c railway.Catalog) { f.mu.Lock(); f.catalog = c; f.mu.Unlock() }

// SetVar sets a variable; serviceID "" is shared.
func (f *Fake) SetVar(envID, serviceID, name, value string, sealed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vars[varKey{envID, serviceID, name}] = varEntry{value, sealed, time.Date(2026, 1, 2, 3, 4, 5, 678e6, time.UTC)}
}

// SetSystemVar sets a Railway system variable (RAILWAY_PRIVATE_DOMAIN and
// the like): it appears only in the rendered variables of its scope.
func (f *Fake) SetSystemVar(envID, serviceID, name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.system[varKey{envID, serviceID, name}] = value
}

func (f *Fake) Var(envID, serviceID, name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.vars[varKey{envID, serviceID, name}]
	return e.value, ok
}

// Mutations are the mutations served successfully, in order.
func (f *Fake) Mutations() []Mutation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Mutation(nil), f.mutations...)
}

// FailNext makes the next request of op answer a GraphQL error (no data).
func (f *Fake) FailNext(op string, err string) {
	f.mu.Lock()
	f.failNext[op] = append(f.failNext[op], err)
	f.mu.Unlock()
}

// FailVars makes the Discover/ReadVariables aliases of one scope ("" =
// shared) answer a partial error with msg and null data.
func (f *Fake) FailVars(envID, serviceID, msg string) {
	f.mu.Lock()
	f.failVars[[2]string{envID, serviceID}] = msg
	f.mu.Unlock()
}

// MaxInFlight is the highest number of requests being served at once.
func (f *Fake) MaxInFlight() int { return int(f.maxInflight.Load()) }

// FailPatches makes patches committed from now on end FAILED with msg,
// applying nothing ("" restores COMMITTED).
func (f *Fake) FailPatches(msg string) { f.mu.Lock(); f.patchFail = msg; f.mu.Unlock() }

// FailPatchesIn is FailPatches for the patches of one environment only
// ("" restores COMMITTED there).
func (f *Fake) FailPatchesIn(envID, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.envPatchFail == nil {
		f.envPatchFail = map[string]string{}
	}
	f.envPatchFail[envID] = msg
}

// PatchStatusAfter: a committed patch reports APPLYING n times, then COMMITTED.
func (f *Fake) PatchStatusAfter(n int) { f.mu.Lock(); f.patchAfter = n; f.mu.Unlock() }

// RedeployBusy: the next n ServiceDeployments answers list an extra
// in-progress (BUILDING) deployment.
func (f *Fake) RedeployBusy(n int) { f.mu.Lock(); f.busy = n; f.mu.Unlock() }

// DeployStatuses scripts the next redeploy of a service: its deployment
// reports statuses[0] in the first ServiceDeployments answer, the next
// status in each later one, and the last status from then on. Without it a
// redeploy's deployment is SUCCESS at once.
func (f *Fake) DeployStatuses(envID, serviceID string, statuses ...string) {
	f.mu.Lock()
	f.nextStatuses[[2]string{envID, serviceID}] = statuses
	f.mu.Unlock()
}

// AddDeployment lists an existing deployment of a service, newest first.
func (f *Fake) AddDeployment(envID, serviceID, id, status string, at time.Time) {
	f.mu.Lock()
	key := [2]string{envID, serviceID}
	f.deploys[key] = append(f.deploys[key], depl{id: id, status: status, at: at})
	f.mu.Unlock()
}

func (f *Fake) SetActiveDeployment(envID, serviceID, id string) {
	f.mu.Lock()
	f.active[[2]string{envID, serviceID}] = id
	f.mu.Unlock()
}

type gqlError struct {
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if isUpgrade(r) {
		f.serveWS(w, r)
		return
	}
	var req struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		m := f.maxInflight.Load()
		if n <= m || f.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	if f.Delay > 0 {
		time.Sleep(f.Delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var data any
	var errs []gqlError
	f.counts[req.OperationName]++
	if q := f.failNext[req.OperationName]; len(q) > 0 {
		f.failNext[req.OperationName] = q[1:]
		errs = []gqlError{{Message: q[0]}}
	} else {
		f.requests[req.OperationName] = Request{Doc: req.Query, Vars: req.Variables}
		data, errs = f.dispatch(req.OperationName, req.Query, req.Variables)
		if q := f.partial[req.OperationName]; len(q) > 0 {
			f.partial[req.OperationName] = q[1:]
			errs = append(errs, gqlError{Message: q[0]})
		}
	}
	out := map[string]any{"data": data}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func str(v any) string { s, _ := v.(string); return s }

func (f *Fake) dispatch(op, doc string, v map[string]any) (any, []gqlError) {
	switch op {
	case "WhoAmI":
		return map[string]any{"me": map[string]any{"id": "u1", "name": "Fake User", "email": "fake@example.test"}}, nil
	case "ListWorkspaces":
		var ws []map[string]string
		for _, name := range f.workspaces() {
			ws = append(ws, map[string]string{"id": "ws:" + name, "name": name})
		}
		return map[string]any{"me": map[string]any{"workspaces": ws}}, nil
	case "WorkspaceProjects":
		return f.workspaceProjects(str(v["workspaceId"]), str(v["after"])), nil
	case "ProjectEnvironment":
		for _, p := range f.catalog.Projects {
			if p.ID == str(v["projectId"]) {
				return map[string]any{"project": projectNode(p)}, nil
			}
		}
		return map[string]any{"project": nil}, []gqlError{{Message: "Project not found", Path: []any{"project"}}}
	case "VariablesPage":
		return f.variablesPage(str(v["environmentId"]), str(v["after"])), nil
	case "Discover":
		return f.discover(doc, v)
	case "ReadVariables":
		scope, errs := f.scope(str(v["environmentId"]), str(v["serviceId"]), v["unrendered"] == true, "variables")
		return map[string]any{"variables": scope}, errs
	case "CommitPatch":
		return f.commitPatch(doc, v)
	case "PatchStatus":
		p, ok := f.patches[str(v["id"])]
		if !ok {
			return map[string]any{"environmentPatch": nil}, []gqlError{{Message: "Patch not found", Path: []any{"environmentPatch"}}}
		}
		status, lastErr := "APPLYING", any(nil)
		if p.applying > 0 {
			p.applying--
		} else {
			status = f.settle(p)
			if p.failMsg != "" {
				lastErr = p.failMsg
			}
		}
		return map[string]any{"environmentPatch": map[string]any{"id": str(v["id"]), "status": status, "lastAppliedError": lastErr}}, nil
	case "Redeploy":
		f.record(op, v)
		key := [2]string{str(v["environmentId"]), str(v["serviceId"])}
		f.nextID++
		d := depl{id: fmt.Sprintf("dep-%d", f.nextID), status: "SUCCESS", at: time.Now().UTC()}
		if st := f.nextStatuses[key]; len(st) > 0 {
			d.status, d.later = st[0], st[1:]
			delete(f.nextStatuses, key)
		}
		f.deploys[key] = append([]depl{d}, f.deploys[key]...)
		return map[string]any{"serviceInstanceRedeploy": true}, nil
	case "ServiceDeployments":
		key := [2]string{str(v["environmentId"]), str(v["serviceId"])}
		list := append([]depl(nil), f.deploys[key]...)
		for i := range f.deploys[key] {
			if d := &f.deploys[key][i]; len(d.later) > 0 {
				d.status, d.later = d.later[0], d.later[1:]
			}
		}
		if f.busy > 0 {
			f.busy--
			list = append([]depl{{id: "dep-busy", status: "BUILDING", at: time.Now().UTC()}}, list...)
		}
		var edges []map[string]any
		for i, d := range list {
			if i == 10 {
				break
			}
			edges = append(edges, map[string]any{"node": map[string]any{"id": d.id, "status": d.status, "createdAt": d.at.Format(time.RFC3339Nano)}})
		}
		return map[string]any{"deployments": map[string]any{"edges": edges}}, nil
	case "ActiveDeployment":
		id := f.active[[2]string{str(v["environmentId"]), str(v["serviceId"])}]
		list := []map[string]string{}
		if id != "" {
			list = append(list, map[string]string{"id": id})
		}
		return map[string]any{"serviceInstance": map[string]any{"activeDeployments": list}}, nil
	case "RestartDeployment":
		f.record(op, v)
		return map[string]any{"deploymentRestart": true}, nil
	case "ServiceDomainCreate", "ServiceDomainDelete", "CustomDomainCreate", "CustomDomainDelete":
		return f.domainMutation(op, v)
	case "ServiceStates", "ServiceMetrics", "ServiceMetricsOf", "ServiceVolumes", "ServiceDetail", "Domains", "EnvironmentConfig", "DeploymentLogs":
		return f.monitorRead(op, v)
	case "EnvironmentHistory", "EnvironmentPatches", "ProjectMembers":
		return f.historyRead(op, v)
	}
	return nil, []gqlError{{Message: "unknown operation " + op}}
}

func (f *Fake) record(op string, v map[string]any) {
	f.mutations = append(f.mutations, Mutation{Op: op, Vars: v})
}

func (f *Fake) workspaces() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range f.catalog.Projects {
		if !seen[p.Workspace] {
			seen[p.Workspace] = true
			out = append(out, p.Workspace)
		}
	}
	return out
}

func projectNode(p railway.Project) map[string]any {
	var envs, svcs []map[string]any
	for _, e := range p.Environments {
		var inst []map[string]any
		for _, s := range e.ServiceIDs {
			inst = append(inst, map[string]any{"node": map[string]any{"serviceId": s}})
		}
		envs = append(envs, map[string]any{"node": map[string]any{
			"id": e.ID, "name": e.Name, "canAccess": e.CanAccess,
			"serviceInstances": map[string]any{"edges": inst},
		}})
	}
	for _, s := range p.Services {
		svcs = append(svcs, map[string]any{"node": map[string]any{"id": s.ID, "name": s.Name}})
	}
	return map[string]any{
		"id": p.ID, "name": p.Name,
		"environments": map[string]any{"edges": envs},
		"services":     map[string]any{"edges": svcs},
	}
}

func (f *Fake) workspaceProjects(wsID, after string) any {
	name := strings.TrimPrefix(wsID, "ws:")
	var mine []railway.Project
	for _, p := range f.catalog.Projects {
		if p.Workspace == name {
			mine = append(mine, p)
		}
	}
	start, _ := strconv.Atoi(after)
	end := min(start+f.PageSize, len(mine))
	if start > len(mine) {
		start = len(mine)
	}
	var edges []map[string]any
	for _, p := range mine[start:end] {
		edges = append(edges, map[string]any{"node": projectNode(p)})
	}
	return map[string]any{"workspace": map[string]any{"projects": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": end < len(mine), "endCursor": strconv.Itoa(end)},
		"edges":    edges,
	}}}
}

func (f *Fake) timestampPage(envID string, offset int) map[string]any {
	var keys []varKey
	for k := range f.vars {
		if k.env == envID {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].svc != keys[j].svc {
			return keys[i].svc < keys[j].svc
		}
		return keys[i].name < keys[j].name
	})
	offset = min(offset, len(keys))
	end := min(offset+f.TimestampPageSize, len(keys))
	var edges []map[string]any
	for _, k := range keys[offset:end] {
		e := f.vars[k]
		var svc any
		if k.svc != "" {
			svc = k.svc
		}
		edges = append(edges, map[string]any{"node": map[string]any{
			"name": k.name, "serviceId": svc, "updatedAt": e.updated.Format("2006-01-02T15:04:05.000Z"), "isSealed": e.sealed,
		}})
	}
	return map[string]any{
		"pageInfo": map[string]any{"hasNextPage": end < len(keys), "endCursor": strconv.Itoa(end)},
		"edges":    edges,
	}
}

func (f *Fake) variablesPage(envID, after string) any {
	off, _ := strconv.Atoi(after)
	return map[string]any{"environment": map[string]any{"variables": f.timestampPage(envID, off)}}
}

// known reports whether the catalog has the environment and, for a service
// scope, that service deployed in it.
func (f *Fake) known(envID, svc string) bool {
	for _, p := range f.catalog.Projects {
		for _, e := range p.Environments {
			if e.ID != envID {
				continue
			}
			if svc == "" {
				return true
			}
			for _, s := range e.ServiceIDs {
				if s == svc {
					return true
				}
			}
		}
	}
	return false
}

// scope returns one scope's variables (sealed ones null) or a partial error.
func (f *Fake) scope(envID, svc string, unrendered bool, alias string) (any, []gqlError) {
	if msg, ok := f.failVars[[2]string{envID, svc}]; ok {
		return nil, []gqlError{{Message: msg, Path: []any{alias}}}
	}
	if !f.known(envID, svc) {
		what := "Environment"
		if f.known(envID, "") {
			what = "Service"
		}
		return nil, []gqlError{{Message: what + " not found", Path: []any{alias}}}
	}
	out := map[string]any{}
	for k, e := range f.vars {
		if k.env == envID && k.svc == svc {
			if e.sealed {
				out[k.name] = nil
			} else if unrendered {
				out[k.name] = e.value
			} else {
				out[k.name] = f.render(envID, svc, e.value)
			}
		}
	}
	if !unrendered {
		for k, v := range f.system {
			if k.env == envID && k.svc == svc {
				out[k.name] = v
			}
		}
	}
	return out, nil
}

var refRE = regexp.MustCompile(`\$\{\{\s*([^}]*?)\s*\}\}`)

// render resolves ${{VAR}} (same service), ${{svc.VAR}} (service by name in
// the same environment) and ${{shared.VAR}}; unresolvable references stay.
// Targets are taken unrendered and unsealed (no recursion).
func (f *Fake) render(envID, svc, value string) string {
	return refRE.ReplaceAllStringFunc(value, func(ref string) string {
		inner := refRE.FindStringSubmatch(ref)[1]
		scope, name := svc, inner
		if a, b, ok := strings.Cut(inner, "."); ok {
			name = b
			if a == "shared" {
				scope = ""
			} else {
				scope = f.serviceID(envID, a)
				if scope == "" {
					return ref
				}
			}
		}
		if e, ok := f.vars[varKey{envID, scope, name}]; ok && !e.sealed {
			return e.value
		}
		return ref
	})
}

func (f *Fake) serviceID(envID, name string) string {
	for _, p := range f.catalog.Projects {
		for _, e := range p.Environments {
			if e.ID == envID {
				for _, s := range p.Services {
					if s.Name == name {
						return s.ID
					}
				}
			}
		}
	}
	return ""
}

var fieldRE = regexp.MustCompile(`(?m)^\s*(\w+): (environment|variables)\(([^)]*)\)`)

// discover serves the aliased Discover document by parsing its fields.
func (f *Fake) discover(doc string, v map[string]any) (any, []gqlError) {
	data := map[string]any{}
	var errs []gqlError
	for _, m := range fieldRE.FindAllStringSubmatch(doc, -1) {
		alias, kind := m[1], m[2]
		args := map[string]string{}
		for _, a := range strings.Split(m[3], ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(a), ": ")
			if !ok {
				continue
			}
			if name, isVar := strings.CutPrefix(val, "$"); isVar {
				val = str(v[name])
			}
			args[k] = val
		}
		if kind == "environment" {
			envID := args["id"]
			if !f.known(envID, "") {
				data[alias] = nil
				errs = append(errs, gqlError{Message: "Environment not found", Path: []any{alias}})
				continue
			}
			data[alias] = map[string]any{"variables": f.timestampPage(envID, 0)}
			continue
		}
		scope, e := f.scope(args["environmentId"], args["serviceId"], args["unrendered"] == "true", alias)
		data[alias] = scope
		errs = append(errs, e...)
	}
	return data, errs
}

// settle ends a patch: it applies its changes (COMMITTED) or applies nothing (FAILED).
func (f *Fake) settle(p *patchState) string {
	if p.failMsg != "" {
		return "FAILED"
	}
	if !p.done {
		p.done = true
		f.applyPatch(p.envID, p.patch)
	}
	return "COMMITTED"
}

func (f *Fake) applyPatch(envID string, patch map[string]any) {
	changed := f.applyConfigPatch(envID, patch)
	before := make(map[varKey]varEntry, len(f.vars))
	for k, e := range f.vars {
		before[k] = e
	}
	defer func() {
		for k, e := range f.vars {
			if o, ok := before[k]; !ok || o.value != e.value || o.sealed != e.sealed {
				changed = true
			}
		}
		for k := range before {
			if _, ok := f.vars[k]; !ok {
				changed = true
			}
		}
		if changed {
			f.bumpEtag(envID)
		}
	}()
	apply := func(svc string, changes map[string]any) {
		for name, c := range changes {
			k := varKey{envID, svc, name}
			if c == nil {
				delete(f.vars, k)
				continue
			}
			cm, _ := c.(map[string]any)
			old := f.vars[k]
			f.vars[k] = varEntry{str(cm["value"]), old.sealed, time.Now().UTC()}
		}
	}
	if shared, ok := patch["sharedVariables"].(map[string]any); ok {
		apply("", shared)
	}
	if svcs, ok := patch["services"].(map[string]any); ok {
		for id, s := range svcs {
			sm, _ := s.(map[string]any)
			if vs, ok := sm["variables"].(map[string]any); ok {
				apply(id, vs)
			}
		}
	}
}

// validPatch checks the shape applyPatch relies on.
func validPatch(patch map[string]any) string {
	checkVars := func(where string, changes any) string {
		m, ok := changes.(map[string]any)
		if !ok {
			return where + " must be an object"
		}
		for name, c := range m {
			if c == nil {
				continue
			}
			cm, ok := c.(map[string]any)
			if !ok {
				return where + "." + name + " must be {value} or null"
			}
			if _, ok := cm["value"].(string); !ok {
				return where + "." + name + ".value must be a string"
			}
		}
		return ""
	}
	if sv, ok := patch["sharedVariables"]; ok {
		if msg := checkVars("sharedVariables", sv); msg != "" {
			return msg
		}
	}
	if svcs, ok := patch["services"]; ok {
		sm, ok := svcs.(map[string]any)
		if !ok {
			return "services must be an object"
		}
		for id, s := range sm {
			m, ok := s.(map[string]any)
			if !ok {
				return "services." + id + " must be an object"
			}
			if vs, ok := m["variables"]; ok {
				if msg := checkVars("services."+id+".variables", vs); msg != "" {
					return msg
				}
			}
		}
	}
	return ""
}

func (f *Fake) commitPatch(doc string, v map[string]any) (any, []gqlError) {
	envID := str(v["environmentId"])
	patch, ok := v["patch"].(map[string]any)
	if !ok {
		return nil, []gqlError{{Message: "Invalid patch: must be an object", Path: []any{"environmentPatchCommit"}}}
	}
	if msg := validPatch(patch); msg != "" {
		return nil, []gqlError{{Message: "Invalid patch: " + msg, Path: []any{"environmentPatchCommit"}}}
	}
	f.nextID++
	id := fmt.Sprintf("patch-%d", f.nextID)
	p := &patchState{applying: f.patchAfter, envID: envID, patch: patch, failMsg: f.patchFail}
	if msg := f.envPatchFail[envID]; msg != "" {
		p.failMsg = msg
	}
	f.patches[id] = p
	if p.applying == 0 {
		f.settle(p)
	}
	f.record("CommitPatch", map[string]any{
		"environmentId": envID,
		"patch":         patch,
		"skipDeploys":   strings.Contains(doc, "skipDeploys: true"),
		"commitMessage": v["message"],
	})
	return map[string]any{"environmentPatchCommit": "commitChanges/" + envID + "/" + id}, nil
}

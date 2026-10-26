package railwaytest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/railway"
)

// Request is the last document and variables the fake served for an operation.
type Request struct {
	Doc  string
	Vars map[string]any
}

// LastRequest is the last successfully dispatched request of op.
func (f *Fake) LastRequest(op string) (Request, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.requests[op]
	return r, ok
}

// Requests is how many requests of op the fake received (failed ones included).
func (f *Fake) Requests(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[op]
}

type monitorState struct {
	states     map[string][]railway.DeploymentState
	metrics    map[string][]railway.MetricSeries
	volumes    map[string][]railway.VolumeMount
	details    map[[2]string]railway.ServiceDetail
	domains    map[[2]string]railway.Domains
	logs       map[string][]railway.LogLine
	configs    map[string]map[string]any // raw environment config
	etags      map[string]int
	fixedEtag  map[string]string
	instRegion map[[2]string]struct {
		region   string
		replicas int
	}
}

func (m *monitorState) init() {
	if m.states == nil {
		m.states = map[string][]railway.DeploymentState{}
		m.metrics = map[string][]railway.MetricSeries{}
		m.volumes = map[string][]railway.VolumeMount{}
		m.details = map[[2]string]railway.ServiceDetail{}
		m.domains = map[[2]string]railway.Domains{}
		m.logs = map[string][]railway.LogLine{}
		m.configs = map[string]map[string]any{}
		m.etags = map[string]int{}
	}
}

func (f *Fake) SetStates(projectID string, s []railway.DeploymentState) {
	f.mu.Lock()
	f.monitor.init()
	f.monitor.states[projectID] = s
	f.mu.Unlock()
}

func (f *Fake) SetMetrics(projectID string, m []railway.MetricSeries) {
	f.mu.Lock()
	f.monitor.init()
	f.monitor.metrics[projectID] = m
	f.mu.Unlock()
}

func (f *Fake) SetVolumes(projectID string, v []railway.VolumeMount) {
	f.mu.Lock()
	f.monitor.init()
	f.monitor.volumes[projectID] = v
	f.mu.Unlock()
}

func (f *Fake) SetDetail(envID, serviceID string, d railway.ServiceDetail) {
	f.mu.Lock()
	f.monitor.init()
	f.monitor.details[[2]string{envID, serviceID}] = d
	f.mu.Unlock()
}

// SetInstanceRegion makes ServiceDetail answer the instance's plain
// `region`/`numReplicas` (no multiRegionConfig in the deployment meta) when
// the detail has no Regions.
func (f *Fake) SetInstanceRegion(envID, serviceID, region string, replicas int) {
	f.mu.Lock()
	f.monitor.init()
	if f.monitor.instRegion == nil {
		f.monitor.instRegion = map[[2]string]struct {
			region   string
			replicas int
		}{}
	}
	f.monitor.instRegion[[2]string{envID, serviceID}] = struct {
		region   string
		replicas int
	}{region, replicas}
	f.mu.Unlock()
}

// PartialError makes the next request of op answer its data plus a GraphQL
// error with msg (an inaccessible alias).
func (f *Fake) PartialError(op, msg string) {
	f.mu.Lock()
	f.partial[op] = append(f.partial[op], msg)
	f.mu.Unlock()
}

// SetLogs sets a deployment's log lines, oldest first; DeploymentLogs
// answers the newest limit of them.
func (f *Fake) SetLogs(deploymentID string, lines []railway.LogLine) {
	f.mu.Lock()
	f.monitor.init()
	f.monitor.logs[deploymentID] = slices.Clone(lines)
	f.mu.Unlock()
}

func (f *Fake) SetDomains(envID, serviceID string, d railway.Domains) {
	f.mu.Lock()
	f.monitor.init()
	f.monitor.domains[[2]string{envID, serviceID}] = d
	f.mu.Unlock()
}

// SetEnvConfig stores the typed config (Etag included, "" = keep the
// counter's). A service with neither CPU nor memory keeps `limitOverride:
// {containers: null}`.
func (f *Fake) SetEnvConfig(envID string, c railway.EnvConfig) {
	raw := map[string]any{}
	svcs := map[string]any{}
	for id, s := range c.Services {
		deploy := map[string]any{}
		if s.CPU != nil || s.MemoryBytes != nil {
			cont := map[string]any{}
			if s.CPU != nil {
				cont["cpu"] = *s.CPU
			}
			if s.MemoryBytes != nil {
				cont["memoryBytes"] = float64(*s.MemoryBytes)
			}
			deploy["limitOverride"] = map[string]any{"containers": cont}
		} else {
			deploy["limitOverride"] = map[string]any{"containers": nil}
		}
		if len(s.Replicas) > 0 {
			mr := map[string]any{}
			for r, n := range s.Replicas {
				mr[r] = map[string]any{"numReplicas": float64(n)}
			}
			deploy["multiRegionConfig"] = mr
		}
		svcs[id] = map[string]any{"deploy": deploy}
	}
	raw["services"] = svcs
	vols := map[string]any{}
	for id, v := range c.Volumes {
		vols[id] = map[string]any{"sizeMB": float64(v.SizeMB), "allowOnlineResize": v.AllowOnlineResize}
	}
	raw["volumes"] = vols
	f.SetEnvConfigRaw(envID, raw, c.Etag)
}

// SetEnvConfigRaw stores a raw config (JSON-shaped: maps, float64, strings,
// bools, nil). etag "" keeps an auto-generated one.
func (f *Fake) SetEnvConfigRaw(envID string, config map[string]any, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monitor.init()
	f.monitor.configs[envID] = config
	if etag != "" {
		if f.monitor.fixedEtag == nil {
			f.monitor.fixedEtag = map[string]string{}
		}
		f.monitor.fixedEtag[envID] = etag
	} else {
		delete(f.monitor.fixedEtag, envID)
	}
}

func (f *Fake) etag(envID string) string {
	if e, ok := f.monitor.fixedEtag[envID]; ok {
		return e
	}
	return fmt.Sprintf("etag-%d", f.monitor.etags[envID])
}

func (f *Fake) bumpEtag(envID string) {
	delete(f.monitor.fixedEtag, envID)
	f.monitor.etags[envID]++
}

func objAt(m map[string]any, path ...string) map[string]any {
	for _, k := range path {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	return m
}

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// applyConfigPatch applies the config fragments of a patch as Railway does
// (seen against the live API): limitOverride null is a no-op, {containers:
// null} clears, {containers: {...}} sets; volumes.<id>.sizeMB sets. The etag
// moves only when the config does (applyPatch bumps it once, after variables
// too).
func (f *Fake) applyConfigPatch(envID string, patch map[string]any) bool {
	f.monitor.init()
	cfg := f.monitor.configs[envID]
	if cfg == nil {
		cfg = map[string]any{}
	}
	before := clone(cfg)
	if svcs, ok := patch["services"].(map[string]any); ok {
		for id, s := range svcs {
			sm, _ := s.(map[string]any)
			deploy, _ := sm["deploy"].(map[string]any)
			lo, ok := deploy["limitOverride"].(map[string]any)
			if !ok { // null (or absent): silently ignored
				continue
			}
			if _, has := lo["containers"]; !has {
				continue
			}
			over := objAt(cfg, "services", id, "deploy", "limitOverride")
			if cm, ok := lo["containers"].(map[string]any); ok {
				// A map merges key by key.
				cont := objAt(over, "containers")
				for k, val := range cm {
					cont[k] = val
				}
				continue
			}
			over["containers"] = lo["containers"]
		}
	}
	if vols, ok := patch["volumes"].(map[string]any); ok {
		for id, v := range vols {
			vm, _ := v.(map[string]any)
			if size, ok := vm["sizeMB"]; ok && size != nil {
				objAt(cfg, "volumes", id)["sizeMB"] = size
			}
		}
	}
	f.monitor.configs[envID] = cfg
	return !reflect.DeepEqual(before, cfg)
}

func edgesOf[T any](nodes []T) map[string]any {
	es := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		es = append(es, map[string]any{"node": n})
	}
	return map[string]any{"edges": es}
}

func (f *Fake) monitorRead(op string, v map[string]any) (any, []gqlError) {
	f.monitor.init()
	m := &f.monitor
	switch op {
	case "ServiceStates":
		states := m.states[str(v["projectId"])]
		byEnv := map[string][]map[string]any{}
		var envOrder []string
		var building []map[string]any
		for _, s := range states {
			if _, ok := byEnv[s.EnvID]; !ok {
				envOrder = append(envOrder, s.EnvID)
			}
			active := []map[string]any{}
			if s.ActiveID != "" {
				active = append(active, map[string]any{"id": s.ActiveID, "status": s.ActiveStatus})
			}
			var latest any
			if s.LatestStatus != "" {
				latest = map[string]any{"id": "latest-" + s.ServiceID, "status": s.LatestStatus}
			}
			byEnv[s.EnvID] = append(byEnv[s.EnvID], map[string]any{
				"serviceId": s.ServiceID, "activeDeployments": active, "latestDeployment": latest,
			})
			for _, id := range s.InProgress {
				building = append(building, map[string]any{"id": id, "status": "BUILDING", "serviceId": s.ServiceID, "environmentId": s.EnvID})
			}
		}
		var envs []map[string]any
		for _, id := range envOrder {
			envs = append(envs, map[string]any{"id": id, "serviceInstances": edgesOf(byEnv[id])})
		}
		return map[string]any{
			"project":     map[string]any{"environments": edgesOf(envs)},
			"deployments": edgesOf(building),
		}, nil
	case "ServiceMetrics", "ServiceMetricsOf":
		var out []map[string]any
		projects := []string{str(v["projectId"])}
		if op == "ServiceMetricsOf" {
			projects = projects[:0]
			for p := range m.metrics {
				projects = append(projects, p)
			}
			sort.Strings(projects)
		}
		for _, p := range projects {
			for _, s := range m.metrics[p] {
				if op == "ServiceMetricsOf" && (s.EnvID != str(v["environmentId"]) || s.ServiceID != str(v["serviceId"])) {
					continue
				}
				vals := []map[string]any{}
				for _, pt := range s.Points {
					vals = append(vals, map[string]any{"ts": pt.TS, "value": pt.Value})
				}
				out = append(out, map[string]any{
					"measurement": s.Measurement,
					"tags":        map[string]any{"serviceId": s.ServiceID, "environmentId": s.EnvID},
					"values":      vals,
				})
			}
		}
		return map[string]any{"metrics": out}, nil
	case "ServiceVolumes":
		var inst []map[string]any
		for _, vm := range m.volumes[str(v["projectId"])] {
			var svc any
			if vm.ServiceID != "" {
				svc = vm.ServiceID
			}
			inst = append(inst, map[string]any{
				"environmentId": vm.EnvID, "serviceId": svc, "mountPath": vm.MountPath,
				"sizeMB": vm.SizeMB, "currentSizeMB": vm.UsedMB, "volume": map[string]any{"id": vm.VolumeID},
			})
		}
		node := map[string]any{"volumeInstances": edgesOf(inst)}
		return map[string]any{"project": map[string]any{"volumes": edgesOf([]map[string]any{node})}}, nil
	case "ServiceDetail":
		d, ok := m.details[[2]string{str(v["environmentId"]), str(v["serviceId"])}]
		if !ok {
			return map[string]any{"serviceInstance": nil, "deployments": edgesOf([]any{})}, []gqlError{{Message: "ServiceInstance not found", Path: []any{"serviceInstance"}}}
		}
		mr := map[string]any{}
		for r, n := range d.Regions {
			mr[r] = map[string]any{"numReplicas": n}
		}
		orNil := func(s string) any {
			if s == "" {
				return nil
			}
			return s
		}
		var nextCron any
		if !d.NextCron.IsZero() {
			nextCron = d.NextCron.UTC().Format(time.RFC3339Nano)
		}
		var region, replicas any
		if r, ok := m.instRegion[[2]string{str(v["environmentId"]), str(v["serviceId"])}]; ok && len(d.Regions) == 0 {
			region, replicas = r.region, r.replicas
		}
		si := map[string]any{
			"region": region, "numReplicas": replicas, "healthcheckPath": orNil(d.Healthcheck), "sleepApplication": d.Sleeps,
			"restartPolicyType": orNil(d.RestartPolicy), "restartPolicyMaxRetries": d.RestartMaxRetries,
			"cronSchedule": orNil(d.Cron), "nextCronRunAt": nextCron, "builder": orNil(d.Builder),
			"source":            map[string]any{"image": orNil(d.Image), "repo": orNil(d.Repo)},
			"activeDeployments": []map[string]any{{"id": "active", "meta": map[string]any{"serviceManifest": map[string]any{"deploy": map[string]any{"multiRegionConfig": mr}}}}},
		}
		var deps []map[string]any
		for i, di := range d.Deploys {
			meta := map[string]any{"reason": di.Reason, "commitHash": di.CommitHash, "commitMessage": di.CommitMessage}
			if i == 0 && d.Branch != "" {
				meta["branch"] = d.Branch
			}
			deps = append(deps, map[string]any{
				"id": di.ID, "status": di.Status, "createdAt": di.CreatedAt.UTC().Format(time.RFC3339Nano),
				"creator": map[string]any{"name": orNil(di.Creator)}, "meta": meta,
			})
		}
		return map[string]any{"serviceInstance": si, "deployments": edgesOf(deps)}, nil
	case "Domains":
		d := m.domains[[2]string{str(v["environmentId"]), str(v["serviceId"])}]
		sd, cd := []map[string]any{}, []map[string]any{}
		for _, s := range d.Service {
			sd = append(sd, map[string]any{"id": s.ID, "domain": s.Domain, "targetPort": s.Port})
		}
		for _, c := range d.Custom {
			cd = append(cd, map[string]any{"id": c.ID, "domain": c.Domain, "targetPort": c.Port,
				"status": map[string]any{"verified": c.Verified, "certificateStatus": c.CertStatus}})
		}
		return map[string]any{"domains": map[string]any{"serviceDomains": sd, "customDomains": cd}}, nil
	case "DeploymentLogs":
		lines := m.logs[str(v["deploymentId"])]
		if n, ok := v["limit"].(float64); ok && int(n) < len(lines) {
			lines = lines[len(lines)-int(n):]
		}
		out := []map[string]any{}
		for _, l := range lines {
			out = append(out, map[string]any{"timestamp": l.Timestamp.Format(time.RFC3339Nano), "message": l.Message, "severity": l.Severity})
		}
		return map[string]any{"deploymentLogs": out}, nil
	case "EnvironmentConfig":
		env := str(v["environmentId"])
		cfg := m.configs[env]
		if cfg == nil {
			cfg = map[string]any{}
		}
		return map[string]any{"environment": map[string]any{"configEtag": f.etag(env), "config": clone(cfg)}}, nil
	}
	return nil, []gqlError{{Message: "unknown operation " + op}}
}

// domainMutation serves the four domain mutations against the same state
// Domains reads.
func (f *Fake) domainMutation(op string, v map[string]any) (any, []gqlError) {
	f.monitor.init()
	f.record(op, v)
	in, _ := v["input"].(map[string]any)
	key := [2]string{str(in["environmentId"]), str(in["serviceId"])}
	port := 0
	if p, ok := in["targetPort"].(float64); ok {
		port = int(p)
	}
	switch op {
	case "ServiceDomainCreate":
		f.nextID++
		d := f.monitor.domains[key]
		sd := railway.ServiceDomain{ID: fmt.Sprintf("sd-%d", f.nextID), Port: port}
		sd.Domain = fmt.Sprintf("%s-%d.up.railway.test", key[1], f.nextID)
		d.Service = append(d.Service, sd)
		f.monitor.domains[key] = d
		return map[string]any{"serviceDomainCreate": map[string]any{"id": sd.ID, "domain": sd.Domain, "targetPort": portOrNil(port)}}, nil
	case "CustomDomainCreate":
		f.nextID++
		d := f.monitor.domains[key]
		cd := railway.CustomDomain{ID: fmt.Sprintf("cd-%d", f.nextID), Domain: str(in["domain"]), Port: port, CertStatus: "VALIDATING_OWNERSHIP"}
		d.Custom = append(d.Custom, cd)
		f.monitor.domains[key] = d
		return map[string]any{"customDomainCreate": map[string]any{
			"id": cd.ID, "domain": cd.Domain, "targetPort": portOrNil(port),
			"status": map[string]any{"verified": cd.Verified, "certificateStatus": cd.CertStatus},
		}}, nil
	case "ServiceDomainDelete", "CustomDomainDelete":
		// Railway answers false for an id it does not know; so does the fake.
		id := str(v["id"])
		found := false
		for k, d := range f.monitor.domains {
			ns, nc := len(d.Service), len(d.Custom)
			d.Service = slices.DeleteFunc(slices.Clone(d.Service), func(s railway.ServiceDomain) bool { return op == "ServiceDomainDelete" && s.ID == id })
			d.Custom = slices.DeleteFunc(slices.Clone(d.Custom), func(c railway.CustomDomain) bool { return op == "CustomDomainDelete" && c.ID == id })
			found = found || len(d.Service) != ns || len(d.Custom) != nc
			f.monitor.domains[k] = d
		}
		return map[string]any{strings.ToLower(op[:1]) + op[1:]: found}, nil
	}
	return nil, []gqlError{{Message: "unknown operation " + op}}
}

func portOrNil(p int) any {
	if p == 0 {
		return nil
	}
	return p
}

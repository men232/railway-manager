package railway_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
)

func TestServiceStates(t *testing.T) {
	f := railwaytest.New(t)
	want := []railway.DeploymentState{
		{EnvID: "e1", ServiceID: "s1", ActiveID: "d1", ActiveStatus: "SUCCESS", LatestStatus: "BUILDING", InProgress: []string{"d2", "d3"}},
		{EnvID: "e1", ServiceID: "s2", LatestStatus: "CRASHED"},
		{EnvID: "e1", ServiceID: "s3"}, // no deployments: latestDeployment null
		{EnvID: "e2", ServiceID: "s1", ActiveID: "d9", ActiveStatus: "SUCCESS", LatestStatus: "SUCCESS"},
	}
	f.SetStates("p1", want)
	got, err := newClient(f).ServiceStates(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("states = %+v\nwant %+v", got, want)
	}
	req, _ := f.LastRequest("ServiceStates")
	if !strings.Contains(req.Doc, "QUEUED, WAITING, INITIALIZING, BUILDING, DEPLOYING") {
		t.Errorf("document lacks the in-progress statuses")
	}
}

var sixMeasurements = []string{"CPU_USAGE", "MEMORY_USAGE_GB", "NETWORK_RX_GB", "NETWORK_TX_GB", "CPU_LIMIT", "MEMORY_LIMIT_GB"}

func TestServiceMetricsShape(t *testing.T) {
	f := railwaytest.New(t)
	want := []railway.MetricSeries{
		{EnvID: "e1", ServiceID: "s1", Measurement: "CPU_USAGE", Points: []railway.MetricPoint{{TS: 100, Value: 0.5}, {TS: 130, Value: 0.75}}},
		{EnvID: "e1", ServiceID: "s1", Measurement: "MEMORY_LIMIT_GB", Points: []railway.MetricPoint{{TS: 100, Value: 8}}},
	}
	f.SetMetrics("p1", want)
	since := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	got, err := newClient(f).ServiceMetrics(context.Background(), "p1", since, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("series = %+v\nwant %+v", got, want)
	}
	req, _ := f.LastRequest("ServiceMetrics")
	for _, m := range sixMeasurements {
		if !strings.Contains(req.Doc, m) {
			t.Errorf("document lacks %s", m)
		}
	}
	if !strings.Contains(req.Doc, "groupBy: [SERVICE_ID, ENVIRONMENT_ID]") || !strings.Contains(req.Doc, "averagingWindowSeconds: $sampleRateSeconds") {
		t.Errorf("document lacks groupBy or averaging window:\n%s", req.Doc)
	}
	if req.Vars["sampleRateSeconds"] != float64(30) || req.Vars["projectId"] != "p1" || req.Vars["startDate"] != "2026-10-24T10:00:00Z" {
		t.Errorf("vars = %v", req.Vars)
	}
}

func TestServiceMetricsOfOneService(t *testing.T) {
	f := railwaytest.New(t)
	f.SetMetrics("p1", []railway.MetricSeries{
		{EnvID: "e1", ServiceID: "s1", Measurement: "CPU_USAGE", Points: []railway.MetricPoint{{TS: 1, Value: 1}}},
		{EnvID: "e1", ServiceID: "s2", Measurement: "CPU_USAGE", Points: []railway.MetricPoint{{TS: 2, Value: 2}}},
	})
	got, err := newClient(f).ServiceMetricsOf(context.Background(), "e1", "s2", time.Unix(0, 0), 300)
	if err != nil {
		t.Fatal(err)
	}
	want := []railway.MetricSeries{{EnvID: "e1", ServiceID: "s2", Measurement: "CPU_USAGE", Points: []railway.MetricPoint{{TS: 2, Value: 2}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("series = %+v", got)
	}
	req, _ := f.LastRequest("ServiceMetricsOf")
	if req.Vars["environmentId"] != "e1" || req.Vars["serviceId"] != "s2" || req.Vars["sampleRateSeconds"] != float64(300) {
		t.Errorf("vars = %v", req.Vars)
	}
	if strings.Contains(req.Doc, "groupBy") || strings.Contains(req.Doc, "projectId") {
		t.Errorf("one-service document must not group or name a project:\n%s", req.Doc)
	}
	for _, m := range sixMeasurements {
		if !strings.Contains(req.Doc, m) {
			t.Errorf("document lacks %s", m)
		}
	}
}

func TestServiceVolumes(t *testing.T) {
	f := railwaytest.New(t)
	want := []railway.VolumeMount{
		{EnvID: "e1", ServiceID: "s1", VolumeID: "v1", MountPath: "/data", SizeMB: 50000, UsedMB: 1234.5},
	}
	// An unmounted volume instance (no service) is dropped.
	f.SetVolumes("p1", append([]railway.VolumeMount{{EnvID: "e1", VolumeID: "v0", MountPath: "/x"}}, want...))
	got, err := newClient(f).ServiceVolumes(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("volumes = %+v\nwant %+v", got, want)
	}
}

func TestServiceDetailRegions(t *testing.T) {
	f := railwaytest.New(t)
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	want := railway.ServiceDetail{
		Regions: map[string]int{"us-west2": 2, "europe-west4": 1},
		Repo:    "acme/api", Image: "", Branch: "main", Builder: "RAILPACK", RestartPolicy: "ON_FAILURE", RestartMaxRetries: 5,
		Cron: "0 * * * *", Healthcheck: "/health", Sleeps: true, NextCron: at.Add(time.Hour),
		Deploys: []railway.DeployInfo{
			{ID: "d1", Status: "SUCCESS", Reason: "deploy", Creator: "Ann", CommitHash: "abc123", CommitMessage: "fix it", CreatedAt: at},
			{ID: "d2", Status: "REMOVED", CreatedAt: at.Add(-time.Hour)},
		},
	}
	f.SetDetail("e1", "s1", want)
	got, err := newClient(f).ServiceDetail(context.Background(), "e1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("detail = %+v\nwant %+v", got, want)
	}
	req, _ := f.LastRequest("ServiceDetail")
	if !strings.Contains(req.Doc, "deployments(first: 3") {
		t.Errorf("document must read the three newest deployments")
	}
}

func TestDomains(t *testing.T) {
	f := railwaytest.New(t)
	want := railway.Domains{
		Service: []railway.ServiceDomain{{ID: "sd1", Domain: "api.up.railway.app", Port: 8080}},
		Custom:  []railway.CustomDomain{{ID: "cd1", Domain: "api.example.test", Port: 8080, Verified: true, CertStatus: "VALID"}},
	}
	f.SetDomains("e1", "s1", want)
	got, err := newClient(f).Domains(context.Background(), "p1", "e1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("domains = %+v\nwant %+v", got, want)
	}
	req, _ := f.LastRequest("Domains")
	if req.Vars["projectId"] != "p1" || req.Vars["environmentId"] != "e1" || req.Vars["serviceId"] != "s1" {
		t.Errorf("vars = %v", req.Vars)
	}
}

func TestEnvironmentConfigParsesOnlyKnownKeys(t *testing.T) {
	f := railwaytest.New(t)
	f.SetEnvConfigRaw("e1", map[string]any{
		"sharedVariables": map[string]any{"SECRET": map[string]any{"value": "hunter2"}},
		"services": map[string]any{
			"s1": map[string]any{
				"variables": map[string]any{"A": map[string]any{"value": "1"}},
				"build":     map[string]any{"builder": "RAILPACK"},
				"deploy": map[string]any{
					"startCommand":  "run",
					"limitOverride": map[string]any{"containers": map[string]any{"cpu": 2.5, "memoryBytes": 4e9}},
					"multiRegionConfig": map[string]any{
						"us-west2": map[string]any{"numReplicas": 3.0},
					},
				},
			},
			"s2": map[string]any{"deploy": map[string]any{"limitOverride": map[string]any{"containers": nil}}},
			"s3": map[string]any{"variables": map[string]any{"B": map[string]any{"value": "2"}}},
		},
		"volumes": map[string]any{"v1": map[string]any{"sizeMB": 50000.0, "allowOnlineResize": true, "mount": "/data"}},
	}, "etag-x")
	got, err := newClient(f).EnvironmentConfig(context.Background(), "e1")
	if err != nil {
		t.Fatal(err)
	}
	cpu, mem := 2.5, int64(4_000_000_000)
	want := railway.EnvConfig{
		Etag: "etag-x",
		Services: map[string]railway.ServiceConfig{
			"s1": {CPU: &cpu, MemoryBytes: &mem, Replicas: map[string]int{"us-west2": 3}},
		},
		Volumes: map[string]railway.VolumeConfig{"v1": {SizeMB: 50000, AllowOnlineResize: true}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %+v\nwant %+v", got, want)
	}
	if s2, ok := got.Services["s2"]; ok && s2.CPU != nil {
		t.Errorf("{containers: null} must parse as no override, got %+v", s2)
	}
	req, _ := f.LastRequest("EnvironmentConfig")
	if !strings.Contains(req.Doc, "config(decryptVariables: false)") {
		t.Errorf("config must be read without decrypting variables")
	}
}

func TestFakeLimitOverrideNullIsNoop(t *testing.T) {
	f := railwaytest.New(t)
	c := newClient(f)
	ctx := context.Background()
	cpu, mem := 1.0, int64(2_000_000_000)
	f.SetEnvConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{"s1": {CPU: &cpu, MemoryBytes: &mem}}})
	commit := func(patch map[string]any) {
		t.Helper()
		id, err := c.CommitPatch(ctx, "e1", patch, "t")
		if err != nil {
			t.Fatal(err)
		}
		if st, _, err := c.PatchStatus(ctx, id); err != nil || st != "COMMITTED" {
			t.Fatalf("status = %q, %v", st, err)
		}
	}
	read := func() railway.EnvConfig {
		t.Helper()
		got, err := c.EnvironmentConfig(ctx, "e1")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	svc := func(lo any) map[string]any {
		return map[string]any{"services": map[string]any{"s1": map[string]any{"deploy": map[string]any{"limitOverride": lo}}}}
	}
	before := read()

	commit(svc(nil))
	after := read()
	if after.Services["s1"].CPU == nil || *after.Services["s1"].CPU != 1 {
		t.Fatalf("limitOverride: null must leave the override, got %+v", after.Services["s1"])
	}
	if after.Etag != before.Etag {
		t.Errorf("etag moved on a no-op: %q -> %q", before.Etag, after.Etag)
	}

	commit(svc(map[string]any{"containers": map[string]any{"cpu": 4.0, "memoryBytes": 8e9}}))
	set := read()
	if set.Services["s1"].CPU == nil || *set.Services["s1"].CPU != 4 || *set.Services["s1"].MemoryBytes != 8_000_000_000 {
		t.Fatalf("set = %+v", set.Services["s1"])
	}
	if set.Etag == after.Etag {
		t.Errorf("etag did not move on a change")
	}

	commit(svc(map[string]any{"containers": nil}))
	cleared := read()
	if _, ok := cleared.Services["s1"]; ok {
		t.Fatalf("{containers: null} must clear the override, got %+v", cleared.Services["s1"])
	}
	if cleared.Etag == set.Etag {
		t.Errorf("etag did not move on clear")
	}

	commit(map[string]any{"volumes": map[string]any{"v1": map[string]any{"sizeMB": 60000.0}}})
	if v := read().Volumes["v1"]; v.SizeMB != 60000 {
		t.Errorf("volume size = %+v", v)
	}
}

func TestServiceDetailInstanceRegionFallback(t *testing.T) {
	f := railwaytest.New(t)
	f.SetDetail("e1", "s1", railway.ServiceDetail{Builder: "NIXPACKS"})
	f.SetInstanceRegion("e1", "s1", "us-east4", 2)
	got, err := newClient(f).ServiceDetail(context.Background(), "e1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]int{"us-east4": 2}); !reflect.DeepEqual(got.Regions, want) {
		t.Fatalf("regions = %v, want %v", got.Regions, want)
	}
	f.SetInstanceRegion("e1", "s2", "us-east4", 0) // replicas unset: one
	f.SetDetail("e1", "s2", railway.ServiceDetail{})
	got, _ = newClient(f).ServiceDetail(context.Background(), "e1", "s2")
	if want := (map[string]int{"us-east4": 0}); !reflect.DeepEqual(got.Regions, want) {
		t.Fatalf("regions = %v, want %v", got.Regions, want)
	}
}

func TestMetricsSampleRateClamped(t *testing.T) {
	f := railwaytest.New(t)
	c := newClient(f)
	if _, err := c.ServiceMetrics(context.Background(), "p1", time.Unix(0, 0), 10); err != nil {
		t.Fatal(err)
	}
	if req, _ := f.LastRequest("ServiceMetrics"); req.Vars["sampleRateSeconds"] != float64(30) {
		t.Errorf("project sample rate = %v, want 30", req.Vars["sampleRateSeconds"])
	}
	if _, err := c.ServiceMetricsOf(context.Background(), "e1", "s1", time.Unix(0, 0), 1); err != nil {
		t.Fatal(err)
	}
	if req, _ := f.LastRequest("ServiceMetricsOf"); req.Vars["sampleRateSeconds"] != float64(30) {
		t.Errorf("service sample rate = %v, want 30", req.Vars["sampleRateSeconds"])
	}
}

func TestProjectReadsKeepPartialData(t *testing.T) {
	reads := []struct {
		op   string
		seed func(*railwaytest.Fake)
		call func(*railway.Client) (int, error)
	}{
		{"ServiceStates", func(f *railwaytest.Fake) {
			f.SetStates("p1", []railway.DeploymentState{{EnvID: "e1", ServiceID: "s1", ActiveID: "d1", ActiveStatus: "SUCCESS"}})
		}, func(c *railway.Client) (int, error) {
			v, err := c.ServiceStates(context.Background(), "p1")
			return len(v), err
		}},
		{"ServiceMetrics", func(f *railwaytest.Fake) {
			f.SetMetrics("p1", []railway.MetricSeries{{EnvID: "e1", ServiceID: "s1", Measurement: "CPU_USAGE"}})
		}, func(c *railway.Client) (int, error) {
			v, err := c.ServiceMetrics(context.Background(), "p1", time.Unix(0, 0), 30)
			return len(v), err
		}},
		{"ServiceVolumes", func(f *railwaytest.Fake) {
			f.SetVolumes("p1", []railway.VolumeMount{{EnvID: "e1", ServiceID: "s1", VolumeID: "v1"}})
		}, func(c *railway.Client) (int, error) {
			v, err := c.ServiceVolumes(context.Background(), "p1")
			return len(v), err
		}},
	}
	for _, r := range reads {
		t.Run(r.op, func(t *testing.T) {
			// One failing alias, the rest returned: data plus a PartialError.
			f := railwaytest.New(t)
			r.seed(f)
			f.PartialError(r.op, "Problem reading environment e2")
			n, err := r.call(newClient(f))
			var pe *railway.PartialError
			if n != 1 || !errors.As(err, &pe) || len(pe.Warnings) != 1 || !strings.Contains(pe.Warnings[0], "Problem reading") {
				t.Fatalf("n = %d, err = %v; want 1 item and a PartialError", n, err)
			}
			// "not found" is dropped: plain success.
			f = railwaytest.New(t)
			r.seed(f)
			f.PartialError(r.op, "Service not found")
			if n, err := r.call(newClient(f)); n != 1 || err != nil {
				t.Fatalf("not-found: n = %d, err = %v", n, err)
			}
			// No data at all: the GraphQL error.
			f = railwaytest.New(t)
			f.FailNext(r.op, "boom")
			var gqe *railway.GraphQLError
			if _, err := r.call(newClient(f)); !errors.As(err, &gqe) {
				t.Fatalf("no data: err = %v, want *GraphQLError", err)
			}
		})
	}
}

func TestFakeLimitOverrideMergesAndEmptyIsNoop(t *testing.T) {
	f := railwaytest.New(t)
	c := newClient(f)
	ctx := context.Background()
	cpu, mem := 1.0, int64(2_000_000_000)
	f.SetEnvConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{"s1": {CPU: &cpu, MemoryBytes: &mem}}})
	commit := func(lo any) railway.EnvConfig {
		t.Helper()
		patch := map[string]any{"services": map[string]any{"s1": map[string]any{"deploy": map[string]any{"limitOverride": lo}}}}
		id, err := c.CommitPatch(ctx, "e1", patch, "t")
		if err != nil {
			t.Fatal(err)
		}
		if st, _, err := c.PatchStatus(ctx, id); err != nil || st != "COMMITTED" {
			t.Fatalf("status = %q, %v", st, err)
		}
		got, err := c.EnvironmentConfig(ctx, "e1")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	start, _ := c.EnvironmentConfig(ctx, "e1")
	for name, lo := range map[string]any{"limitOverride {}": map[string]any{}, "containers {}": map[string]any{"containers": map[string]any{}}} {
		got := commit(lo)
		if got.Etag != start.Etag || got.Services["s1"].CPU == nil || *got.Services["s1"].CPU != 1 || *got.Services["s1"].MemoryBytes != mem {
			t.Errorf("%s must change nothing: %+v", name, got)
		}
	}
	// Only cpu given: memoryBytes survives (merge).
	got := commit(map[string]any{"containers": map[string]any{"cpu": 3.0}})
	if *got.Services["s1"].CPU != 3 || got.Services["s1"].MemoryBytes == nil || *got.Services["s1"].MemoryBytes != mem {
		t.Errorf("merge lost memoryBytes: %+v", got.Services["s1"])
	}
	if got.Etag == start.Etag {
		t.Errorf("etag did not move on a change")
	}
}

func TestFakeEtagMovesOnlyOnVariableChange(t *testing.T) {
	f := railwaytest.New(t)
	c := newClient(f)
	ctx := context.Background()
	f.SetEnvConfigRaw("e1", map[string]any{}, "")
	etag := func() string {
		got, err := c.EnvironmentConfig(ctx, "e1")
		if err != nil {
			t.Fatal(err)
		}
		return got.Etag
	}
	commit := func(value string) {
		t.Helper()
		patch := map[string]any{"services": map[string]any{"s1": map[string]any{"variables": map[string]any{"A": map[string]any{"value": value}}}}}
		id, err := c.CommitPatch(ctx, "e1", patch, "t")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.PatchStatus(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	e0 := etag()
	commit("1")
	e1 := etag()
	if e1 == e0 {
		t.Errorf("etag did not move on a new variable")
	}
	commit("1") // same value: nothing changed
	if etag() != e1 {
		t.Errorf("etag moved although the variable did not change")
	}
	commit("2")
	if etag() == e1 {
		t.Errorf("etag did not move on a changed value")
	}
}

func TestDeploymentLogs(t *testing.T) {
	f := railwaytest.New(t)
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	var lines []railway.LogLine
	for i := range 5 {
		lines = append(lines, railway.LogLine{Timestamp: at.Add(time.Duration(i) * time.Second), Severity: "info", Message: "line " + string(rune('a'+i))})
	}
	f.SetLogs("dep-1", lines)
	got, err := newClient(f).DeploymentLogs(context.Background(), "dep-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, lines[2:]) {
		t.Fatalf("logs = %+v\nwant %+v", got, lines[2:])
	}
	req, _ := f.LastRequest("DeploymentLogs")
	if req.Vars["deploymentId"] != "dep-1" || req.Vars["limit"] != float64(3) {
		t.Errorf("vars = %v", req.Vars)
	}
	if !strings.Contains(railway.DeploymentLogsSubscription, "subscription") ||
		!strings.Contains(railway.DeploymentLogsSubscription, "deploymentLogs(deploymentId: $deploymentId, limit: $limit)") {
		t.Errorf("subscription document = %q", railway.DeploymentLogsSubscription)
	}
}

func TestDecodeDeploymentLogs(t *testing.T) {
	got, err := railway.DecodeDeploymentLogs([]byte(`{"deploymentLogs":[
		{"timestamp":"2026-10-24T12:00:01.5Z","message":"b","severity":"error"},
		{"timestamp":"2026-10-24T12:00:00Z","message":"a","severity":"info"},
		{"timestamp":"not a time","message":"c","severity":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []railway.LogLine{
		{Timestamp: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), Severity: "info", Message: "a"},
		{Timestamp: time.Date(2026, 10, 24, 12, 0, 1, 5e8, time.UTC), Severity: "error", Message: "b"},
		{Message: "c"},
	}
	// sorted by time, stable; an unreadable time sorts as zero, first
	want = append(want[2:], want[:2]...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %+v\nwant %+v", got, want)
	}
	if _, err := railway.DecodeDeploymentLogs([]byte(`{`)); err == nil {
		t.Error("malformed data decoded")
	}
}

package view_test

import (
	"slices"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/monitor"
	"github.com/men232/railway-manager/internal/railway"
)

func pts(vals ...float64) []railway.MetricPoint {
	out := make([]railway.MetricPoint, len(vals))
	for i, v := range vals {
		out[i] = railway.MetricPoint{TS: int64(30 * (i + 1)), Value: v}
	}
	return out
}

func series(svc, m string, p []railway.MetricPoint) railway.MetricSeries {
	return railway.MetricSeries{EnvID: "e1", ServiceID: svc, Measurement: m, Points: p}
}

func TestMonitorRowsStatusAndSeries(t *testing.T) {
	f := newFx(t)
	f.st.ReplaceStates("p1", []string{"e1"}, []railway.DeploymentState{
		{EnvID: "e1", ServiceID: "api", ActiveID: "d1", ActiveStatus: "SUCCESS"},
		{EnvID: "e1", ServiceID: "db", ActiveID: "d2", ActiveStatus: "SLEEPING"},
	})
	// "sleeps when idle" is a setting, not the state: api still runs
	f.st.SetDetail("e1", "api", railway.ServiceDetail{Sleeps: true})
	rx, tx := pts(0.001, 0.002), pts(0.0005, 0.0005)
	f.st.ReplaceMetrics([]string{"e1"}, []railway.MetricSeries{
		series("api", "CPU_USAGE", []railway.MetricPoint{{TS: 60, Value: 2}, {TS: 30, Value: 1}}),
		series("api", "MEMORY_USAGE_GB", pts(0.5)),
		series("api", "CPU_LIMIT", pts(4, 8)),
		series("api", "MEMORY_LIMIT_GB", pts(16)),
		series("api", "NETWORK_RX_GB", rx), series("api", "NETWORK_TX_GB", tx),
	})
	f.set(t, "api", "PLAIN", "two")
	cpu := 2.0
	mem := int64(2e9)
	f.st.SetConfig("e1", railway.EnvConfig{})
	lc, err := kinds.SetLimits(f.st, pl("api"), &cpu, &mem, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(lc, nil); err != nil {
		t.Fatal(err)
	}

	rows := f.v.Monitor(f.cs, []string{"e1"})
	by := map[string]int{}
	for i, r := range rows {
		by[r.Service] = i
	}
	api, db, web := rows[by["api"]], rows[by["db"]], rows[by["web"]]
	if api.Status.Kind != monitor.Online || db.Status.Kind != monitor.Sleeping || web.Status.Kind != monitor.Offline {
		t.Errorf("statuses = %v %v %v", api.Status.Kind, db.Status.Kind, web.Status.Kind)
	}
	if !slices.Equal(api.CPU, []float64{1, 2}) || !slices.Equal(api.Mem, []float64{0.5}) {
		t.Errorf("cpu %v mem %v", api.CPU, api.Mem)
	}
	if want := monitor.Rate(rx, tx); !slices.Equal(api.Net, want) || len(want) != 2 {
		t.Errorf("net %v, want %v", api.Net, want)
	}
	if api.CPULimit != 8 || api.MemLimitGB != 16 {
		t.Errorf("limits %v %v", api.CPULimit, api.MemLimitGB)
	}
	if api.Pending != 2 || api.LimitsOverlay == "" || db.LimitsOverlay != "" {
		t.Errorf("pending %d overlay %q / %q", api.Pending, api.LimitsOverlay, db.LimitsOverlay)
	}
}

func TestMonitorStatusBeforeFirstRead(t *testing.T) {
	f := newFx(t)
	for _, r := range f.v.Monitor(f.cs, []string{"e1"}) {
		if r.Status.Kind != monitor.Reading {
			t.Errorf("%s status = %v, want reading", r.Service, r.Status.Kind)
		}
	}
}

func TestMonitorRecomputesOnMetricsRev(t *testing.T) {
	f := newFx(t)
	envs := []string{"e1"}
	f.st.ReplaceMetrics(envs, []railway.MetricSeries{series("api", "CPU_USAGE", pts(1))})
	cpuOf := func() []float64 {
		for _, r := range f.v.Monitor(f.cs, envs) {
			if r.Service == "api" {
				return r.CPU
			}
		}
		return nil
	}
	if got := cpuOf(); !slices.Equal(got, []float64{1}) {
		t.Fatalf("cpu = %v", got)
	}
	f.st.ReplaceMetrics(envs, []railway.MetricSeries{series("api", "CPU_USAGE", pts(1, 3))})
	if got := cpuOf(); !slices.Equal(got, []float64{1, 3}) {
		t.Errorf("after metrics rev cpu = %v, want [1 3]", got)
	}
	f.st.ReplaceStates("p1", envs, []railway.DeploymentState{{EnvID: "e1", ServiceID: "api", ActiveStatus: "SLEEPING"}})
	for _, r := range f.v.Monitor(f.cs, envs) {
		if r.Service == "api" && r.Status.Kind != monitor.Sleeping {
			t.Errorf("status = %v, want sleeping after the states rev", r.Status.Kind)
		}
	}
}

func TestDetailChartsPerWindow(t *testing.T) {
	f := newFx(t)
	p := pl("api")
	f.st.SetPanelSeries(p, 60, time.Hour, []railway.MetricSeries{
		series("api", "CPU_USAGE", pts(1, 3, 2)), series("api", "CPU_LIMIT", pts(4)),
		series("api", "MEMORY_USAGE_GB", pts(0.5)),
	})
	f.st.SetPanelSeries(p, 300, time.Hour, []railway.MetricSeries{series("api", "CPU_USAGE", pts(9))})
	d1 := f.v.Detail(f.cs, p, 60, time.Hour)
	d6 := f.v.Detail(f.cs, p, 300, time.Hour)
	cpu1, cpu6 := d1.Charts[0], d6.Charts[0]
	if cpu1.Name != "cpu" || !slices.Equal(cpu1.Values, []float64{1, 3, 2}) || cpu1.Peak != 3 || cpu1.Last != 2 || cpu1.Limit != 4 || !cpu1.HasData {
		t.Errorf("1h cpu = %+v", cpu1)
	}
	if !slices.Equal(cpu6.Values, []float64{9}) || cpu6.Limit != 0 || d6.Step != 300 {
		t.Errorf("6h cpu = %+v", cpu6)
	}
	if net := d1.Charts[2]; net.Name != "net" || net.HasData {
		t.Errorf("net = %+v", net)
	}
	if d1.Charts[1].Name != "mem" || d1.Charts[1].Peak != 0.5 {
		t.Errorf("mem = %+v", d1.Charts[1])
	}
	// switching back must not be served the 6h result
	if again := f.v.Detail(f.cs, p, 60, time.Hour); again.Charts[0].Peak != 3 {
		t.Errorf("1h again = %+v", again.Charts[0])
	}
	f.st.SetPanelSeries(p, 60, time.Hour, []railway.MetricSeries{series("api", "CPU_USAGE", pts(7))})
	if got := f.v.Detail(f.cs, p, 60, time.Hour).Charts[0].Peak; got != 7 {
		t.Errorf("after panel rev peak = %v, want 7", got)
	}
	// another span of the same step is another series
	if d := f.v.Detail(f.cs, p, 60, 2*time.Hour); d.Charts[0].HasData || d.Span != 2*time.Hour {
		t.Errorf("60s/2h = %+v", d.Charts[0])
	}
	f.st.SetPanelSeries(p, 60, 2*time.Hour, []railway.MetricSeries{series("api", "CPU_USAGE", pts(5))})
	if got := f.v.Detail(f.cs, p, 60, 2*time.Hour).Charts[0].Peak; got != 5 {
		t.Errorf("60s/2h peak = %v, want 5", got)
	}
}

func TestDetailPendingLimits(t *testing.T) {
	f := newFx(t)
	p := pl("api")
	cpu, mem := 1.0, int64(1e9)
	f.st.SetConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{"api": {CPU: &cpu, MemoryBytes: &mem}}})
	d := f.v.Detail(f.cs, p, 60, time.Hour)
	if !d.Limits.Override || d.Limits.CPU != 1 || d.Limits.MemBytes != 1e9 || d.Limits.Pending != nil {
		t.Fatalf("limits = %+v", d.Limits)
	}
	ncpu, nmem := 2.0, int64(2e9)
	c, err := kinds.SetLimits(f.st, p, &ncpu, &nmem, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(c, nil); err != nil {
		t.Fatal(err)
	}
	d = f.v.Detail(f.cs, p, 60, time.Hour)
	want := f.v.Reg[c.Kind].Overlay(c)
	if d.Limits.Pending == nil || *d.Limits.Pending != want {
		t.Errorf("pending = %+v, want %+v", d.Limits.Pending, want)
	}
	if d.Limits.CPU != 1 {
		t.Errorf("current limits changed to %v", d.Limits.CPU)
	}
	if other := f.v.Detail(f.cs, pl("db"), 60, time.Hour); other.Limits.Pending != nil || other.Limits.Override {
		t.Errorf("db limits = %+v", other.Limits)
	}
}

func TestDetailPendingVolume(t *testing.T) {
	f := newFx(t)
	p := pl("db")
	f.st.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{{EnvID: "e1", ServiceID: "db", VolumeID: "v1", MountPath: "/data", SizeMB: 5000, UsedMB: 100}})
	f.st.SetConfig("e1", railway.EnvConfig{Volumes: map[string]railway.VolumeConfig{"v1": {SizeMB: 5000}}})
	c, err := kinds.ResizeVolume(f.st, p, "v1", 10000, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cs.Put(c, nil); err != nil {
		t.Fatal(err)
	}
	d := f.v.Detail(f.cs, p, 60, time.Hour)
	if len(d.Volumes) != 1 || d.Volumes[0].MountPath != "/data" || d.Volumes[0].Pending == nil || d.Volumes[0].Pending.Tone != "warn" {
		t.Errorf("volumes = %+v", d.Volumes)
	}
}

func TestDetailPendingDomainAddAndDelete(t *testing.T) {
	f := newFx(t)
	p := pl("api")
	f.st.SetDomains(p, railway.Domains{
		Service: []railway.ServiceDomain{{ID: "sd1", Domain: "api.up.railway.test", Port: 8080}},
		Custom:  []railway.CustomDomain{{ID: "cd1", Domain: "old.example.com", Port: 80, Verified: true}},
	})
	d := f.v.Detail(f.cs, p, 60, time.Hour)
	if len(d.Domains) != 2 || d.Domains[0].Custom || !d.Domains[1].Custom || !d.Domains[1].Verified || d.Domains[1].ID != "cd1" {
		t.Fatalf("domains = %+v", d.Domains)
	}
	put := func(c changeset.Change, err error) changeset.Change {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.cs.Put(c, nil); err != nil {
			t.Fatal(err)
		}
		return c
	}
	add := put(kinds.AddCustomDomain(f.st, p, "New.Example.com", 8080, now))
	del := put(kinds.DeleteCustomDomain(f.st, p, "old.example.com", now))
	sdel := put(kinds.DeleteServiceDomain(f.st, p, "sd1", now))
	sadd := put(kinds.AddServiceDomain(p, 0, now), nil)

	d = f.v.Detail(f.cs, p, 60, time.Hour)
	if len(d.Domains) != 4 {
		t.Fatalf("domains = %+v", d.Domains)
	}
	ov := func(c changeset.Change) string { return f.v.Reg[c.Kind].Overlay(c).Short }
	if r := d.Domains[0]; r.Host != "api.up.railway.test" || r.Pending == nil || r.Pending.Short != ov(sdel) {
		t.Errorf("service delete row = %+v", r)
	}
	if r := d.Domains[1]; r.Host != "old.example.com" || r.Pending == nil || r.Pending.Short != ov(del) {
		t.Errorf("custom delete row = %+v", r)
	}
	var gotAdd, gotSvcAdd bool
	for _, r := range d.Domains[2:] {
		if r.Custom && r.Host == "new.example.com" && r.ID == "" && r.Pending != nil && r.Pending.Short == ov(add) {
			gotAdd = true
		}
		if !r.Custom && r.Host == "" && r.Pending != nil && r.Pending.Short == ov(sadd) {
			gotSvcAdd = true
		}
	}
	if !gotAdd || !gotSvcAdd {
		t.Errorf("pending add rows missing: %+v", d.Domains)
	}
}

func TestDetailRecomputesOnEachCollection(t *testing.T) {
	f := newFx(t)
	p := pl("api")
	f.v.Detail(f.cs, p, 60, time.Hour)
	f.st.SetDomains(p, railway.Domains{Service: []railway.ServiceDomain{{ID: "s", Domain: "a.test"}}})
	if d := f.v.Detail(f.cs, p, 60, time.Hour); len(d.Domains) != 1 {
		t.Errorf("domains not refreshed: %+v", d.Domains)
	}
	f.st.SetDetail("e1", "api", railway.ServiceDetail{Image: "img"})
	if d := f.v.Detail(f.cs, p, 60, time.Hour); !d.HasInfo || d.Info.Image != "img" {
		t.Errorf("info not refreshed: %+v", d.Info)
	}
	f.st.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{{EnvID: "e1", ServiceID: "api", VolumeID: "v", MountPath: "/x"}})
	if d := f.v.Detail(f.cs, p, 60, time.Hour); len(d.Volumes) != 1 {
		t.Errorf("volumes not refreshed")
	}
	cpu := 3.0
	f.st.SetConfig("e1", railway.EnvConfig{Services: map[string]railway.ServiceConfig{"api": {CPU: &cpu}}})
	if d := f.v.Detail(f.cs, p, 60, time.Hour); d.Limits.CPU != 3 {
		t.Errorf("config not refreshed")
	}
}

// The domains screen needs each row's change (to drop or check it), a
// pending add's port and a custom domain's certificate status.
func TestDetailDomainChangeIDsPortsAndCerts(t *testing.T) {
	f := newFx(t)
	p := pl("api")
	f.st.SetDomains(p, railway.Domains{
		Service: []railway.ServiceDomain{{ID: "sd1", Domain: "api.up.railway.test", Port: 8080}},
		Custom:  []railway.CustomDomain{{ID: "cd1", Domain: "old.example.com", Port: 80, CertStatus: "CERTIFICATE_STATUS_TYPE_VALID"}},
	})
	add, err := kinds.AddCustomDomain(f.st, p, "new.example.com", 3000, now)
	if err != nil {
		t.Fatal(err)
	}
	sdel, err := kinds.DeleteServiceDomain(f.st, p, "sd1", now)
	if err != nil {
		t.Fatal(err)
	}
	sadd := kinds.AddServiceDomain(p, 9000, now)
	for _, c := range []changeset.Change{add, sdel, sadd} {
		if err := f.cs.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	d := f.v.Detail(f.cs, p, 60, time.Hour)
	if len(d.Domains) != 4 {
		t.Fatalf("domains = %+v", d.Domains)
	}
	if r := d.Domains[0]; r.ChangeID != sdel.ID {
		t.Errorf("service delete row ChangeID = %q, want %q", r.ChangeID, sdel.ID)
	}
	if r := d.Domains[1]; r.ChangeID != "" || r.CertStatus != "CERTIFICATE_STATUS_TYPE_VALID" {
		t.Errorf("custom row = %+v", r)
	}
	for _, r := range d.Domains[2:] {
		switch {
		case r.Custom && (r.ChangeID != add.ID || r.Port != 3000):
			t.Errorf("custom add row = %+v", r)
		case !r.Custom && (r.ChangeID != sadd.ID || r.Port != 9000):
			t.Errorf("service add row = %+v", r)
		}
	}
}

// Monitor builds its rows once per revision: the Services screen reads them
// for its note and its view every frame.
func TestMonitorMemoizedPerRevision(t *testing.T) {
	f := newFx(t)
	envs := []string{"e1"}
	builds := func(want int, what string) {
		t.Helper()
		if got := f.v.MonitorBuilds(); got != want {
			t.Errorf("%s: builds = %d, want %d", what, got, want)
		}
	}
	a := f.v.Monitor(f.cs, envs)
	b := f.v.Monitor(f.cs, slices.Clone(envs))
	builds(1, "two reads")
	if len(a) == 0 || &a[0] != &b[0] {
		t.Error("the second read did not return the cached rows")
	}
	f.st.ReplaceVariables("p1", vars()) // a collection the rows do not read
	f.v.Monitor(f.cs, envs)
	builds(1, "variables rev")
	f.st.ReplaceMetrics(envs, []railway.MetricSeries{series("api", "CPU_USAGE", pts(1))})
	f.v.Monitor(f.cs, envs)
	builds(2, "metrics rev")
	f.st.ReplaceStates("p1", envs, nil)
	f.v.Monitor(f.cs, envs)
	builds(3, "deploys rev")
	f.set(t, "api", "PLAIN", "two")
	f.v.Monitor(f.cs, envs)
	f.v.Monitor(f.cs, envs)
	builds(4, "change set rev")
	f.v.Monitor(f.cs, nil)
	builds(5, "env set")
	f.v.Monitor(f.cs, nil)
	builds(5, "same env set")
}

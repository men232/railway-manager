package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
)

var place = domain.Place{ProjectID: "p", EnvID: "e1", ServiceID: "s1"}

func monitorCollections() []Collection {
	return []Collection{Deploys, Metrics, Volumes, Details, Configs, DomainsCol, PanelMetrics}
}

func TestMonitorRevisions(t *testing.T) {
	muts := map[Collection]func(*Store){
		Deploys:      func(s *Store) { s.ReplaceStates("p", []string{"e1"}, nil) },
		Metrics:      func(s *Store) { s.ReplaceMetrics([]string{"e1"}, nil) },
		Volumes:      func(s *Store) { s.ReplaceVolumes([]string{"e1"}, nil) },
		Details:      func(s *Store) { s.SetDetail("e1", "s1", railway.ServiceDetail{}) },
		Configs:      func(s *Store) { s.SetConfig("e1", railway.EnvConfig{}) },
		DomainsCol:   func(s *Store) { s.SetDomains(place, railway.Domains{}) },
		PanelMetrics: func(s *Store) { s.SetPanelSeries(place, 60, time.Hour, nil) },
	}
	for _, c := range monitorCollections() {
		s := New()
		before := s.revs
		muts[c](s)
		for i := Collection(0); i < collectionCount; i++ {
			want := before[i]
			if i == c {
				want++
			}
			if s.Rev(i) != want {
				t.Errorf("mutator of %d: rev of %d = %d, want %d", c, i, s.Rev(i), want)
			}
		}
	}
}

func TestReplaceStatesMarksUnlistedRead(t *testing.T) {
	s := New()
	if s.StatesRead("e1") {
		t.Fatal("read before any replace")
	}
	s.ReplaceStates("p", []string{"e1", "e2"}, []railway.DeploymentState{
		{EnvID: "e1", ServiceID: "s1", ActiveStatus: "SUCCESS", InProgress: []string{"d1"}},
	})
	if !s.StatesRead("e1") || !s.StatesRead("e2") || s.StatesRead("e3") {
		t.Errorf("StatesRead e1/e2/e3 = %v/%v/%v", s.StatesRead("e1"), s.StatesRead("e2"), s.StatesRead("e3"))
	}
	if d, ok := s.State("e1", "s1"); !ok || d.ActiveStatus != "SUCCESS" {
		t.Errorf("State e1/s1 = %+v, %v", d, ok)
	}
	if _, ok := s.State("e1", "s2"); ok {
		t.Error("unlisted instance reported as present")
	}
	// A second read of e1 drops what is no longer listed; e2 is untouched.
	s.ReplaceStates("p", []string{"e1"}, nil)
	if _, ok := s.State("e1", "s1"); ok {
		t.Error("stale state kept")
	}
	if !s.StatesRead("e1") {
		t.Error("e1 no longer read")
	}
}

func TestReplaceScopedToEnvs(t *testing.T) {
	s := New()
	s.ReplaceMetrics([]string{"e1", "e2"}, []railway.MetricSeries{
		{EnvID: "e1", ServiceID: "s1", Measurement: "CPU", Points: []railway.MetricPoint{{TS: 1, Value: 1}}},
		{EnvID: "e2", ServiceID: "s1", Measurement: "CPU", Points: []railway.MetricPoint{{TS: 2, Value: 2}}},
	})
	s.ReplaceMetrics([]string{"e1"}, nil)
	if len(s.Series("e1", "s1", "CPU")) != 0 || len(s.Series("e2", "s1", "CPU")) != 1 {
		t.Error("metrics replace leaked across environments")
	}
	s.ReplaceVolumes([]string{"e1", "e2"}, []railway.VolumeMount{{EnvID: "e1", ServiceID: "s1", VolumeID: "v1"}, {EnvID: "e2", ServiceID: "s1", VolumeID: "v2"}})
	s.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{{EnvID: "e1", ServiceID: "s1", VolumeID: "v3"}})
	if v := s.Volumes("e1", "s1"); len(v) != 1 || v[0].VolumeID != "v3" {
		t.Errorf("e1 volumes = %+v", v)
	}
	if v := s.Volumes("e2", "s1"); len(v) != 1 || v[0].VolumeID != "v2" {
		t.Errorf("e2 volumes = %+v", v)
	}
}

func TestPanelSeriesPerStepAndSpan(t *testing.T) {
	s := New()
	pt := func(v float64) []railway.MetricPoint { return []railway.MetricPoint{{TS: 1, Value: v}} }
	s.SetPanelSeries(place, 60, time.Hour, []railway.MetricSeries{{Measurement: "CPU", Points: pt(1)}})
	s.SetPanelSeries(place, 300, time.Hour, []railway.MetricSeries{{Measurement: "CPU", Points: pt(6)}})
	s.SetPanelSeries(place, 60, 2*time.Hour, []railway.MetricSeries{{Measurement: "CPU", Points: pt(3)}})
	if got := s.PanelSeries(place, 60, time.Hour, "CPU"); len(got) != 1 || got[0].Value != 1 {
		t.Errorf("60s/1h = %v", got)
	}
	if got := s.PanelSeries(place, 60, 2*time.Hour, "CPU"); len(got) != 1 || got[0].Value != 3 {
		t.Errorf("60s/2h = %v", got)
	}
	if got := s.PanelSeries(place, 300, time.Hour, "CPU"); len(got) != 1 || got[0].Value != 6 {
		t.Errorf("300s/1h = %v", got)
	}
	if s.PanelSeries(place, 900, time.Hour, "CPU") != nil || s.PanelSeries(place, 300, 2*time.Hour, "CPU") != nil {
		t.Error("unset step or span has points")
	}
}

func fullStore() *Store {
	s := New()
	cpu, mem := 2.0, int64(1<<30)
	s.ReplaceStates("p", []string{"e1"}, []railway.DeploymentState{{EnvID: "e1", ServiceID: "s1", ActiveStatus: "SUCCESS", InProgress: []string{"d1"}}})
	s.ReplaceMetrics([]string{"e1"}, []railway.MetricSeries{{EnvID: "e1", ServiceID: "s1", Measurement: "CPU", Points: []railway.MetricPoint{{TS: 1, Value: 1}}}})
	s.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{{EnvID: "e1", ServiceID: "s1", VolumeID: "v1", SizeMB: 10}})
	s.SetDetail("e1", "s1", railway.ServiceDetail{Regions: map[string]int{"us": 1}, Deploys: []railway.DeployInfo{{ID: "d1"}}})
	s.SetPanelSeries(place, 60, time.Hour, []railway.MetricSeries{{Measurement: "CPU", Points: []railway.MetricPoint{{TS: 1, Value: 1}}}})
	s.SetConfig("e1", railway.EnvConfig{Etag: "x",
		Services: map[string]railway.ServiceConfig{"s1": {CPU: &cpu, MemoryBytes: &mem, Replicas: map[string]int{"us": 1}}},
		Volumes:  map[string]railway.VolumeConfig{"v1": {SizeMB: 10}}})
	s.SetDomains(place, railway.Domains{Service: []railway.ServiceDomain{{ID: "a", Domain: "a.up"}}, Custom: []railway.CustomDomain{{ID: "b", Domain: "b.com"}}})
	return s
}

func TestCloneDeepMonitor(t *testing.T) {
	s := fullStore()
	want := fullStore()
	c := s.Clone()
	if !reflect.DeepEqual(c.mon, s.mon) {
		t.Fatal("clone differs from original")
	}

	// Mutate every nested level of the clone.
	c.mon.states["e1"]["s1"].InProgress[0] = "X"
	c.mon.states["e1"]["new"] = railway.DeploymentState{}
	c.mon.statesRead["e9"] = true
	c.mon.metrics[svcKey{"e1", "s1"}]["CPU"][0].Value = 99
	c.mon.metrics[svcKey{"e1", "s1"}]["MEM"] = nil
	c.mon.volumes[svcKey{"e1", "s1"}][0].SizeMB = 99
	d := c.mon.details[svcKey{"e1", "s1"}]
	d.Regions["eu"] = 5
	d.Regions["us"] = 99
	d.Deploys[0].ID = "X"
	c.mon.panel[place.Key()][panelSlot{60, time.Hour}]["CPU"][0].Value = 99
	c.mon.panel[place.Key()][panelSlot{300, time.Hour}] = seriesSet{}
	cfg := c.mon.configs["e1"]
	sc := cfg.Services["s1"]
	*sc.CPU = 99
	*sc.MemoryBytes = 99
	sc.Replicas["us"] = 99
	cfg.Services["other"] = railway.ServiceConfig{}
	cfg.Volumes["v1"] = railway.VolumeConfig{SizeMB: 99}
	dom := c.mon.domains[place.Key()]
	dom.Service[0].Domain = "X"
	dom.Custom[0].Domain = "X"

	if !reflect.DeepEqual(s.mon, want.mon) {
		t.Error("mutating the clone changed the original")
	}
}

func TestSettersCopyInput(t *testing.T) {
	s := New()
	pts := []railway.MetricPoint{{TS: 1, Value: 1}}
	s.ReplaceMetrics([]string{"e1"}, []railway.MetricSeries{{EnvID: "e1", ServiceID: "s1", Measurement: "CPU", Points: pts}})
	pts[0].Value = 99
	if s.Series("e1", "s1", "CPU")[0].Value != 1 {
		t.Error("store shares the caller's points")
	}
}

func TestVolumeMountByID(t *testing.T) {
	s := New()
	s.ReplaceVolumes([]string{"e1", "e2"}, []railway.VolumeMount{
		{EnvID: "e1", ServiceID: "s1", VolumeID: "v1", MountPath: "/data"},
		{EnvID: "e2", ServiceID: "s2", VolumeID: "v2", MountPath: "/db"},
	})
	if vm, ok := s.VolumeMount("e1", "v1"); !ok || vm.ServiceID != "s1" || vm.MountPath != "/data" {
		t.Errorf("e1/v1 = %+v, %v", vm, ok)
	}
	if _, ok := s.VolumeMount("e1", "v2"); ok {
		t.Error("v2 found in e1")
	}
}

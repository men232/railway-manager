package store

import (
	"maps"
	"slices"
	"time"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
)

// svcKey identifies one service instance.
type svcKey struct{ env, service string }

// seriesSet is the points of each measurement of one service instance.
type seriesSet map[string][]railway.MetricPoint

// panelSlot is a panel read: its sample step (seconds) and span.
type panelSlot struct {
	step int
	span time.Duration
}

// monitorData holds the monitor collections. Everything it holds is owned by
// the store: values are copied in on write and deep-copied by clone.
type monitorData struct {
	states     map[string]map[string]railway.DeploymentState // env -> service
	statesRead map[string]bool
	metrics    map[svcKey]seriesSet
	volumes    map[svcKey][]railway.VolumeMount
	details    map[svcKey]railway.ServiceDetail
	panel      map[string]map[panelSlot]seriesSet // place key -> step and span -> series
	configs    map[string]railway.EnvConfig
	domains    map[string]railway.Domains // place key
}

func newMonitorData() monitorData {
	return monitorData{
		states:     map[string]map[string]railway.DeploymentState{},
		statesRead: map[string]bool{},
		metrics:    map[svcKey]seriesSet{},
		volumes:    map[svcKey][]railway.VolumeMount{},
		details:    map[svcKey]railway.ServiceDetail{},
		panel:      map[string]map[panelSlot]seriesSet{},
		configs:    map[string]railway.EnvConfig{},
		domains:    map[string]railway.Domains{},
	}
}

func cloneState(d railway.DeploymentState) railway.DeploymentState {
	d.InProgress = slices.Clone(d.InProgress)
	return d
}

func cloneDetail(d railway.ServiceDetail) railway.ServiceDetail {
	d.Regions = maps.Clone(d.Regions)
	d.Deploys = slices.Clone(d.Deploys)
	return d
}

func cloneConfig(c railway.EnvConfig) railway.EnvConfig {
	if c.Services != nil {
		svcs := make(map[string]railway.ServiceConfig, len(c.Services))
		for id, sc := range c.Services {
			if sc.CPU != nil {
				v := *sc.CPU
				sc.CPU = &v
			}
			if sc.MemoryBytes != nil {
				v := *sc.MemoryBytes
				sc.MemoryBytes = &v
			}
			sc.Replicas = maps.Clone(sc.Replicas)
			svcs[id] = sc
		}
		c.Services = svcs
	}
	c.Volumes = maps.Clone(c.Volumes)
	return c
}

func cloneDomains(d railway.Domains) railway.Domains {
	d.Service = slices.Clone(d.Service)
	d.Custom = slices.Clone(d.Custom)
	return d
}

func cloneSet(m seriesSet) seriesSet {
	out := make(seriesSet, len(m))
	for k, pts := range m {
		out[k] = slices.Clone(pts)
	}
	return out
}

func (m monitorData) clone() monitorData {
	c := newMonitorData()
	for env, byService := range m.states {
		cp := make(map[string]railway.DeploymentState, len(byService))
		for id, d := range byService {
			cp[id] = cloneState(d)
		}
		c.states[env] = cp
	}
	maps.Copy(c.statesRead, m.statesRead)
	for k, set := range m.metrics {
		c.metrics[k] = cloneSet(set)
	}
	for k, v := range m.volumes {
		c.volumes[k] = slices.Clone(v)
	}
	for k, d := range m.details {
		c.details[k] = cloneDetail(d)
	}
	for place, byWindow := range m.panel {
		cp := make(map[panelSlot]seriesSet, len(byWindow))
		for w, set := range byWindow {
			cp[w] = cloneSet(set)
		}
		c.panel[place] = cp
	}
	for env, cfg := range m.configs {
		c.configs[env] = cloneConfig(cfg)
	}
	for k, d := range m.domains {
		c.domains[k] = cloneDomains(d)
	}
	return c
}

// dropEnvs removes the entries of the given environments from a map keyed by
// service instance.
func dropEnvs[V any](m map[svcKey]V, envs map[string]bool) {
	for k := range m {
		if envs[k.env] {
			delete(m, k)
		}
	}
}

func envSet(envIDs []string) map[string]bool {
	set := make(map[string]bool, len(envIDs))
	for _, e := range envIDs {
		set[e] = true
	}
	return set
}

// ReplaceStates replaces the deployment states of the given environments.
// Those environments count as read: an instance not listed has nothing
// running. projectID is the project the read belonged to.
func (s *Store) ReplaceStates(projectID string, envIDs []string, st []railway.DeploymentState) {
	_ = projectID
	for _, env := range envIDs {
		s.mon.states[env] = map[string]railway.DeploymentState{}
		s.mon.statesRead[env] = true
	}
	set := envSet(envIDs)
	for _, d := range st {
		if !set[d.EnvID] {
			continue
		}
		s.mon.states[d.EnvID][d.ServiceID] = cloneState(d)
	}
	s.bump(Deploys)
}

// State returns the deployment state of a service instance; false when it was
// not listed (nothing running) or never read.
func (s *Store) State(envID, serviceID string) (railway.DeploymentState, bool) {
	d, ok := s.mon.states[envID][serviceID]
	return d, ok
}

// StatesRead reports whether the environment's states were read at least once.
func (s *Store) StatesRead(envID string) bool { return s.mon.statesRead[envID] }

func groupSeries(m []railway.MetricSeries) map[svcKey]seriesSet {
	out := map[svcKey]seriesSet{}
	for _, ms := range m {
		k := svcKey{ms.EnvID, ms.ServiceID}
		if out[k] == nil {
			out[k] = seriesSet{}
		}
		out[k][ms.Measurement] = slices.Clone(ms.Points)
	}
	return out
}

// ReplaceMetrics replaces the table metrics of the given environments.
func (s *Store) ReplaceMetrics(envIDs []string, m []railway.MetricSeries) {
	set := envSet(envIDs)
	dropEnvs(s.mon.metrics, set)
	for k, ss := range groupSeries(m) {
		if set[k.env] {
			s.mon.metrics[k] = ss
		}
	}
	s.bump(Metrics)
}

// Series returns the table points of one measurement of a service instance.
// The result is read-only.
func (s *Store) Series(envID, serviceID, measurement string) []railway.MetricPoint {
	return s.mon.metrics[svcKey{envID, serviceID}][measurement]
}

// ReplaceVolumes replaces the volume mounts of the given environments.
func (s *Store) ReplaceVolumes(envIDs []string, v []railway.VolumeMount) {
	set := envSet(envIDs)
	dropEnvs(s.mon.volumes, set)
	for _, vm := range v {
		if !set[vm.EnvID] {
			continue
		}
		k := svcKey{vm.EnvID, vm.ServiceID}
		s.mon.volumes[k] = append(s.mon.volumes[k], vm)
	}
	s.bump(Volumes)
}

// Volumes returns the volumes mounted in a service instance. Read-only.
func (s *Store) Volumes(envID, serviceID string) []railway.VolumeMount {
	return s.mon.volumes[svcKey{envID, serviceID}]
}

// VolumeMount returns the mount of a volume in an environment, if the store
// has it.
func (s *Store) VolumeMount(envID, volumeID string) (railway.VolumeMount, bool) {
	for k, vs := range s.mon.volumes {
		if k.env != envID {
			continue
		}
		for _, v := range vs {
			if v.VolumeID == volumeID {
				return v, true
			}
		}
	}
	return railway.VolumeMount{}, false
}

// SetDetail stores the detail panel data of a service instance.
func (s *Store) SetDetail(envID, serviceID string, d railway.ServiceDetail) {
	s.mon.details[svcKey{envID, serviceID}] = cloneDetail(d)
	s.bump(Details)
}

// Detail returns the detail panel data of a service instance.
func (s *Store) Detail(envID, serviceID string) (railway.ServiceDetail, bool) {
	d, ok := s.mon.details[svcKey{envID, serviceID}]
	return d, ok
}

// SetPanelSeries stores the panel series of a place read at step seconds
// over span, replacing the ones kept for that place, step and span.
func (s *Store) SetPanelSeries(p domain.Place, step int, span time.Duration, m []railway.MetricSeries) {
	set := seriesSet{}
	for _, ms := range m {
		set[ms.Measurement] = slices.Clone(ms.Points)
	}
	k := p.Key()
	if s.mon.panel[k] == nil {
		s.mon.panel[k] = map[panelSlot]seriesSet{}
	}
	s.mon.panel[k][panelSlot{step, span}] = set
	s.bump(PanelMetrics)
}

// PanelSeries returns the panel points of one measurement of a place, step
// and span. The result is read-only.
func (s *Store) PanelSeries(p domain.Place, step int, span time.Duration, measurement string) []railway.MetricPoint {
	return s.mon.panel[p.Key()][panelSlot{step, span}][measurement]
}

// SetConfig stores an environment's config.
func (s *Store) SetConfig(envID string, c railway.EnvConfig) {
	s.mon.configs[envID] = cloneConfig(c)
	s.bump(Configs)
}

// Config returns an environment's config. The result is read-only: its maps
// and pointers belong to the store.
func (s *Store) Config(envID string) (railway.EnvConfig, bool) {
	c, ok := s.mon.configs[envID]
	return c, ok
}

// SetDomains stores the domains of a place.
func (s *Store) SetDomains(p domain.Place, d railway.Domains) {
	s.mon.domains[p.Key()] = cloneDomains(d)
	s.bump(DomainsCol)
}

// Domains returns the domains of a place. Read-only.
func (s *Store) Domains(p domain.Place) (railway.Domains, bool) {
	d, ok := s.mon.domains[p.Key()]
	return d, ok
}

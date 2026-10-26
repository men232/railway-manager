// Package store is the typed in-memory data model every screen and kind reads.
package store

import (
	"maps"
	"slices"
	"time"

	"github.com/men232/railway-manager/internal/railway"
)

// Collection names a group of store data with its own revision counter.
type Collection int

const (
	Catalog Collection = iota
	Variables
	Loads
	Deploys
	Metrics
	Volumes
	Details
	Configs
	DomainsCol
	PanelMetrics
	History
	Members
	Patches
	collectionCount
)

// Update is implemented by every loader message that carries data. The root
// model applies any Update before routing the message, so new data types
// never touch the root model.
type Update interface {
	ApplyTo(s *Store)
}

// LoadState is the status of one load.
type LoadState struct {
	Loading bool
	Err     string
	ReadAt  time.Time
}

// Store holds the catalog, variables, indexes and load status.
//
// A Store is not safe for concurrent use: it is read and mutated only from
// Bubble Tea's Update goroutine, so it has no mutex.
type Store struct {
	revs [collectionCount]uint64

	// catalog and its indexes
	projects       []projectRef
	envProject     map[string]*projectRef
	envName        map[string]string
	serviceNames   map[string]string
	envServiceByNm map[string]map[string]string

	// variables and their indexes
	vars     map[string]*Variable
	byPlace  map[string][]string
	refs     map[string][]string
	revRefs  map[string][]string
	projVars map[string][]string
	// system holds each project's Railway system values (railway.
	// SystemVarNames) by location id; never variables.
	system map[string]map[string]string

	loads map[string]LoadState

	mon  monitorData
	hist historyData
}

// New returns an empty store.
func New() *Store {
	return &Store{
		envProject:     map[string]*projectRef{},
		envName:        map[string]string{},
		serviceNames:   map[string]string{},
		envServiceByNm: map[string]map[string]string{},
		vars:           map[string]*Variable{},
		byPlace:        map[string][]string{},
		refs:           map[string][]string{},
		revRefs:        map[string][]string{},
		projVars:       map[string][]string{},
		system:         map[string]map[string]string{},
		loads:          map[string]LoadState{},
		mon:            newMonitorData(),
		hist:           newHistoryData(),
	}
}

// Rev returns the revision of a collection; it grows on every write.
func (s *Store) Rev(c Collection) uint64 { return s.revs[c] }

func (s *Store) bump(c Collection) { s.revs[c]++ }

// BeginLoad marks a load as running; the last error is cleared, ReadAt kept.
func (s *Store) BeginLoad(key string) {
	st := s.loads[key]
	st.Loading, st.Err = true, ""
	s.loads[key] = st
	s.bump(Loads)
}

// FinishLoad marks a load as done at the given time.
func (s *Store) FinishLoad(key string, at time.Time) {
	s.loads[key] = LoadState{ReadAt: at}
	s.bump(Loads)
}

// FinishLoadWarn marks a load as done at the given time, keeping a warning
// ("! " + text) in Err: the data was applied but part of the read failed.
func (s *Store) FinishLoadWarn(key string, at time.Time, warning string) {
	s.loads[key] = LoadState{ReadAt: at, Err: "! " + warning}
	s.bump(Loads)
}

// FailLoad records why a load failed; the last ReadAt (and data) is kept.
func (s *Store) FailLoad(key, err string) {
	st := s.loads[key]
	st.Loading, st.Err = false, err
	s.loads[key] = st
	s.bump(Loads)
}

// Load returns the status of a load; the zero value if never started.
func (s *Store) Load(key string) LoadState { return s.loads[key] }

// Clone returns a deep copy: nothing the copy holds is shared with s, so a
// copy can be read from another goroutine while s keeps changing.
func (s *Store) Clone() *Store {
	c := New()
	c.revs = s.revs
	c.projects = make([]projectRef, len(s.projects))
	for i, p := range s.projects {
		pr := p.Project
		pr.Services = slices.Clone(pr.Services)
		pr.Environments = make([]railway.Environment, len(p.Environments))
		for j, e := range p.Environments {
			e.ServiceIDs = slices.Clone(e.ServiceIDs)
			pr.Environments[j] = e
		}
		c.projects[i] = projectRef{pr}
	}
	for envID, p := range s.envProject {
		for i := range c.projects {
			if c.projects[i].ID == p.ID {
				c.envProject[envID] = &c.projects[i]
				break
			}
		}
	}
	maps.Copy(c.envName, s.envName)
	maps.Copy(c.serviceNames, s.serviceNames)
	for envID, m := range s.envServiceByNm {
		c.envServiceByNm[envID] = maps.Clone(m)
	}
	for id, v := range s.vars {
		cp := *v
		c.vars[id] = &cp
	}
	cloneLists := func(dst, src map[string][]string) {
		for k, l := range src {
			dst[k] = slices.Clone(l)
		}
	}
	cloneLists(c.byPlace, s.byPlace)
	cloneLists(c.refs, s.refs)
	cloneLists(c.revRefs, s.revRefs)
	cloneLists(c.projVars, s.projVars)
	for p, m := range s.system {
		c.system[p] = maps.Clone(m)
	}
	maps.Copy(c.loads, s.loads)
	c.mon = s.mon.clone()
	c.hist = s.hist.clone()
	return c
}

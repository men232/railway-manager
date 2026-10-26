package store

import (
	"sort"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
)

type projectRef struct {
	railway.Project
}

// Instance is one service in one environment, by name.
type Instance struct {
	Place                 domain.Place
	Project, Env, Service string
}

// ReplaceCatalog replaces the catalog and rebuilds its indexes. References
// depend on service names, so they are resolved again. Variables of projects
// that vanished from the catalog are deliberately kept until their project is
// replaced again.
func (s *Store) ReplaceCatalog(c railway.Catalog) {
	s.projects = make([]projectRef, len(c.Projects))
	s.envProject = map[string]*projectRef{}
	s.envName = map[string]string{}
	s.serviceNames = map[string]string{}
	s.envServiceByNm = map[string]map[string]string{}
	for i, p := range c.Projects {
		s.projects[i] = projectRef{p}
	}
	for i := range s.projects {
		p := &s.projects[i]
		names := map[string]string{}
		for _, sv := range p.Services {
			s.serviceNames[sv.ID] = sv.Name
			names[sv.ID] = sv.Name
		}
		for _, e := range p.Environments {
			s.envProject[e.ID] = p
			s.envName[e.ID] = e.Name
			byName := map[string]string{}
			for _, id := range e.ServiceIDs {
				if n, ok := names[id]; ok {
					byName[n] = id
				}
			}
			s.envServiceByNm[e.ID] = byName
		}
	}
	s.rebuildReferences()
	s.bump(Catalog)
	// Reference edges (Dependents, ResolveReference) depend on the catalog, so
	// when variables exist their revision moves too, or memoized selectors
	// reading Rev(Variables) would go stale.
	if len(s.vars) > 0 {
		s.bump(Variables)
	}
}

// Projects returns the catalog in its original order. The outer slice is a
// copy, but the Environments, Services and ServiceIDs slices alias store
// internals: treat the result as read-only.
func (s *Store) Projects() []railway.Project {
	out := make([]railway.Project, len(s.projects))
	for i, p := range s.projects {
		out[i] = p.Project
	}
	return out
}

// EnvLabel is "project/env", or the bare id for an unknown environment.
func (s *Store) EnvLabel(envID string) string {
	p, ok := s.envProject[envID]
	if !ok {
		return envID
	}
	return p.Name + "/" + s.envName[envID]
}

// EnvName is the environment's own name, without its project; the bare id
// for an unknown environment.
func (s *Store) EnvName(envID string) string {
	if n, ok := s.envName[envID]; ok {
		return n
	}
	return envID
}

// ServiceName is the service's name, "shared" for the shared scope, and the
// bare id for an unknown service.
func (s *Store) ServiceName(serviceID string) string {
	if serviceID == "" {
		return "shared"
	}
	if n, ok := s.serviceNames[serviceID]; ok {
		return n
	}
	return serviceID
}

// ServiceIDByName returns the id of the named service in an environment, or "".
func (s *Store) ServiceIDByName(envID, name string) string {
	return s.envServiceByNm[envID][name]
}

// PlaceOf builds the Place for an environment and service ("" for shared).
func (s *Store) PlaceOf(envID, serviceID string) domain.Place {
	pl := domain.Place{EnvID: envID, ServiceID: serviceID}
	if p, ok := s.envProject[envID]; ok {
		pl.ProjectID = p.ID
	}
	return pl
}

// Instances lists the services of the given environments, in project order,
// then environment name, then service name.
func (s *Store) Instances(envIDs []string) []Instance {
	want := map[string]bool{}
	for _, id := range envIDs {
		want[id] = true
	}
	var out []Instance
	for _, p := range s.projects {
		envs := make([]railway.Environment, 0, len(p.Environments))
		for _, e := range p.Environments {
			if want[e.ID] {
				envs = append(envs, e)
			}
		}
		sort.SliceStable(envs, func(i, j int) bool { return envs[i].Name < envs[j].Name })
		for _, e := range envs {
			start := len(out)
			for _, id := range e.ServiceIDs {
				out = append(out, Instance{
					Place:   domain.Place{ProjectID: p.ID, EnvID: e.ID, ServiceID: id},
					Project: p.Name, Env: e.Name, Service: s.ServiceName(id),
				})
			}
			part := out[start:]
			sort.SliceStable(part, func(i, j int) bool { return part[i].Service < part[j].Service })
		}
	}
	return out
}

package store

import (
	"slices"
	"sort"
	"strings"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
)

// Variable is one variable at one place. Fingerprints come from the
// unrendered value; Unrendered and Rendered may be secrets and must never be
// logged or put in an error.
type Variable struct {
	ID                                                 string
	Place                                              domain.Place
	Name, Unrendered, Rendered, UpdatedAt, Fingerprint string
	Sealed, Literal                                    bool
}

// Known returns what the store holds now of a project at the places unread
// matches, as ReplaceVariables takes it: a partial read passes them on, so
// a place it could not read keeps its last variables instead of losing them.
func (s *Store) Known(projectID string, unread func(envID, serviceID string) bool) ([]railway.VarRecord, []railway.SystemRecord) {
	var recs []railway.VarRecord
	for _, id := range s.projVars[projectID] {
		v := s.vars[id]
		if v == nil || !unread(v.Place.EnvID, v.Place.ServiceID) {
			continue
		}
		recs = append(recs, railway.VarRecord{EnvID: v.Place.EnvID, ServiceID: v.Place.ServiceID, Name: v.Name,
			Unrendered: v.Unrendered, Rendered: v.Rendered, UpdatedAt: v.UpdatedAt, Sealed: v.Sealed})
	}
	var system []railway.SystemRecord
	for id, value := range s.system[projectID] {
		parts := strings.SplitN(id, "/", 3) // env/service-or-shared/name; Railway ids hold no "/"
		if len(parts) != 3 {
			continue
		}
		svc := parts[1]
		if svc == "shared" {
			svc = ""
		}
		if unread(parts[0], svc) {
			system = append(system, railway.SystemRecord{EnvID: parts[0], ServiceID: svc, Name: parts[2], Value: value})
		}
	}
	return recs, system
}

// ReplaceVariables replaces one project's variables and system values and
// recomputes literal, fingerprint and references for the whole store.
func (s *Store) ReplaceVariables(projectID string, recs []railway.VarRecord, system ...railway.SystemRecord) {
	sys := make(map[string]string, len(system))
	for _, r := range system {
		sys[domain.LocationID(r.EnvID, r.ServiceID, r.Name)] = r.Value
	}
	s.system[projectID] = sys
	for _, id := range s.projVars[projectID] {
		delete(s.vars, id)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		id := domain.LocationID(r.EnvID, r.ServiceID, r.Name)
		literal := domain.IsLiteral(r.Unrendered)
		s.vars[id] = &Variable{
			ID:          id,
			Place:       domain.Place{ProjectID: projectID, EnvID: r.EnvID, ServiceID: r.ServiceID},
			Name:        r.Name,
			Unrendered:  r.Unrendered,
			Rendered:    r.Rendered,
			UpdatedAt:   r.UpdatedAt,
			Sealed:      r.Sealed,
			Literal:     literal,
			Fingerprint: domain.Fingerprint(r.Name, r.Unrendered, r.Sealed, literal, r.EnvID, r.ServiceID),
		}
		ids = append(ids, id)
	}
	s.projVars[projectID] = ids

	s.byPlace = map[string][]string{}
	for id, v := range s.vars {
		k := v.Place.Key()
		s.byPlace[k] = append(s.byPlace[k], id)
	}
	for _, list := range s.byPlace {
		sort.Slice(list, func(i, j int) bool { return s.vars[list[i]].Name < s.vars[list[j]].Name })
	}
	s.rebuildReferences()
	s.bump(Variables)
}

// rebuildReferences recomputes forward and reverse reference edges for every
// variable. Edges point only at variables that exist.
func (s *Store) rebuildReferences() {
	s.refs = map[string][]string{}
	s.revRefs = map[string][]string{}
	for id, v := range s.vars {
		seen := map[string]bool{}
		for _, r := range domain.ParseReferences(v.Unrendered) {
			target := s.ResolveReference(v.Place, r)
			if target == "" || seen[target] {
				continue
			}
			seen[target] = true
			s.refs[id] = append(s.refs[id], target)
			s.revRefs[target] = append(s.revRefs[target], id)
		}
	}
}

// VariablesAt lists the variables at a place, sorted by name. Only the
// environment and service of the place are used.
func (s *Store) VariablesAt(p domain.Place) []*Variable {
	ids := s.byPlace[p.Key()]
	out := make([]*Variable, len(ids))
	for i, id := range ids {
		out[i] = s.vars[id]
	}
	return out
}

// SystemVar returns a Railway system value (railway.SystemVarNames) of a
// service in an environment, or "" when unknown. System values are never
// variables: they only identify a service's hosts.
func (s *Store) SystemVar(envID, serviceID, name string) string {
	id := domain.LocationID(envID, serviceID, name)
	for _, m := range s.system {
		if v, ok := m[id]; ok {
			return v
		}
	}
	return ""
}

// Variable returns the variable with a location id, or nil.
func (s *Store) Variable(locID string) *Variable { return s.vars[locID] }

// VariableCount is the number of variables held for a project.
func (s *Store) VariableCount(projectID string) int { return len(s.projVars[projectID]) }

// VariableCountIn is the number of variables held for a project in the
// environments envIDs.
func (s *Store) VariableCountIn(projectID string, envIDs []string) int {
	n := 0
	for _, id := range s.projVars[projectID] {
		if v := s.vars[id]; v != nil && slices.Contains(envIDs, v.Place.EnvID) {
			n++
		}
	}
	return n
}

// Dependents returns every variable that references locID directly or
// through other references, sorted. It terminates on cycles; a variable in a
// cycle is its own dependent.
func (s *Store) Dependents(locID string) []string {
	seen := map[string]bool{}
	stack := []string{locID}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range s.revRefs[cur] {
			if !seen[d] {
				seen[d] = true
				stack = append(stack, d)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ResolveReference returns the location id a reference in a variable at
// `from` points to, or "" when the target service or variable is unknown.
func (s *Store) ResolveReference(from domain.Place, r domain.Reference) string {
	var serviceID string
	switch {
	case r.Shared:
		serviceID = ""
	case r.Service == "":
		serviceID = from.ServiceID
	default:
		serviceID = s.ServiceIDByName(from.EnvID, r.Service)
		if serviceID == "" {
			return ""
		}
	}
	id := domain.LocationID(from.EnvID, serviceID, r.Name)
	if _, ok := s.vars[id]; !ok {
		return ""
	}
	return id
}

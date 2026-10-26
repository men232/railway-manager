package view

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

// Grouping is how the Variables tree groups places.
type Grouping int

const (
	// ByName: a group per variable name, sorted by name.
	ByName Grouping = iota
	// ByValue: a group per value fingerprint; sealed and
	// RAILWAY_ variables are one-place groups at the end (Open decision 6).
	ByValue
)

// By marks of a place whose history is unread, or has no matching entry.
const (
	ByUnread = "…"
	ByNone   = "?"
)

// PlaceRow is one variable at one place in the Variables tree. It never
// holds a value: Pending is a copy of the change with New and Base.Value
// cleared, and Ref names the references, not the template.
//
// Env is the environment label ("project/env") and Service the service name
// ("shared" for the shared scope), unsanitized like every row string. Letter
// indexes the place's current fingerprint among its group's (0 = a), -1 when
// the place holds no value yet (a pending add, or a variable gone from
// Railway); Fingerprint is "" then. A sealed place has no letter either: its
// fingerprint is unique to it and says nothing about the value. By names who
// changed the variable last (Open decision 4): ByUnread until the
// environment's history is read, ByNone without a matching Variable entry or
// actor in the pages read so far (an older page may hold one). Overlay,
// Missing, Invalid and Drifted are as VarRow's.
type PlaceRow struct {
	Place              domain.Place
	Env, Service, Name string
	LocationID         string
	Length             int
	Letter             int
	Fingerprint        string
	UpdatedAt          time.Time
	By                 string
	Sealed             bool
	Ref                string
	Pending            *changeset.Change
	Overlay            apply.Overlay
	Missing            bool
	Invalid, Drifted   string
}

// GroupRow is one group of the tree. Key is stable across refreshes for the
// fold state: "name:<NAME>" by name, "fp:<fingerprint>" by value (a
// one-place group adds "@<location id>"). Label is the sorted distinct names
// joined with " = ". Values counts the distinct fingerprints, the pending
// new values included; Envs lists the environment labels in place order;
// Pending counts the places with a pending change. Sealed places add no
// value to Values.
type GroupRow struct {
	Key, Label string
	Names      []string
	Places     []PlaceRow
	Values     int
	Envs       []string
	Pending    int
}

// treeRow is a place row with what grouping needs and the row must not
// carry.
type treeRow struct {
	PlaceRow
	newFP string // fingerprint of the pending new value, "" without one
	alone bool   // sealed or RAILWAY_: a one-place group by value
}

// Tree lists the variables of the environments as groups of places. With a
// scope only that place's variables are listed (the per-service view) and
// envIDs is ignored. Places are ordered by project, environment name, then
// the shared scope and services by name; they include pending adds.
func (v *Views) Tree(cs *changeset.ChangeSet, envIDs []string, scope *domain.Place, g Grouping) []GroupRow {
	places := v.treePlaces(envIDs, scope)
	k := key(append(rev(v.Store, store.Catalog, store.Variables, store.History, store.Members),
		csRev(cs), strings.Join(envIDs, ","), scopeKey(scope), strconv.Itoa(int(g)), v.placesRead(places))...)
	return v.tree.Get(k, func() []GroupRow {
		v.treeBuilds++
		rows := v.treeRows(changes(cs), places)
		if g == ByValue {
			return groupByValue(rows)
		}
		return groupByName(rows)
	})
}

func scopeKey(p *domain.Place) string {
	if p == nil {
		return "-"
	}
	return p.ProjectID + "|" + p.Key()
}

// TreePlaces lists the places of the environments in the tree's order:
// by project, environment name, then the shared scope and the services by
// name.
func (v *Views) TreePlaces(envIDs []string) []domain.Place { return v.treePlaces(envIDs, nil) }

// treePlaces lists the places of the tree in order.
func (v *Views) treePlaces(envIDs []string, scope *domain.Place) []domain.Place {
	if scope != nil {
		return []domain.Place{*scope}
	}
	want := map[string]bool{}
	for _, id := range envIDs {
		want[id] = true
	}
	var out []domain.Place
	for _, p := range v.Store.Projects() {
		var envs []railway.Environment
		for _, e := range p.Environments {
			if want[e.ID] {
				envs = append(envs, e)
			}
		}
		sort.SliceStable(envs, func(i, j int) bool { return envs[i].Name < envs[j].Name })
		for _, e := range envs {
			out = append(out, domain.Place{ProjectID: p.ID, EnvID: e.ID})
			svcs := slices.Clone(e.ServiceIDs)
			sort.SliceStable(svcs, func(i, j int) bool { return v.Store.ServiceName(svcs[i]) < v.Store.ServiceName(svcs[j]) })
			for _, id := range svcs {
				out = append(out, domain.Place{ProjectID: p.ID, EnvID: e.ID, ServiceID: id})
			}
		}
	}
	return out
}

// placesRead is a cache key part: which projects of the places have had
// their inventory read (drift waits for it).
func (v *Views) placesRead(places []domain.Place) string {
	seen := map[string]bool{}
	var read []string
	for _, p := range places {
		if seen[p.ProjectID] {
			continue
		}
		seen[p.ProjectID] = true
		if v.inventoryRead(p.ProjectID) {
			read = append(read, p.ProjectID)
		}
	}
	return strings.Join(read, ",")
}

// treeRows builds the rows of every place, in place order then name.
func (v *Views) treeRows(cl []changeset.Change, places []domain.Place) []treeRow {
	byPlace := variableChanges(cl)
	authors := map[string]*byIndex{}
	var out []treeRow
	for _, p := range places {
		ix := authors[p.EnvID]
		if ix == nil {
			ix = v.historyIndex(p.EnvID)
			authors[p.EnvID] = ix
		}
		env, svc := v.Store.EnvLabel(p.EnvID), v.Store.ServiceName(p.ServiceID)
		for _, vr := range v.placeVars(byPlace[p.Key()], p) {
			r := treeRow{PlaceRow: PlaceRow{
				Place: p, Env: env, Service: svc, Name: vr.Name, LocationID: vr.LocationID,
				Length: vr.Length, Letter: -1, Sealed: vr.Sealed, Pending: stripValues(vr.Change),
				Overlay: vr.Overlay, Missing: vr.Missing, Invalid: vr.Invalid, Drifted: vr.Drifted,
				By: ix.by(v.Store, p, vr.Name),
			}}
			r.UpdatedAt = parseUpdated(vr.UpdatedAt)
			if sv := v.Store.Variable(vr.LocationID); sv != nil && !vr.Missing {
				r.Fingerprint = sv.Fingerprint
				r.Ref = refNames(sv.Unrendered)
			}
			if vr.Change != nil {
				r.newFP = v.Reg.NewFingerprint(*vr.Change)
			}
			r.alone = r.Sealed || strings.HasPrefix(r.Name, "RAILWAY_")
			out = append(out, r)
		}
	}
	return out
}

// parseUpdated reads a variable's updatedAt: Railway's UTC
// "2006-01-02T15:04:05" (railway.VarRecord), or RFC 3339; zero when neither.
func parseUpdated(s string) time.Time {
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// stripValues copies a change without its values.
func stripValues(c *changeset.Change) *changeset.Change {
	if c == nil {
		return nil
	}
	cp := *c
	cp.New = nil
	cp.Base.Value = ""
	cp.Requires = slices.Clone(c.Requires)
	if c.Origin != nil {
		o := *c.Origin
		cp.Origin = &o
	}
	return &cp
}

// refNames names the references of an unrendered value ("svc.VAR",
// "shared.VAR" or "VAR"), joined with ", "; "" for a literal. The rest of the
// template is left out: it may hold a secret.
func refNames(unrendered string) string {
	var out []string
	for _, r := range domain.ParseReferences(unrendered) {
		switch {
		case r.Shared:
			out = append(out, "shared."+r.Name)
		case r.Service != "":
			out = append(out, r.Service+"."+r.Name)
		default:
			out = append(out, r.Name)
		}
	}
	return strings.Join(out, ", ")
}

// byIndex is an environment's newest history entry per service and name.
type byIndex struct {
	read   bool
	latest map[string]railway.HistoryEntry // service id + "/" + name
}

func (v *Views) historyIndex(envID string) *byIndex {
	entries, _, _, read := v.Store.History(envID)
	ix := &byIndex{read: read, latest: map[string]railway.HistoryEntry{}}
	add := func(k string, e railway.HistoryEntry) {
		if _, ok := ix.latest[k]; !ok { // newest first: keep the first
			ix.latest[k] = e
		}
	}
	for _, e := range entries {
		if e.Object != "Variable" { // another object's payload name is not this variable's
			continue
		}
		for _, n := range e.Names {
			if len(e.ServiceIDs) == 0 {
				add("/"+n, e)
			}
			for _, s := range e.ServiceIDs {
				add(s+"/"+n, e)
			}
		}
	}
	return ix
}

func (ix *byIndex) by(s *store.Store, p domain.Place, name string) string {
	if !ix.read {
		return ByUnread
	}
	e, ok := ix.latest[p.ServiceID+"/"+name]
	if !ok || e.Actor == "" {
		return ByNone
	}
	return s.MemberName(p.ProjectID, e.Actor)
}

// group collects rows into one group, assigning letters by first
// appearance of each current fingerprint.
func group(k string, rows []treeRow) GroupRow {
	g := GroupRow{Key: k}
	letters := map[string]int{}
	values := map[string]bool{}
	names := map[string]bool{}
	envs := map[string]bool{}
	for _, r := range rows {
		pr := r.PlaceRow
		// A sealed fingerprint is unique per place and says nothing about
		// the value: no letter, not counted.
		if pr.Fingerprint != "" && !pr.Sealed {
			values[pr.Fingerprint] = true
			l, ok := letters[pr.Fingerprint]
			if !ok {
				l = len(letters)
				letters[pr.Fingerprint] = l
			}
			pr.Letter = l
		}
		if r.newFP != "" && !pr.Sealed {
			values[r.newFP] = true
		}
		if pr.Pending != nil {
			g.Pending++
		}
		if !names[pr.Name] {
			names[pr.Name] = true
			g.Names = append(g.Names, pr.Name)
		}
		if !envs[pr.Env] {
			envs[pr.Env] = true
			g.Envs = append(g.Envs, pr.Env)
		}
		g.Places = append(g.Places, pr)
	}
	sort.Strings(g.Names)
	g.Label = strings.Join(g.Names, " = ")
	g.Values = len(values)
	return g
}

func groupByName(rows []treeRow) []GroupRow {
	byName := map[string][]treeRow{}
	var order []string
	for _, r := range rows {
		if _, ok := byName[r.Name]; !ok {
			order = append(order, r.Name)
		}
		byName[r.Name] = append(byName[r.Name], r)
	}
	sort.Strings(order)
	out := make([]GroupRow, len(order))
	for i, n := range order {
		out[i] = group("name:"+n, byName[n])
	}
	return out
}

// valueKey is the by-value group of a row: its current fingerprint, else the
// one it will hold; a variable gone from Railway with nothing new is its own
// group, as are sealed and RAILWAY_ variables.
func valueKey(r treeRow) string {
	fp := r.Fingerprint
	if fp == "" {
		fp = r.newFP
	}
	if fp == "" {
		return "fp:gone@" + r.LocationID
	}
	if r.alone {
		return "fp:" + fp + "@" + r.LocationID
	}
	return "fp:" + fp
}

func groupByValue(rows []treeRow) []GroupRow {
	byKey := map[string][]treeRow{}
	var order []string
	for _, r := range rows {
		k := valueKey(r)
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], r)
	}
	out := make([]GroupRow, len(order))
	alone := make(map[string]bool, len(order))
	for i, k := range order {
		out[i] = group(k, byKey[k])
		alone[k] = byKey[k][0].alone
	}
	// order is first appearance, so ties keep place order
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if alone[a.Key] != alone[b.Key] {
			return !alone[a.Key]
		}
		if len(a.Places) != len(b.Places) {
			return len(a.Places) > len(b.Places)
		}
		return a.Names[0] < b.Names[0]
	})
	return out
}

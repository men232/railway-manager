// Package rotation turns a store snapshot into rotation groups: identical
// secret values across the selected environments, classified by name and
// shape. It is pure: no network, no clock, no TUI. Group, Loc
// and DB never hold a value; only its length and shape are kept.
package rotation

import (
	"sort"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

// Class is a group's classification. The order is the sort order.
type Class int

const (
	ClassDB Class = iota
	ClassURL
	ClassGen
	ClassAsk
	ClassHidden
)

func (c Class) String() string {
	switch c {
	case ClassDB:
		return "DB"
	case ClassURL:
		return "URL"
	case ClassGen:
		return "GEN"
	case ClassAsk:
		return "ASK"
	default:
		return "HIDDEN"
	}
}

// Loc is one location of a group: a variable at a place.
type Loc struct {
	ID, Name string
	Place    domain.Place
	// Updated is the variable's last change; zero when unknown.
	Updated time.Time
	Sealed  bool
	// Fresh: changed after the leak time.
	Fresh bool
}

// Group is one old value (by fingerprint) and every selected location
// holding it.
type Group struct {
	// Key is the fingerprint.
	Key   string
	Names []string
	Locs  []Loc
	Class Class

	Public, OffByDefault, Sealed, AllFresh bool
	Warning, Problem, Note                 string

	// DB is the database a DB or URL group belongs to.
	DB *DB
	// OldLen and Hex describe the old value for generation (sealed counts as
	// empty); the value itself is not kept.
	OldLen int
	Hex    bool
}

// Options selects what Analyze reads.
type Options struct {
	// EnvIDs are the selected environments; places elsewhere are not read.
	EnvIDs []string
	// AllEnvIDs are the environments read for database detection and URL
	// matching; EnvIDs when empty. Groups come from EnvIDs only.
	AllEnvIDs []string
	// Internal names are merged into the built-in internal (GEN) names.
	Internal []string
	// Since is the leak time; zero for none.
	Since time.Time
}

// Analysis is the result of Analyze.
type Analysis struct {
	Groups   []Group
	DBs      []DB
	dbPlaces map[string]bool
}

// IsDBPlace reports whether a place belongs to a database: an owner, a
// member, or a place with REPLICA_SET_NAME.
func (a Analysis) IsDBPlace(p domain.Place) bool { return a.dbPlaces[p.Key()] }

// Label is the sorted distinct names joined with " = ".
func Label(names []string) string {
	return strings.Join(distinctSorted(names), " = ")
}

// Analyze groups the literal variables of the selected environments by
// fingerprint and classifies each group.
func Analyze(s *store.Store, o Options) Analysis {
	places := selectedPlaces(s, o.EnvIDs)
	detect := places
	if len(o.AllEnvIDs) > 0 {
		detect = selectedPlaces(s, o.AllEnvIDs)
	}
	dbs, dbPlaces := detectDBs(s, detect)
	// rootOf maps a root-password location to the index of its DB.
	rootOf := map[string]int{}
	for i, db := range dbs {
		rootOf[domain.LocationID(db.Owner.EnvID, db.Owner.ServiceID, rootVar(db.Kind))] = i
	}

	internal := map[string]bool{}
	for _, n := range builtinInternal {
		internal[n] = true
	}
	for _, n := range o.Internal {
		internal[n] = true
	}

	type acc struct {
		g     Group
		value string
		dbs   []int
	}
	byKey := map[string]*acc{}
	var order []string
	for _, p := range places {
		for _, v := range s.VariablesAt(p) {
			if strings.HasPrefix(v.Name, "RAILWAY_") || !v.Literal {
				continue
			}
			if v.Sealed && !looksSecret(v.Name, internal) {
				continue
			}
			a, ok := byKey[v.Fingerprint]
			if !ok {
				a = &acc{g: Group{Key: v.Fingerprint}}
				if !v.Sealed {
					a.value = v.Unrendered
				}
				byKey[v.Fingerprint] = a
				order = append(order, v.Fingerprint)
			}
			loc := Loc{ID: v.ID, Name: v.Name, Place: v.Place, Updated: parseUpdated(v.UpdatedAt), Sealed: v.Sealed}
			loc.Fresh = !o.Since.IsZero() && loc.Updated.After(o.Since)
			a.g.Locs = append(a.g.Locs, loc)
			a.g.Names = append(a.g.Names, v.Name)
			if v.Sealed {
				a.g.Sealed = true
			}
			if i, ok := rootOf[v.ID]; ok {
				a.dbs = append(a.dbs, i)
			}
		}
	}

	c := classifier{s: s, dbs: dbs, internal: internal}
	groups := make([]Group, 0, len(order))
	for _, k := range order {
		a := byKey[k]
		g := a.g
		g.Names = distinctSorted(g.Names)
		if !g.Sealed {
			g.OldLen = len(a.value)
			g.Hex = isLowerHex(a.value)
		}
		c.classify(&g, a.value, a.dbs)
		if !o.Since.IsZero() {
			g.AllFresh = len(g.Locs) > 0
			for _, l := range g.Locs {
				g.AllFresh = g.AllFresh && l.Fresh
			}
			if g.AllFresh {
				g.Note = joinNote(g.Note, "already changed after the leak everywhere")
			}
		}
		if g.Sealed {
			g.Note = joinNote("sealed: value unreadable, may be empty", g.Note)
		}
		groups = append(groups, g)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Class != groups[j].Class {
			return groups[i].Class < groups[j].Class
		}
		li, lj := Label(groups[i].Names), Label(groups[j].Names)
		if li != lj {
			return li < lj
		}
		return groups[i].Key < groups[j].Key
	})
	return Analysis{Groups: groups, DBs: dbs, dbPlaces: dbPlaces}
}

// selectedPlaces lists the places of the selected environments in catalog
// order: per environment the shared scope, then services by name.
func selectedPlaces(s *store.Store, envIDs []string) []domain.Place {
	want := map[string]bool{}
	for _, id := range envIDs {
		want[id] = true
	}
	var out []domain.Place
	for _, p := range s.Projects() {
		for _, e := range p.Environments {
			if !want[e.ID] {
				continue
			}
			out = append(out, domain.Place{ProjectID: p.ID, EnvID: e.ID})
			ids := append([]string(nil), e.ServiceIDs...)
			sort.SliceStable(ids, func(i, j int) bool { return s.ServiceName(ids[i]) < s.ServiceName(ids[j]) })
			for _, id := range ids {
				out = append(out, domain.Place{ProjectID: p.ID, EnvID: e.ID, ServiceID: id})
			}
		}
	}
	return out
}

// parseUpdated reads a variable's updatedAt: Railway's UTC
// "2006-01-02T15:04:05", or RFC 3339; zero when neither.
func parseUpdated(s string) time.Time {
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func distinctSorted(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func isLowerHex(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + " · " + b
	}
}

package rotation

import (
	"slices"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/store"
)

const (
	groupPrefix = "rotate:"
	addPrefix   = "rotate:add:"
)

// GroupKey is the change group of a row group: "rotate:<fingerprint>".
func GroupKey(fingerprint string) string { return groupPrefix + fingerprint }

// AddGroupKey is the change group of an added variable.
func AddGroupKey(name string) string { return addPrefix + name }

// ParseGroupKey returns the fingerprint of a rotate group; false for add
// groups and groups of other producers.
func ParseGroupKey(g string) (fingerprint string, ok bool) {
	if strings.HasPrefix(g, addPrefix) || !strings.HasPrefix(g, groupPrefix) {
		return "", false
	}
	return strings.TrimPrefix(g, groupPrefix), true
}

// Coverage is how many locations of a group have a change. Group is the
// change group it was counted for.
type Coverage struct {
	Group, Label   string
	Changed, Total int
}

// CoverageOf counts the locations whose current fingerprint is the group's,
// in the environments envIDs, and those among them that have a change of
// that group (same place and target = variable name).
func CoverageOf(s *store.Store, changes []changeset.Change, group string, envIDs []string) (Coverage, bool) {
	fp, ok := ParseGroupKey(group)
	if !ok {
		return Coverage{}, false
	}
	has := map[string]bool{}
	for _, c := range changes {
		if c.Group == group {
			has[c.Place.Key()+"|"+c.Target] = true
		}
	}
	cov := Coverage{Group: group}
	var names []string
	for _, p := range selectedPlaces(s, envIDs) {
		for _, v := range s.VariablesAt(p) {
			if v.Fingerprint != fp {
				continue
			}
			cov.Total++
			names = append(names, v.Name)
			if has[v.Place.Key()+"|"+v.Name] {
				cov.Changed++
			}
		}
	}
	cov.Label = Label(names)
	return cov, true
}

const dbPrefix = "rotate-db:"

// keepKind is the change kind that keeps a place as is; it counts toward a
// password group's coverage.
const keepKind = "variable.keep"

// DBGroupKey is the change group of every change of one database password:
// "rotate-db:<db key>:<user>" (KeyFileUser for a Mongo KEYFILE).
// ParseGroupKey never accepts it.
func DBGroupKey(dbKey, user string) string { return dbPrefix + dbKey + ":" + user }

// ParseDBGroupKey splits a password group into its database key and user;
// false for any other group. Database keys hold no ":".
func ParseDBGroupKey(g string) (dbKey, user string, ok bool) {
	rest, found := strings.CutPrefix(g, dbPrefix)
	if !found {
		return "", "", false
	}
	dbKey, user, found = strings.Cut(rest, ":")
	if !found || dbKey == "" || user == "" {
		return "", "", false
	}
	return dbKey, user, true
}

// RowOf is the password row of group, built as PasswordRows builds it from
// the environments o reads, whether or not it is in o.EnvIDs; false when
// there is none.
func RowOf(s *store.Store, group string, o Options) (PasswordRow, bool) {
	places := selectedPlaces(s, readEnvIDs(o))
	dbs, _ := detectDBs(s, places)
	for _, r := range buildPasswordRows(s, dbs, places, time.Time{}) {
		if r.Key == group {
			return r, true
		}
	}
	return PasswordRow{}, false
}

// DBCoverageOf counts, in o.EnvIDs, the places of a password row that take
// a decision — its own variable, its rewritten URLs, its literal copies
// and its unrecognised places (not references or other users) — and those
// among them with a change of the group or a variable.keep. The row is
// built as PasswordRows builds it, from the environments it reads; false
// when group is not a password group or names no row.
func DBCoverageOf(s *store.Store, changes []changeset.Change, group string, o Options) (Coverage, bool) {
	if _, _, ok := ParseDBGroupKey(group); !ok {
		return Coverage{}, false
	}
	row, ok := RowOf(s, group, o)
	if !ok {
		return Coverage{}, false
	}
	has := map[string]bool{}
	for _, c := range changes {
		if c.Group == group || c.Kind == keepKind {
			has[c.Key()] = true
		}
	}
	var locs []Loc
	if row.Own != nil {
		locs = append(locs, *row.Own)
	}
	for _, u := range slices.Concat(row.URLs, row.Copies) {
		if u.Mode == URLRewrite || u.Mode == URLUnrecognised {
			locs = append(locs, u.Loc)
		}
	}
	cov := Coverage{Group: group}
	var names []string
	for _, l := range locs {
		if !slices.Contains(o.EnvIDs, l.Place.EnvID) {
			continue
		}
		cov.Total++
		names = append(names, l.Name)
		if has[l.Place.Key()+"|"+l.Name] {
			cov.Changed++
		}
	}
	cov.Label = Label(names)
	return cov, true
}

package rotation

import (
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
)

func TestPlaceRefusal(t *testing.T) {
	s := pwStore(envRec("e1", "sworker", "ODD", "mongodb://mongo:pw-0123@x?y@mongo.railway.internal:27017/db"),
		envRec("e2", "ssync", "LEGACY", "mongodb://mongo:"+rootPW+"@old-host.example.com:27017/db"))
	row := rowByKey(t, passwordRows(s, pwOptions("e1", "e2")), "rotate-db:e1/smongo:mongo")
	place := func(id string) URLPlace {
		t.Helper()
		if row.Own != nil && row.Own.ID == id {
			return URLPlace{Loc: *row.Own}
		}
		for _, u := range row.URLs {
			if u.Loc.ID == id {
				return u
			}
		}
		t.Fatalf("no place %s", id)
		return URLPlace{}
	}
	at := func(env, svc, name, group, kind string, auto bool) changeset.Change {
		c := changeset.Change{ID: name + env, Kind: kind, Place: domain.Place{ProjectID: "p1", EnvID: env, ServiceID: svc}, Target: name, Group: group}
		if auto {
			c.Origin = &changeset.Origin{Producer: "rotate", Row: group, Letter: changeset.LetterRewrite}
		}
		return c
	}
	all := func(string) bool { return true }
	onlyE1 := func(env string) bool { return env == "e1" }
	own, worker, sync, odd, api := place("e1/smongo/MONGO_INITDB_ROOT_PASSWORD"), place("e1/sworker/MONGO_URL"),
		place("e2/ssync/MONGO_SOURCE"), place("e1/sworker/ODD"), place("e1/sapi/MONGO_URL")
	legacy := place("e2/ssync/LEGACY")
	g := row.Key
	for _, tc := range []struct {
		name    string
		p       URLPlace
		pending []changeset.Change
		write   func(string) bool
		decided bool
		want    string
	}{
		{"own foreign edit", own, []changeset.Change{at("e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", "", "variable.set", false)}, nil, false,
			"production · mongo MONGO_INITDB_ROOT_PASSWORD has a pending edit · decide this place"},
		{"own read only", own, nil, onlyE1, false, ""},
		{"own read only e2", own, nil, func(env string) bool { return env == "e2" }, false, "production · mongo is read only · Settings w"},
		{"value rotation change", worker, []changeset.Change{at("e1", "sworker", "MONGO_URL", "rotate:fp", "variable.set", false)}, nil, false,
			"production · worker MONGO_URL has a pending rotation change · remove it first"},
		{"foreign keep", sync, []changeset.Change{at("e2", "ssync", "MONGO_SOURCE", "rotate-db:other:u", "variable.keep", false)}, all, false,
			"staging · sync MONGO_SOURCE has a pending rotation change · remove it first"},
		{"read only", sync, nil, onlyE1, false, "staging · sync is read only · Settings w or k keeps it"},
		{"read only kept", sync, []changeset.Change{at("e2", "ssync", "MONGO_SOURCE", g, "variable.keep", false)}, onlyE1, true, ""},
		{"read only, decision not checked", sync, nil, nil, false, ""},
		{"unclear", odd, nil, all, false, "production · worker ODD: set this place by hand"},
		{"unclear decided by hand", odd, []changeset.Change{at("e1", "sworker", "ODD", g, "variable.delete", false)}, all, true, ""},
		{"undecided after the password", worker, nil, all, true, "production · worker MONGO_URL has no decision · select it or k"},
		{"rewritten", worker, []changeset.Change{at("e1", "sworker", "MONGO_URL", g, "variable.set", true)}, all, true, ""},
		{"unrecognised", legacy, nil, all, false, "staging · sync LEGACY carries this password · p f c x or k"},
		{"unrecognised read only", legacy, nil, onlyE1, false, "staging · sync is read only · Settings w or k keeps it"},
		{"unrecognised kept", legacy, []changeset.Change{at("e2", "ssync", "LEGACY", g, "variable.keep", false)}, onlyE1, true, ""},
		{"unrecognised decided", legacy, []changeset.Change{at("e2", "ssync", "LEGACY", g, "variable.set", false)}, all, true, ""},
		{"follows never refuses", api, []changeset.Change{at("e1", "sapi", "MONGO_URL", "", "variable.set", false)}, onlyE1, true, ""},
	} {
		if got := PlaceRefusal(s, row, tc.p, tc.pending, tc.write, tc.decided); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

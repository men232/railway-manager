package view_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/view"
)

// Password fixture values: none may ever reach a row.
const (
	pwRoot    = "mongo-root-pw-0123456789abc"
	pwApp     = "app-pw-0123456789"
	pwKeyFile = "keyfile-value-0123456789abcdef"
	pwNew     = "brand-new-password-0123456789"
)

const (
	pwRootGroup = "rotate-db:e1/smongo:mongo"
	pwAppGroup  = "rotate-db:e1/smongo:app"
)

// pwCatalog: production holds Mongo, api and worker; staging holds api and
// sync, which reads production's Mongo through its TCP proxy.
func pwCatalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{
			{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"smongo", "sapi", "sworker", "smongo2"}},
			{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"sapi", "ssync"}},
		},
		Services: []railway.Service{
			{ID: "smongo", Name: "mongo"}, {ID: "sapi", Name: "api"},
			{ID: "sworker", Name: "worker"}, {ID: "ssync", Name: "sync"}, {ID: "smongo2", Name: "mongo-2"},
		},
	}}}
}

func pwRec(env, service, name, value string) railway.VarRecord {
	return railway.VarRecord{EnvID: env, ServiceID: service, Name: name, Unrendered: value, Rendered: value}
}

func newPwFx(t *testing.T, extra ...railway.VarRecord) *fx {
	t.Helper()
	f := newFx(t)
	f.st.ReplaceCatalog(pwCatalog())
	vars, sys := pwRecs(extra...)
	f.st.ReplaceVariables("p1", vars, sys...)
	return f
}

// pwMember is a Mongo replica member of production's Mongo, with its
// KEYFILE copy.
func pwMember() []railway.VarRecord {
	return []railway.VarRecord{
		pwRec("e1", "smongo2", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"),
		pwRec("e1", "smongo2", "REPLICA_SET_NAME", "rs0"),
		pwRec("e1", "smongo2", "KEYFILE", pwKeyFile),
	}
}

// pwRecs are the fixture's variables plus extra, split from the system
// values.
func pwRecs(extra ...railway.VarRecord) ([]railway.VarRecord, []railway.SystemRecord) {
	recs := append([]railway.VarRecord{
		pwRec("e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", pwRoot),
		pwRec("e1", "smongo", "MONGO_INITDB_ROOT_USERNAME", "mongo"),
		pwRec("e1", "smongo", "KEYFILE", pwKeyFile),
		pwRec("e1", "smongo", "MONGO_URL", "mongodb://${{MONGO_INITDB_ROOT_USERNAME}}:${{MONGO_INITDB_ROOT_PASSWORD}}@${{RAILWAY_PRIVATE_DOMAIN}}:27017"),
		pwRec("e1", "smongo", "RAILWAY_PRIVATE_DOMAIN", "mongo.railway.internal"),
		pwRec("e1", "smongo", "RAILWAY_TCP_PROXY_DOMAIN", "a.proxy.rlwy.net"),
		pwRec("e1", "smongo", "RAILWAY_TCP_PROXY_PORT", "11111"),
		pwRec("e1", "sapi", "MONGO_URL", "${{mongo.MONGO_URL}}"),
		pwRec("e1", "sworker", "MONGO_URL", "mongodb://mongo:"+pwRoot+"@mongo.railway.internal:27017/db?authSource=admin"),
		pwRec("e1", "sworker", "APP_DB", "mongodb://app:"+pwApp+"@mongo.railway.internal:27017/app"),
		pwRec("e2", "ssync", "MONGO_SOURCE", "mongodb://mongo:"+pwRoot+"@a.proxy.rlwy.net:11111/?replicaSet=rs0"),
		pwRec("e2", "sapi", "SRC", "${{sync.MONGO_SOURCE}}"),
	}, extra...)
	var vars []railway.VarRecord
	var sys []railway.SystemRecord
	for _, r := range recs {
		if slices.Contains(railway.SystemVarNames, r.Name) {
			sys = append(sys, railway.SystemRecord{EnvID: r.EnvID, ServiceID: r.ServiceID, Name: r.Name, Value: r.Rendered})
			continue
		}
		vars = append(vars, r)
	}
	return vars, sys
}

var pwOpts = rotation.Options{EnvIDs: []string{"e1", "e2"}, AllEnvIDs: []string{"e1", "e2"}}

var pwWrite = []string{"e1", "e2"}

func pwRow(t *testing.T, rows []view.RotateRow, key string) view.RotateRow {
	t.Helper()
	for _, r := range rows {
		if r.Password != nil && r.Key == key {
			return r
		}
	}
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	t.Fatalf("no password row %q in %v", key, keys)
	return view.RotateRow{}
}

// placeLine is "<loc> <role>[ proxy][ unclear][ ro][ auto|kept|decided]".
func placeLine(p view.RotatePlace) string {
	role := "own"
	if !p.Own {
		role = p.URLMode.String()
	}
	s := p.Loc.ID + " " + role
	if p.ViaProxy {
		s += " proxy"
	}
	if p.Unclear {
		s += " unclear"
	}
	if p.ReadOnly {
		s += " ro"
	}
	switch {
	case p.Kept:
		s += " kept"
	case p.Auto:
		s += " auto"
	case p.Pending != nil:
		s += " decided"
	}
	return s
}

func placeLines(r view.RotateRow) []string {
	out := make([]string, len(r.Places))
	for i, p := range r.Places {
		out[i] = placeLine(p)
	}
	return out
}

func assertNoPwValues(t *testing.T, rows []view.RotateRow) {
	t.Helper()
	dump := fmt.Sprintf("%+v", rows)
	for _, s := range []string{pwRoot, pwApp, pwKeyFile, pwNew} {
		if strings.Contains(dump, s) {
			t.Errorf("a value leaked into the rows")
		}
	}
}

// putPassword puts every change of a password decision.
func (f *fx) putPassword(t *testing.T, group, source, value string) {
	t.Helper()
	row, ok := rotation.RowOf(f.st, group, pwOpts)
	if !ok {
		t.Fatalf("no row %s", group)
	}
	cs, err := kinds.NewPasswordChanges(f.st, f.cs.Changes, row, source, value, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if err := f.cs.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fx) put(t *testing.T, c changeset.Change) {
	t.Helper()
	if err := f.cs.Put(c, nil); err != nil {
		t.Fatal(err)
	}
}

// Every place in every environment, with its mode; the value rotation DB and URL
// rows are gone.
func TestRotatePasswordRowPlaces(t *testing.T) {
	f := newPwFx(t)
	rows := f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{})
	for _, r := range rows {
		if r.Section == rotation.SectionDatabase && r.Password == nil {
			t.Errorf("value rotation database row %q left", r.Label)
		}
	}
	r := pwRow(t, rows, pwRootGroup)
	if r.Label != "mongo root" || r.Class != rotation.ClassDB || r.Section != rotation.SectionDatabase {
		t.Errorf("row label %q class %v section %v", r.Label, r.Class, r.Section)
	}
	want := []string{
		"e1/smongo/MONGO_INITDB_ROOT_PASSWORD own",
		"e1/sapi/MONGO_URL follows",
		"e1/smongo/MONGO_URL follows",
		"e1/sworker/MONGO_URL rewrite",
		"e2/sapi/SRC follows",
		"e2/ssync/MONGO_SOURCE rewrite proxy",
		"e1/sworker/APP_DB other",
	}
	if got := placeLines(r); !slices.Equal(got, want) {
		t.Errorf("places =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.Decided != 0 || r.Decidable != 3 {
		t.Errorf("decided %d/%d, want 0/3", r.Decided, r.Decidable)
	}
	if !slices.Equal(r.Envs, []string{"Proj/production", "Proj/staging"}) {
		t.Errorf("envs = %v", r.Envs)
	}
	if r.Blocked != "" || r.Stalled || r.Changed || r.Run != nil || r.Change != nil {
		t.Errorf("row state = blocked %q stalled %v changed %v run %v change %v", r.Blocked, r.Stalled, r.Changed, r.Run, r.Change)
	}
	if u := pwRow(t, rows, pwAppGroup); u.Label != "mongo user app" || u.Class != rotation.ClassURL || u.Decidable != 2 {
		t.Errorf("user row %q class %v decidable %d", u.Label, u.Class, u.Decidable)
	}
	kf := pwRow(t, rows, "rotate-db:e1/smongo:@keyfile")
	if kf.Label != "mongo KEYFILE" || len(kf.Places) != 1 || !kf.Places[0].Own || kf.Warning == "" {
		t.Errorf("keyfile row %q places %v warning %q", kf.Label, placeLines(kf), kf.Warning)
	}
	assertNoPwValues(t, rows)
}

// The decision per place comes from the live set: the automatic rewrite, a
// user decision, a keep. Decided never exceeds Decidable.
func TestRotatePasswordDecisions(t *testing.T) {
	f := newPwFx(t)
	f.putPassword(t, pwRootGroup, kinds.SourceGenerate, pwNew)
	rows := f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{})
	r := pwRow(t, rows, pwRootGroup)
	if r.Change == nil || r.Change.Kind != "db.password" || r.Change.New != nil {
		t.Fatalf("row change = %+v", r.Change)
	}
	if r.Decided != 3 || r.Decidable != 3 {
		t.Errorf("decided %d/%d, want 3/3", r.Decided, r.Decidable)
	}
	if got := placeLine(r.Places[0]); got != "e1/smongo/MONGO_INITDB_ROOT_PASSWORD own auto" {
		t.Errorf("own line = %s", got)
	}
	assertNoPwValues(t, rows)

	// a keep and a user decision replace the rewrites
	f.put(t, kinds.Keep(tpl("e2", "ssync"), "MONGO_SOURCE", pwRootGroup, now))
	del, err := kinds.DeleteVariable(f.st, tpl("e1", "sworker"), "MONGO_URL", now)
	if err != nil {
		t.Fatal(err)
	}
	del.Group = pwRootGroup
	f.put(t, del)
	r = pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
	got := placeLines(r)
	for _, w := range []string{"e2/ssync/MONGO_SOURCE rewrite proxy kept", "e1/sworker/MONGO_URL rewrite decided"} {
		if !slices.Contains(got, w) {
			t.Errorf("no %q in\n%s", w, strings.Join(got, "\n"))
		}
	}
	if r.Decided != 3 || r.Decided > r.Decidable {
		t.Errorf("decided %d/%d, want 3/3", r.Decided, r.Decidable)
	}

	// a change outside the group shows, is not counted, and blocks
	g := newPwFx(t)
	other, err := kinds.DeleteVariable(g.st, tpl("e1", "sworker"), "MONGO_URL", now)
	if err != nil {
		t.Fatal(err)
	}
	g.put(t, other)
	r = pwRow(t, g.v.Rotate(g.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
	if r.Decided != 0 || r.Blocked != "production · worker MONGO_URL has a pending edit · decide this place" {
		t.Errorf("foreign edit: decided %d blocked %q", r.Decided, r.Blocked)
	}

	// a value rotation change at a place
	other.Group = rotation.GroupKey("fp")
	g = newPwFx(t)
	g.put(t, other)
	r = pwRow(t, g.v.Rotate(g.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
	if r.Blocked != "production · worker MONGO_URL has a pending rotation change · remove it first" {
		t.Errorf("value rotation change: blocked %q", r.Blocked)
	}

	// another group's keep is foreign: not kept, not counted
	g = newPwFx(t)
	g.put(t, kinds.Keep(tpl("e2", "ssync"), "MONGO_SOURCE", "rotate-db:e9/other:u", now))
	r = pwRow(t, g.v.Rotate(g.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
	if p := r.Places[5]; p.Loc.ID != "e2/ssync/MONGO_SOURCE" || p.Kept || p.Pending == nil || r.Decided != 0 ||
		r.Blocked != "staging · sync MONGO_SOURCE has a pending rotation change · remove it first" {
		t.Errorf("foreign keep: place %s decided %d blocked %q", placeLine(p), r.Decided, r.Blocked)
	}

	// an edit of the own variable made elsewhere
	g = newPwFx(t)
	own, err := kinds.SetVariable(g.st, nil, tpl("e1", "smongo"), "MONGO_INITDB_ROOT_PASSWORD", pwNew, now)
	if err != nil {
		t.Fatal(err)
	}
	g.put(t, own)
	r = pwRow(t, g.v.Rotate(g.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
	if r.Places[0].Auto || r.Blocked != "production · mongo MONGO_INITDB_ROOT_PASSWORD has a pending edit · decide this place" {
		t.Errorf("own edit: %s blocked %q", placeLine(r.Places[0]), r.Blocked)
	}
	assertNoPwValues(t, []view.RotateRow{r})
}

func TestRotatePasswordBlocked(t *testing.T) {
	f := newPwFx(t)
	// a URL place in a read-only environment, until kept
	r := pwRow(t, f.v.Rotate(f.cs, pwOpts, []string{"e1"}, view.RotateRuns{}), pwRootGroup)
	if r.Blocked != "staging · sync is read only · Settings w or k keeps it" {
		t.Errorf("blocked = %q", r.Blocked)
	}
	if r.Decidable != 3 {
		t.Errorf("decidable = %d, want 3 (k is allowed read only)", r.Decidable)
	}
	f.put(t, kinds.Keep(tpl("e2", "ssync"), "MONGO_SOURCE", pwRootGroup, now))
	r = pwRow(t, f.v.Rotate(f.cs, pwOpts, []string{"e1"}, view.RotateRuns{}), pwRootGroup)
	if r.Blocked != "" {
		t.Errorf("kept: blocked = %q", r.Blocked)
	}

	// the database's own environment read only
	r = pwRow(t, f.v.Rotate(f.cs, pwOpts, []string{"e2"}, view.RotateRuns{}), pwRootGroup)
	if r.Blocked != "production · mongo is read only · Settings w" {
		t.Errorf("own read only: blocked = %q", r.Blocked)
	}

	// an unclear URL without a decision
	g := newPwFx(t, pwRec("e1", "sworker", "ODD", "mongodb://mongo:pw-0123@x?y@mongo.railway.internal:27017/db"))
	r = pwRow(t, g.v.Rotate(g.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
	if !slices.Contains(placeLines(r), "e1/sworker/ODD rewrite unclear") {
		t.Fatalf("places = %v", placeLines(r))
	}
	if r.Blocked != "production · worker ODD: set this place by hand" {
		t.Errorf("unclear: blocked = %q", r.Blocked)
	}
	del, err := kinds.DeleteVariable(g.st, tpl("e1", "sworker"), "ODD", now)
	if err != nil {
		t.Fatal(err)
	}
	del.Group = pwRootGroup
	g.put(t, del)
	r = pwRow(t, g.v.Rotate(g.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
	if r.Blocked != "" {
		t.Errorf("decided unclear: blocked = %q", r.Blocked)
	}

	// once the password is decided, a rewritten place without a decision
	h := newPwFx(t)
	h.putPassword(t, pwRootGroup, kinds.SourceGenerate, pwNew)
	vars, sys := pwRecs(pwRec("e1", "sapi", "LATE", "mongodb://mongo:"+pwRoot+"@mongo.railway.internal:27017/x"))
	h.st.ReplaceVariables("p1", vars, sys...)
	r = pwRow(t, h.v.Rotate(h.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
	if r.Blocked != "production · api LATE has no decision · select it or k" {
		t.Errorf("late place: blocked = %q", r.Blocked)
	}
}

// stalledRun is a failed run of the root password past its database step:
// worker redeployed, api and sync not.
func stalledRun(id, state string) apply.RunInfo {
	return apply.RunInfo{ID: id, State: state, Steps: map[string]apply.StepRecord{
		"preflight:e1/smongo:mongo": {Status: apply.StatusDone, Acted: true},
		"dbpw:e1/smongo:mongo":      {Status: apply.StatusDone, Acted: true},
		"patch:e1":                  {Status: apply.StatusDone},
		"patch:e2":                  {Status: apply.StatusFailed},
		"redeploy:e1/sworker":       {Status: apply.StatusDone},
		"redeploy:e1/sapi":          {Status: apply.StatusFailed},
		"redeploy:e2/ssync":         {Status: apply.StatusBlocked},
		"redeploy:e9/elsewhere":     {Status: apply.StatusPending},
		"dbverify:e1/smongo:mongo":  {Status: apply.StatusBlocked},
	}}
}

func TestRotatePasswordStalled(t *testing.T) {
	f := newPwFx(t)
	at := func(runs view.RotateRuns) view.RotateRow {
		return pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, runs), pwRootGroup)
	}
	r := at(view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunFailed)}})
	if !r.Stalled || r.Changed || r.Run == nil {
		t.Fatalf("failed acted run: stalled %v changed %v run %+v (stalled wins)", r.Stalled, r.Changed, r.Run)
	}
	if r.Run.ID != "r1" || r.Run.Verified || r.Run.Redeploys != 3 || r.Run.NotRedeployed != 2 {
		t.Errorf("run = %+v, want r1 not verified, 2 of 3 not redeployed", *r.Run)
	}
	if u := pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunFailed)}}), pwAppGroup); u.Stalled || u.Run != nil {
		t.Errorf("another password of the database is stalled")
	}

	// interrupted: running on disk, not applying here
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunRunning)}}); !r.Stalled {
		t.Errorf("interrupted run not stalled")
	}
	// applying here: not stalled; not changed until it finishes (no leak time)
	applying := map[string]string{"r1": apply.RunRunning}
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunRunning)}, Live: applying}); r.Stalled || r.Changed || r.Run == nil || !r.Run.Applying {
		t.Errorf("applying run: stalled %v changed %v run %+v", r.Stalled, r.Changed, r.Run)
	}
	// the live state wins over the index's: a run that just finished here
	// is no stalled one
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunRunning)}, Live: map[string]string{"r1": apply.RunFinished}}); r.Stalled || !r.Changed || r.Run.State != apply.RunFinished {
		t.Errorf("finished here: stalled %v changed %v run %+v", r.Stalled, r.Changed, r.Run)
	}
	// abandoned runs are not read
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunAbandoned)}}); r.Stalled || r.Changed || r.Run != nil {
		t.Errorf("abandoned run read: %+v", r.Run)
	}

	// failed before the database step acted: not stalled, not changed
	early := stalledRun("r2", apply.RunFailed)
	early.Steps["dbpw:e1/smongo:mongo"] = apply.StepRecord{Status: apply.StatusBlocked}
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{early}}); r.Stalled || r.Changed || r.Run != nil {
		t.Errorf("unacted run: stalled %v changed %v run %+v", r.Stalled, r.Changed, r.Run)
	}
	// the database step failed after it may have acted: stalled
	maybe := stalledRun("r3", apply.RunFailed)
	maybe.Steps["dbpw:e1/smongo:mongo"] = apply.StepRecord{Status: apply.StatusFailed, Acted: true}
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{maybe}}); !r.Stalled {
		t.Errorf("acted, failed dbpw not stalled")
	}

	// a password changed by hand: the pre-flight is the database step
	hand := stalledRun("r4", apply.RunFailed)
	delete(hand.Steps, "dbpw:e1/smongo:mongo")
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{hand}}); !r.Stalled {
		t.Errorf("hand run past its pre-flight not stalled")
	}
	hand.Steps["preflight:e1/smongo:mongo"] = apply.StepRecord{Status: apply.StatusFailed}
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{hand}}); r.Stalled {
		t.Errorf("hand run failing its pre-flight stalled")
	}

	// finished: changed and verified
	done := stalledRun("r5", apply.RunFinished)
	for k, rec := range done.Steps {
		rec.Status = apply.StatusDone
		done.Steps[k] = rec
	}
	r = at(view.RotateRuns{Runs: []apply.RunInfo{done}})
	if r.Stalled || !r.Changed || r.Run == nil || !r.Run.Verified || r.Run.NotRedeployed != 0 {
		t.Errorf("finished run: stalled %v changed %v run %+v", r.Stalled, r.Changed, r.Run)
	}
	// a finished revert set the old password back: never changed, with or
	// without a leak time (its frozen db.password has no fingerprint)
	reverted := done
	reverted.ID = "r7" // the memo keys a run's frozen set by its id
	reverted.Frozen = []apply.FrozenChange{{ID: "rv", Kind: "db.password", Group: pwRootGroup, Target: "mongo", Place: fplace("e1", "smongo")}}
	if r := at(view.RotateRuns{Runs: []apply.RunInfo{reverted}}); r.Stalled || r.Changed || r.Run == nil {
		t.Errorf("finished revert: stalled %v changed %v run %+v", r.Stalled, r.Changed, r.Run)
	}
	since := pwOpts
	since.Since = reverted.Created.Add(-time.Hour)
	if r := pwRow(t, f.v.Rotate(f.cs, since, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{reverted}}), pwRootGroup); r.Changed {
		t.Errorf("finished revert after the leak time reads changed")
	}
	// the newest run that acted wins
	r = at(view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r6", apply.RunFailed), done}})
	if !r.Stalled || r.Run.ID != "r6" {
		t.Errorf("newest acted run not read: %+v", r.Run)
	}
	assertNoPwValues(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunFailed)}}))
}

// The memo key follows whatever the stalled state reads.
func TestRotatePasswordMemoRuns(t *testing.T) {
	f := newPwFx(t)
	runs := view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunRunning)}}
	f.v.Rotate(f.cs, pwOpts, pwWrite, runs)
	f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{stalledRun("r1", apply.RunRunning)}})
	if n := f.v.RotateBuilds(); n != 1 {
		t.Fatalf("builds = %d, want 1", n)
	}
	steps := []func(r *apply.RunInfo){
		func(r *apply.RunInfo) {
			r.Steps["redeploy:e1/sapi"] = apply.StepRecord{Status: apply.StatusDone}
		},
		func(r *apply.RunInfo) {
			r.Steps["dbverify:e1/smongo:mongo"] = apply.StepRecord{Status: apply.StatusDone}
		},
		func(r *apply.RunInfo) { r.State = apply.RunFailed },
		func(r *apply.RunInfo) {
			r.Steps["dbpw:e1/smongo:mongo"] = apply.StepRecord{Status: apply.StatusFailed}
		},
	}
	run := stalledRun("r1", apply.RunRunning)
	want := 1
	for i, edit := range steps {
		edit(&run)
		f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}})
		want++
		if n := f.v.RotateBuilds(); n != want {
			t.Errorf("edit %d: builds = %d, want %d", i, n, want)
		}
	}
	f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}, Live: map[string]string{"r1": apply.RunRunning}})
	if n := f.v.RotateBuilds(); n != want+1 {
		t.Errorf("applying: builds = %d, want %d", n, want+1)
	}
	run.Created = now
	f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}, Live: map[string]string{"r1": apply.RunRunning}})
	if n := f.v.RotateBuilds(); n != want+2 {
		t.Errorf("created: builds = %d, want %d", n, want+2)
	}
	// the same index read again: no rebuild
	runs = view.RotateRuns{Runs: []apply.RunInfo{run}}
	f.v.Rotate(f.cs, pwOpts, pwWrite, runs)
	f.v.Rotate(f.cs, pwOpts, pwWrite, runs)
	if n := f.v.RotateBuilds(); n != want+3 {
		t.Errorf("same index: builds = %d, want %d", n, want+3)
	}
}

// With a leak time, Changed needs the acting run created after it.
func TestRotatePasswordChangedSince(t *testing.T) {
	f := newPwFx(t)
	o := pwOpts
	o.Since = now
	run := stalledRun("r1", apply.RunRunning)
	live := map[string]string{"r1": apply.RunRunning}
	for _, tc := range []struct {
		created time.Time
		want    bool
	}{{now.Add(-time.Hour), false}, {now.Add(time.Hour), true}} {
		run.Created = tc.created
		r := pwRow(t, f.v.Rotate(f.cs, o, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}, Live: live}), pwRootGroup)
		if r.Changed != tc.want || r.Stalled {
			t.Errorf("created %v: changed %v stalled %v, want changed %v", tc.created, r.Changed, r.Stalled, tc.want)
		}
	}
}

// A KEYFILE run acted once a patch of its owner's environment is done; it
// stalls while it is unfinished and not verified.
func TestRotateKeyFileStalled(t *testing.T) {
	f := newPwFx(t, pwMember()...)
	const kfGroup = "rotate-db:e1/smongo:@keyfile"
	run := apply.RunInfo{ID: "k1", State: apply.RunFailed, Steps: map[string]apply.StepRecord{
		"patch:e1":                    {Status: apply.StatusDone},
		"redeploy:e1/smongo":          {Status: apply.StatusDone},
		"redeploy:e1/smongo2":         {Status: apply.StatusFailed},
		"dbverify:e1/smongo:@keyfile": {Status: apply.StatusBlocked},
	}}
	at := func(run apply.RunInfo) view.RotateRow {
		return pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}}), kfGroup)
	}
	r := at(run)
	if !r.Stalled || r.Run == nil || r.Run.Redeploys != 2 || r.Run.NotRedeployed != 1 {
		t.Fatalf("keyfile run: stalled %v run %+v", r.Stalled, r.Run)
	}
	if root := pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}}), pwRootGroup); root.Run != nil {
		t.Errorf("the root row read the KEYFILE run")
	}
	run.Steps["patch:e1"] = apply.StepRecord{Status: apply.StatusFailed}
	if r := at(run); r.Run != nil || r.Stalled {
		t.Errorf("no patch done: run %+v", r.Run)
	}
	// a patch Railway committed before the step failed may have written
	run.Steps["patch:e1"] = apply.StepRecord{Status: apply.StatusFailed, PatchID: "pt1"}
	if r := at(run); !r.Stalled || r.Run == nil {
		t.Errorf("committed-but-failed patch: stalled %v run %+v", r.Stalled, r.Run)
	}
	run.Steps["patch:e1"] = apply.StepRecord{Status: apply.StatusDone}
	run.Steps["dbverify:e1/smongo:@keyfile"] = apply.StepRecord{Status: apply.StatusDone}
	if r := at(run); r.Stalled || !r.Run.Verified {
		t.Errorf("verified: stalled %v run %+v", r.Stalled, r.Run)
	}
	run.State = apply.RunFinished
	if r := at(run); r.Stalled || !r.Changed {
		t.Errorf("finished: stalled %v changed %v", r.Stalled, r.Changed)
	}

	// read only: the KEYFILE's members are written with the owner
	r = pwRow(t, f.v.Rotate(f.cs, pwOpts, []string{"e2"}, view.RotateRuns{}), kfGroup)
	if len(r.Password.DB.Members) != 1 || r.Blocked != "production · mongo is read only · Settings w" {
		t.Errorf("read-only keyfile: members %v blocked %q", r.Password.DB.Members, r.Blocked)
	}
}

// A user row whose URLs carry different passwords has no single current
// password: it is blocked until every URL place is decided by hand.
func TestRotatePasswordUserMixedBlocked(t *testing.T) {
	f := newPwFx(t, pwRec("e1", "sapi", "APP_DB", "mongodb://app:other-app-pw-0001@mongo.railway.internal:27017/app"))
	r := pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{}), pwAppGroup)
	if r.Blocked != "the user's URLs hold different passwords · decide each place" {
		t.Errorf("blocked = %q", r.Blocked)
	}
	for _, loc := range []string{"e1/sapi/APP_DB", "e1/sworker/APP_DB"} {
		parts := strings.SplitN(loc, "/", 3)
		k := kinds.Keep(tpl(parts[0], parts[1]), parts[2], pwAppGroup, time.Now())
		if err := f.cs.Put(k, nil); err != nil {
			t.Fatal(err)
		}
	}
	r = pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{}), pwAppGroup)
	if strings.Contains(r.Blocked, "different passwords") {
		t.Errorf("decided places still blocked: %q", r.Blocked)
	}
}

// A pending revert (decided, or held by a revert run that failed before its
// database step acted) needs no decision per place: the row is not blocked
// by "has no decision", as kinds.CheckPasswordGroups.
func TestRotatePasswordRevertPendingNotBlocked(t *testing.T) {
	for _, tc := range []struct {
		source, runID string
		blocked       bool
	}{
		{"revert", "", false},
		{"revert", "r9", false},
		{"generate", "", true},
	} {
		f := newPwFx(t)
		pw := changeset.Change{
			ID: "pw-" + tc.source, Kind: "db.password", Place: fplace("e1", "smongo"), Target: "mongo",
			Group: pwRootGroup, New: json.RawMessage(`{"source":"` + tc.source + `"}`),
		}
		if err := f.cs.Put(pw, nil); err != nil {
			t.Fatal(err)
		}
		if tc.runID != "" {
			if err := f.cs.Lock([]string{pw.ID}, tc.runID); err != nil {
				t.Fatal(err)
			}
		}
		r := pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{}), pwRootGroup)
		if (r.Blocked != "") != tc.blocked || r.Change == nil {
			t.Errorf("%s run %q: blocked %q, change %v", tc.source, tc.runID, r.Blocked, r.Change != nil)
		}
	}
}

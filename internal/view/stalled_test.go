package view_test

import (
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/view"
)

func fplace(env, svc string) domain.Place {
	return domain.Place{ProjectID: "p1", EnvID: env, ServiceID: svc}
}

// frozenRun is stalledRun with what List reads of its frozen set: the root
// password change (with the new password's fingerprint), its rewrites at
// worker and sync, an unrelated change, and the frozen redeploys — worker
// done, the database owner's failed, sync blocked, an unrelated change's
// pending. Neither patch wrote anything (e1 and e2 failed before Railway
// committed).
func frozenRun(id, state string) apply.RunInfo {
	run := stalledRun(id, state)
	run.Steps["patch:e1"] = apply.StepRecord{Status: apply.StatusFailed}
	run.Steps["patch:e2"] = apply.StepRecord{Status: apply.StatusFailed}
	run.Frozen = []apply.FrozenChange{
		{ID: "c-pw", Kind: "db.password", Group: pwRootGroup, Target: "mongo", Place: fplace("e1", "smongo"), Fingerprint: rotation.Fingerprint(pwNew)},
		{ID: "c-worker", Kind: "variable.set", Group: pwRootGroup, Target: "MONGO_URL", Place: fplace("e1", "sworker")},
		{ID: "c-sync", Kind: "variable.set", Group: pwRootGroup, Target: "MONGO_SOURCE", Place: fplace("e2", "ssync")},
		{ID: "c-other", Kind: "variable.set", Group: "other", Target: "X", Place: fplace("e1", "sapi")},
	}
	run.Actions = []apply.FrozenAction{
		{Key: "redeploy:e1/sworker", Serves: []string{"c-worker"}},
		{Key: "redeploy:e1/smongo", Serves: []string{"c-pw"}},
		{Key: "redeploy:e2/ssync", Serves: []string{"c-sync"}},
		{Key: "redeploy:e9/elsewhere", Serves: []string{"c-other"}},
	}
	run.Steps["redeploy:e1/smongo"] = apply.StepRecord{Status: apply.StatusFailed}
	return run
}

// N of M comes from the run's frozen actions, not the live row's places,
// and counts apps: the database owner's redeploy is not one.
func TestRotatePasswordFrozenRedeploys(t *testing.T) {
	f := newPwFx(t)
	r := pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunFailed)}}), pwRootGroup)
	if !r.Stalled || r.Run.Redeploys != 2 || r.Run.NotRedeployed != 1 {
		t.Errorf("run = %+v, want stalled, 1 of 2 app redeploys not done (worker done, sync blocked)", r.Run)
	}
}

// Revert is offered on a stalled root row only while no place holds the
// stalled run's new password.
func TestRotatePasswordRevertOffered(t *testing.T) {
	at := func(f *fx, run apply.RunInfo) view.RotateRow {
		return pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}}), pwRootGroup)
	}
	f := newPwFx(t)
	if r := at(f, frozenRun("r1", apply.RunFailed)); !r.Stalled || !r.Revert {
		t.Errorf("no place holds the new password: stalled %v revert %v", r.Stalled, r.Revert)
	}
	// one environment's patch is done: its places hold the new password
	// even before the store reads it again
	run := frozenRun("r1", apply.RunFailed)
	run.Steps["patch:e2"] = apply.StepRecord{Status: apply.StatusDone, Acted: true}
	if r := at(f, run); r.Revert {
		t.Errorf("revert offered after a patch of the row's environment was done")
	}
	// a patch Railway committed before the step failed polling: it holds
	// a patch id, and the store has not read the write yet
	run = frozenRun("r1", apply.RunFailed)
	run.Steps["patch:e2"] = apply.StepRecord{Status: apply.StatusFailed, PatchID: "patch-1"}
	if r := at(f, run); r.Revert {
		t.Errorf("revert offered after a committed patch that failed polling")
	}
	// a patch step that started, or whose Data records a committed change
	// of the group
	run = frozenRun("r1", apply.RunFailed)
	run.Steps["patch:e2"] = apply.StepRecord{Status: apply.StatusStarted}
	if r := at(f, run); r.Revert {
		t.Errorf("revert offered while a patch of the row started")
	}
	run = frozenRun("r1", apply.RunFailed)
	run.Steps["patch:e2"] = apply.StepRecord{Status: apply.StatusFailed, Data: []byte(`{"committed":["c-sync"]}`)}
	if r := at(f, run); r.Revert {
		t.Errorf("revert offered after a committed change of the group")
	}
	run.Steps["patch:e2"] = apply.StepRecord{Status: apply.StatusFailed, Data: []byte(`{"committed":`)}
	if r := at(f, run); r.Revert {
		t.Errorf("revert offered with unreadable patch data")
	}
	run.Steps["patch:e2"] = apply.StepRecord{Status: apply.StatusFailed, Data: []byte(`{"committed":["c-other"]}`)}
	if r := at(f, run); !r.Revert {
		t.Errorf("revert withheld for another group's committed change")
	}
	// another environment's patch does not count
	run = frozenRun("r1", apply.RunFailed)
	run.Steps["patch:e7"] = apply.StepRecord{Status: apply.StatusDone}
	if r := at(f, run); !r.Revert {
		t.Errorf("revert withheld for a patch outside the row")
	}
	// no fingerprint (a run that froze none, or a revert): withheld
	run = frozenRun("r1", apply.RunFailed)
	run.Frozen[0].Fingerprint = ""
	if r := at(f, run); r.Revert {
		t.Errorf("revert offered without the new password's fingerprint")
	}
	// not stalled: withheld
	if r := at(f, frozenRun("r1", apply.RunFinished)); r.Revert {
		t.Errorf("revert offered on a finished run")
	}

	// a place holds the new password: withheld
	held := newPwFx(t)
	vars, sys := pwRecs()
	for i := range vars {
		if vars[i].ServiceID == "ssync" {
			vars[i].Unrendered = "mongodb://mongo:" + pwNew + "@a.proxy.rlwy.net:11111/?replicaSet=rs0"
			vars[i].Rendered = vars[i].Unrendered
		}
	}
	held.st.ReplaceVariables("p1", vars, sys...)
	if r := at(held, frozenRun("r1", apply.RunFailed)); !r.Stalled || r.Revert {
		t.Errorf("a URL holds the new password: stalled %v revert %v", r.Stalled, r.Revert)
	}
	vars, sys = pwRecs()
	for i := range vars {
		if vars[i].ServiceID == "smongo" && vars[i].Name == "MONGO_INITDB_ROOT_PASSWORD" {
			vars[i].Unrendered, vars[i].Rendered = pwNew, pwNew
		}
	}
	held.st.ReplaceVariables("p1", vars, sys...)
	if r := at(held, frozenRun("r1", apply.RunFailed)); r.Revert {
		t.Errorf("the database's own variable holds the new password: revert offered")
	}
	assertNoPwValues(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunFailed)}}))
}

// A run executing in another app instance is neither stalled nor
// revertable; the row says where it runs.
func TestRotatePasswordElsewhere(t *testing.T) {
	f := newPwFx(t)
	run := frozenRun("r1", apply.RunRunning)
	run.Elsewhere = true
	r := pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}}), pwRootGroup)
	if r.Stalled || r.Revert || r.Run == nil || !r.Run.Elsewhere || r.Run.Applying {
		t.Errorf("elsewhere: stalled %v revert %v run %+v", r.Stalled, r.Revert, r.Run)
	}
	run.Elsewhere = false
	r = pwRow(t, f.v.Rotate(f.cs, pwOpts, pwWrite, view.RotateRuns{Runs: []apply.RunInfo{run}}), pwRootGroup)
	if !r.Stalled || r.Run.Elsewhere {
		t.Errorf("interrupted: stalled %v run %+v", r.Stalled, r.Run)
	}
}

// The banner's stalled runs come from the runs index alone.
func TestStalledRuns(t *testing.T) {
	f := newPwFx(t, pwMember()...)
	got := f.v.Stalled(view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunFailed)}})
	want := []view.StalledRun{{RunID: "r1", Group: pwRootGroup, Label: "mongo root", Redeploys: 2, NotRedeployed: 1}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Stalled = %+v, want %+v", got, want)
	}

	none := []struct {
		name string
		runs view.RotateRuns
	}{
		{"applying here", view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunRunning)}, Live: map[string]string{"r1": apply.RunRunning}}},
		{"finished", view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunFinished)}}},
		{"abandoned", view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunAbandoned)}}},
		{"finished here", view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunRunning)}, Live: map[string]string{"r1": apply.RunFinished}}},
	}
	unacted := frozenRun("r1", apply.RunFailed)
	unacted.Steps["dbpw:e1/smongo:mongo"] = apply.StepRecord{Status: apply.StatusFailed}
	none = append(none, struct {
		name string
		runs view.RotateRuns
	}{"not acted", view.RotateRuns{Runs: []apply.RunInfo{unacted}}})
	for _, c := range none {
		if got := f.v.Stalled(c.runs); len(got) != 0 {
			t.Errorf("%s: Stalled = %+v", c.name, got)
		}
	}
	if got := f.v.Stalled(view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunRunning)}}); len(got) != 1 || got[0].Elsewhere {
		t.Errorf("interrupted run not stalled: %+v", got)
	}
	// executing in another instance: listed as running elsewhere, with
	// its pid, not dropped
	elsewhere := frozenRun("r1", apply.RunRunning)
	elsewhere.Elsewhere, elsewhere.HolderPID = true, 4242
	if got := f.v.Stalled(view.RotateRuns{Runs: []apply.RunInfo{elsewhere}}); len(got) != 1 || !got[0].Elsewhere || got[0].HolderPID != 4242 {
		t.Errorf("elsewhere: %+v", got)
	}

	// a hand run (no dbpw step) past its pre-flight; a KEYFILE run patched
	// and not verified; without frozen actions no counts
	hand := stalledRun("h1", apply.RunFailed)
	delete(hand.Steps, "dbpw:e1/smongo:mongo")
	kf := apply.RunInfo{ID: "k1", State: apply.RunFailed, Steps: map[string]apply.StepRecord{
		"patch:e1":                    {Status: apply.StatusDone},
		"dbverify:e1/smongo:@keyfile": {Status: apply.StatusBlocked},
	}, Frozen: []apply.FrozenChange{{ID: "k", Kind: "db.keyfile", Group: "rotate-db:e1/smongo:@keyfile", Place: fplace("e1", "smongo")}}}
	got = f.v.Stalled(view.RotateRuns{Runs: []apply.RunInfo{kf, hand}})
	if len(got) != 2 || got[0].Label != "mongo KEYFILE" || !got[0].KeyFile || got[1].RunID != "h1" || got[1].Label != "database mongo" || got[1].Redeploys != 0 {
		t.Errorf("Stalled = %+v", got)
	}
	// a KEYFILE run acted only once a patch of its group's frozen
	// environments is done
	kfOther := kf
	kfOther.Steps = map[string]apply.StepRecord{
		"patch:e2":                    {Status: apply.StatusDone},
		"patch:e1":                    {Status: apply.StatusFailed},
		"dbverify:e1/smongo:@keyfile": {Status: apply.StatusBlocked},
	}
	if got := f.v.Stalled(view.RotateRuns{Runs: []apply.RunInfo{kfOther}}); len(got) != 0 {
		t.Errorf("KEYFILE run acted by another environment's patch: %+v", got)
	}
	// a patch of its environment Railway committed before the step failed
	kfCommitted := kf
	kfCommitted.Steps = map[string]apply.StepRecord{
		"patch:e1":                    {Status: apply.StatusFailed, PatchID: "pt1"},
		"dbverify:e1/smongo:@keyfile": {Status: apply.StatusBlocked},
	}
	if got := f.v.Stalled(view.RotateRuns{Runs: []apply.RunInfo{kfCommitted}}); len(got) != 1 || !got[0].KeyFile {
		t.Errorf("KEYFILE run with a committed-but-failed patch: %+v", got)
	}
	kf.Steps["dbverify:e1/smongo:@keyfile"] = apply.StepRecord{Status: apply.StatusDone}
	if got := f.v.Stalled(view.RotateRuns{Runs: []apply.RunInfo{kf}}); len(got) != 0 {
		t.Errorf("verified KEYFILE run stalled: %+v", got)
	}

	// memoized on the runs index and the store
	runs := view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunFailed)}}
	f.v.Stalled(runs)
	n := f.v.StalledBuilds()
	f.v.Stalled(runs)
	if f.v.StalledBuilds() != n {
		t.Errorf("Stalled rebuilt for the same index")
	}
	f.v.Stalled(view.RotateRuns{Runs: runs.Runs, Live: map[string]string{"r1": apply.RunRunning}})
	if f.v.StalledBuilds() != n+1 {
		t.Errorf("Stalled not rebuilt for a new live state")
	}

	// labels are sanitized
	cat := pwCatalog()
	cat.Projects[0].Services[0].Name = "mon\x1b[31mgo"
	f.st.ReplaceCatalog(cat)
	if got := f.v.Stalled(view.RotateRuns{Runs: []apply.RunInfo{frozenRun("r1", apply.RunFailed)}}); len(got) != 1 || strings.ContainsRune(got[0].Label, 0x1b) || !strings.HasPrefix(got[0].Label, "mon") || !strings.HasSuffix(got[0].Label, " root") {
		t.Errorf("label = %q", got[0].Label)
	}
}

func TestDBActed(t *testing.T) {
	if !view.DBActed(frozenRun("r1", apply.RunFailed)) {
		t.Errorf("acted dbpw not reported")
	}
	run := frozenRun("r1", apply.RunFailed)
	run.Steps["dbpw:e1/smongo:mongo"] = apply.StepRecord{Status: apply.StatusPending}
	if view.DBActed(run) {
		t.Errorf("unacted dbpw reported")
	}
	if view.DBActed(apply.RunInfo{Steps: map[string]apply.StepRecord{"patch:e1": {Status: apply.StatusDone}}}) {
		t.Errorf("a run without a database step reported")
	}
}

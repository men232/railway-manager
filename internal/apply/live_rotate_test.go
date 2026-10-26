package apply_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
)

// The live checks of Rotate. Both need LIVE_RAILWAY=1; the write
// test also needs LIVE_WRITE=1. They log counts and "changed: true/false"
// only, never a value, a name, a fingerprint or a group key.

const (
	liveRotateName = livePrefix + "ROTATE"
	// liveRotateOld is a fixed, non-secret fixture: 24 lowercase hex chars.
	liveRotateOld = "0123456789abcdef01234567"
)

// TestLiveRotationGroups reads the live env and runs the rotation analysis.
// It writes nothing: its client refuses every write.
func TestLiveRotationGroups(t *testing.T) {
	liveSkip(t)
	api := liveReadClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cat, err := api.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	st := store.New()
	st.ReplaceCatalog(cat)
	// Discover as the app's inventory does, so system values come along.
	tg := railway.DiscoveryTarget{ProjectID: liveProject}
	for _, p := range cat.Projects {
		if p.ID != liveProject {
			continue
		}
		tg.ProjectName = p.Name
		for _, e := range p.Environments {
			if e.ID == liveEnv {
				tg.Environments = []railway.Environment{e}
			}
		}
	}
	if len(tg.Environments) == 0 {
		t.Fatal("live env not in the catalog")
	}
	res, err := api.Discover(ctx, tg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	st.ReplaceVariables(liveProject, res.Vars, res.System...)

	a := rotation.Analyze(st, rotation.Options{EnvIDs: []string{liveEnv}})
	if len(a.Groups) == 0 {
		t.Fatal("no rotation groups in the live env")
	}
	t.Logf("databases: %d", len(a.DBs))
	for i, db := range a.DBs {
		t.Logf("database %d: %s, hosts %d", i, db.Kind, len(db.Hosts))
		if len(db.Hosts) == 0 {
			t.Errorf("database %d (%s) has no hosts: no credential URL can match it", i, db.Kind)
		}
	}
	classes := map[rotation.Class]int{}
	for _, g := range a.Groups {
		classes[g.Class]++
	}
	for _, c := range []rotation.Class{rotation.ClassDB, rotation.ClassURL, rotation.ClassGen, rotation.ClassAsk, rotation.ClassHidden} {
		t.Logf("groups %s: %d", c, classes[c])
	}
	sections := map[rotation.Section]int{}
	for _, r := range rotation.Rows(a) {
		sections[r.Section]++
	}
	for _, s := range []struct {
		name string
		sec  rotation.Section
	}{
		{"generated", rotation.SectionGenerated}, {"paste", rotation.SectionPaste},
		{"added", rotation.SectionAdded}, {"database", rotation.SectionDatabase},
	} {
		t.Logf("rows %s: %d", s.name, sections[s.sec])
	}
}

// TestLiveRotateGenerate adds RM_TEST_ROTATE at one dev place, rotates it as
// the Rotate screen does (grouped by the old fingerprint, origin rotate, a
// generated value), applies it through the pipeline, requires the
// fingerprint to change, and deletes it. The cleanup deletes it even when the
// test fails; it is registered only after RM_TEST_ROTATE is seen absent.
func TestLiveRotateGenerate(t *testing.T) {
	liveSkip(t)
	if os.Getenv("LIVE_WRITE") != "1" {
		t.Skip("set LIVE_WRITE=1 (with LIVE_RAILWAY=1) to write RM_TEST_ROTATE to the real Railway")
	}
	rig := newLiveRig(t)
	// Checked before the cleanup is registered: a pre-existing one is not
	// this test's to delete.
	if _, exists := rig.read(t)[liveRotateName]; exists {
		t.Fatalf("%s already exists", liveRotateName)
	}
	rig.cleanup(t, liveRotateName)
	loc := domain.LocationID(liveEnv, rig.serviceID, liveRotateName)

	add, err := kinds.SetVariable(rig.st, nil, rig.place, liveRotateName, liveRotateOld, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, add, true)
	v := rig.st.Variable(loc)
	if v == nil {
		t.Fatalf("%s missing from the store after add", liveRotateName)
	}
	oldFP := v.Fingerprint

	nv, err := rotation.Generate(len(liveRotateOld), true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := kinds.SetVariable(rig.st, nil, rig.place, liveRotateName, nv, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c.Group = rotation.GroupKey(oldFP)
	c.Origin = &changeset.Origin{Producer: "rotate", Row: oldFP, Letter: "a"}
	rig.runOne(t, c, true)

	v = rig.st.Variable(loc)
	if v == nil {
		t.Fatalf("%s missing from the store after rotate", liveRotateName)
	}
	changed := v.Fingerprint != oldFP
	t.Logf("changed: %v", changed)
	if !changed {
		t.Error("fingerprint did not change after the rotate change was applied")
	}

	del, err := kinds.DeleteVariable(rig.st, rig.place, liveRotateName, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig.runOne(t, del, true)
	if _, ok := rig.read(t)[liveRotateName]; ok {
		t.Errorf("%s still present after delete", liveRotateName)
	}
}

package kinds

import (
	"context"
	"github.com/men232/railway-manager/internal/store"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/rotation"
)

func TestDBKeyFileChange(t *testing.T) {
	s := dbStore(t)
	c, err := DBKeyFile(s, dbRow(t, s, mongoKey), now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "db.keyfile" || c.Place != dbPlace("smongo") || c.Target != rotation.KeyFileUser || c.Group != mongoKey {
		t.Errorf("change = %+v", c)
	}
	k := NewRegistry()[c.Kind]
	if k.IsNoop(c) || k.Overlay(c).Short == "" {
		t.Errorf("overlay = %+v", k.Overlay(c))
	}
	var places []string
	for _, p := range k.Redeploys(s, c) {
		places = append(places, p.Key())
	}
	if !reflect.DeepEqual(places, []string{"e1/smongo", "e1/smongo2"}) {
		t.Errorf("Redeploys = %v, want every member", places)
	}
	if err := NewRegistry().CheckData(c); err != nil {
		t.Errorf("CheckData: %v", err)
	}
	if _, err := DBKeyFile(s, dbRow(t, s, mongoRoot), now); err == nil {
		t.Error("a password row is not a KEYFILE row")
	}
}

func TestDBKeyFileCompileAndVerify(t *testing.T) {
	s := dbStore(t)
	kf, err := DBKeyFile(s, dbRow(t, s, mongoKey), now)
	if err != nil {
		t.Fatal(err)
	}
	set := func(svc string) changeset.Change {
		c := mustSet(t, s, dbPlace(svc), "KEYFILE", keyNew)
		c.Group = mongoKey
		return c
	}
	g := compileDB(t, s, kf, set("smongo"), set("smongo2"))
	ver, ok := g.Steps["dbverify:e1/smongo:@keyfile"]
	if !ok {
		t.Fatalf("no keyfile verify in %v", g.Order)
	}
	for _, k := range []string{"redeploy:e1/smongo", "redeploy:e1/smongo2"} {
		st, ok := g.Steps[k]
		if !ok {
			t.Errorf("no %s", k)
			continue
		}
		if w, _ := asWaiter(st); !w {
			t.Errorf("%s does not wait for SUCCESS", k)
		}
		if !slices.Contains(st.Deps(), "patch:e1") {
			t.Errorf("%s deps = %v", k, st.Deps())
		}
		if !slices.Contains(ver.Deps(), k) {
			t.Errorf("verify deps %v lack %s", ver.Deps(), k)
		}
	}
	if !reflect.DeepEqual(ver.Serves(), []string{kf.ID}) {
		t.Errorf("verify serves %v", ver.Serves())
	}

	r := dbRunner(t)
	onMongo(r, "replSetGetStatus", mongoOld, okOut("PRIMARY"))
	env := dbEnv(t, dbRailway(t), s, r, kf)
	res, err := ownStep(t, kf, "dbverify").Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[kf.ID].Status != "done" {
		t.Errorf("outcomes = %v", res.Outcomes)
	}
	scanRun(t, env, r)

	r = dbRunner(t)
	onMongo(r, "replSetGetStatus", mongoOld, okOut("LOST_MULTI"))
	env = dbEnv(t, dbRailway(t), s, r, kf)
	_, err = ownStep(t, kf, "dbverify").Run(context.Background(), env, apply.StepRecord{})
	if err == nil || !strings.Contains(err.Error(), "repair by hand") {
		t.Errorf("err = %v", err)
	}
	scanRun(t, env, r)
}

func TestKeepKind(t *testing.T) {
	reg := NewRegistry()
	c := Keep(dbPlace("sapi"), "MONGO_SOURCE", mongoRoot, now)
	if c.Kind != "variable.keep" || c.Target != "MONGO_SOURCE" || c.Group != mongoRoot || c.ID == "" {
		t.Errorf("change = %+v", c)
	}
	k := reg[c.Kind]
	if k.IsNoop(c) {
		t.Error("keep is not a no-op")
	}
	if got := k.Overlay(c); got.Short != "kept · old password stops working" || got.Tone != "danger" {
		t.Errorf("overlay = %+v", got)
	}
	if len(k.OwnSteps(c)) != 0 || k.Redeploys(dbStore(t), c) != nil || len(k.Writes(c)) != 0 {
		t.Error("keep has no steps, redeploys or writes")
	}
	if _, ok := k.Patch(c); ok {
		t.Error("keep has no patch")
	}
	if !reg.Marker(c) {
		t.Error("keep is a marker")
	}
	if err := reg.CheckData(c); err != nil {
		t.Errorf("CheckData: %v", err)
	}
	g := compileDB(t, dbStore(t), c)
	if len(g.Steps) != 0 {
		t.Errorf("steps = %v", g.Order)
	}
}

// A KEYFILE copy outside the known members refuses the keyfile change, at
// creation and in Validate.
func TestDBKeyFileCopyOutsideMembers(t *testing.T) {
	s := dbStore(t)
	c, err := DBKeyFile(s, dbRow(t, s, mongoKey), now)
	if err != nil {
		t.Fatal(err)
	}
	s.ReplaceVariables("p1", append(dbVars(), dbRec("sapi", "MONGO_KEY", keyOld, "")), dbSystem()...)
	if _, err := DBKeyFile(s, dbRow(t, s, mongoKey), now); err == nil || err.Error() != "a KEYFILE copy outside the known members" {
		t.Errorf("DBKeyFile: err = %v", err)
	}
	if err := NewRegistry()[c.Kind].Validate(s, c); err == nil || err.Error() != "a KEYFILE copy outside the known members" {
		t.Errorf("Validate: err = %v", err)
	}
}

// Mongo takes a KEYFILE of 6 to 1024 base64 characters: the decision and
// every write of a KEYFILE group refuse anything else, whoever made it.
func TestKeyFileFormatEnforced(t *testing.T) {
	s := dbStore(t)
	for _, bad := range []string{"abc", "abc!def-ghi", strings.Repeat("a", 1025)} {
		if _, err := NewPasswordChanges(s, nil, dbRow(t, s, mongoKey), SourcePaste, bad, now); err == nil || strings.Contains(err.Error(), bad) {
			t.Errorf("KEYFILE of %d chars accepted or quoted: %v", len(bad), err)
		}
	}
	cs := mustPasswordChanges(t, s, nil, mongoKey, SourcePaste, "abc+/=XYZ0123456789")
	reg := NewRegistry()
	for _, c := range cs {
		if err := reg.CheckData(c); err != nil {
			t.Errorf("%s %s: %v", c.Kind, c.Target, err)
		}
	}
	edited := mustSet(t, s, dbPlace("smongo"), "KEYFILE", "not a keyfile!")
	edited.Group = mongoKey
	if err := reg.CheckData(edited); err == nil {
		t.Error("a KEYFILE group's set with a bad value passed ValidateData")
	}
	edited.Group = ""
	if err := reg.CheckData(edited); err != nil {
		t.Errorf("an ungrouped set is not a KEYFILE: %v", err)
	}
}

// A KEYFILE change whose row the store no longer finds is refused, not
// waved through (M7).
func TestDBKeyFileValidateRowGone(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoKey, SourceGenerate, keyNew)
	gone := store.New()
	gone.ReplaceCatalog(dbCatalog())
	if err := (dbKeyFileKind{}).Validate(gone, cs[0]); err == nil {
		t.Error("Validate passed without the KEYFILE's row")
	}
	if err := (dbKeyFileKind{}).Validate(s, cs[0]); err != nil {
		t.Errorf("Validate with the row: %v", err)
	}
}

package kinds

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
)

// dbStoreWith is dbStore with extra variables.
func dbStoreWith(t *testing.T, extra ...railway.VarRecord) *store.Store {
	t.Helper()
	s := store.New()
	s.ReplaceCatalog(dbCatalog())
	s.ReplaceVariables("p1", append(dbVars(), extra...), dbSystem()...)
	return s
}

func byTarget(t *testing.T, cs []changeset.Change, svc, target string) changeset.Change {
	t.Helper()
	for _, c := range cs {
		if c.Place.ServiceID == svc && c.Target == target {
			return c
		}
	}
	t.Fatalf("no change %s %s", svc, target)
	return changeset.Change{}
}

func mustPasswordChanges(t *testing.T, s *store.Store, pending []changeset.Change, group, source, value string) []changeset.Change {
	t.Helper()
	cs, err := NewPasswordChanges(s, pending, dbRow(t, s, group), source, value, now)
	if err != nil {
		t.Fatalf("NewPasswordChanges: %v", err)
	}
	for _, c := range cs {
		if c.Group != group {
			t.Errorf("%s %s: group %q", c.Kind, c.Target, c.Group)
		}
	}
	return cs
}

// The cross-environment case: MONGO_SOURCE, a literal root URL in an app, is
// rewritten with the new password and waits for the database.
func TestNewPasswordChangesMongoSource(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoRoot, SourceGenerate, mongoNew)
	db := cs[0]
	if db.Kind != kindDBPassword || db.Target != "root" {
		t.Fatalf("first = %s %s", db.Kind, db.Target)
	}
	src := byTarget(t, cs, "sapi", "MONGO_SOURCE")
	want := "mongodb://root:" + mongoNew + "@mongo.railway.internal:27017/db?authSource=admin"
	if v, _ := SetValue(src); v != want {
		t.Error("MONGO_SOURCE is not the rewritten URL")
	}
	if !reflect.DeepEqual(src.Requires, []string{db.ID}) {
		t.Errorf("MONGO_SOURCE requires %v", src.Requires)
	}
	own := byTarget(t, cs, "smongo", "MONGO_INITDB_ROOT_PASSWORD")
	if v, _ := SetValue(own); v != mongoNew || !reflect.DeepEqual(own.Requires, []string{db.ID}) {
		t.Errorf("own var: requires %v", own.Requires)
	}
	// Followers (references) take no change; the database change is not
	// duplicated.
	if len(cs) != 3 {
		var got []string
		for _, c := range cs {
			got = append(got, c.Kind+" "+c.Place.ServiceID+" "+c.Target)
		}
		t.Errorf("changes = %v, want db.password, own var, MONGO_SOURCE", got)
	}
	compileDB(t, s, cs...)

	// A user row has no own variable: its URL alone.
	cs = mustPasswordChanges(t, s, nil, mongoApp, SourcePaste, appNew)
	if len(cs) != 2 {
		t.Fatalf("user changes = %d", len(cs))
	}
	if v, _ := SetValue(byTarget(t, cs, "sapi", "APP_DB")); v != "mongodb://app:"+appNew+"@mongo.railway.internal:27017/app" {
		t.Error("APP_DB is not the rewritten URL")
	}
}

func TestNewPasswordChangesDecidedPlaces(t *testing.T) {
	s := dbStoreWith(t, dbRec("sweb", "MONGO_TWO", "mongodb://root:"+mongoOld+"@mongo.railway.internal:27017/two", ""),
		dbRec("sjob", "MONGO_THREE", "mongodb://root:"+mongoOld+"@mongo.railway.internal:27017/three", ""))
	pasted := mustSet(t, s, dbPlace("sapi"), "MONGO_SOURCE", "mongodb://root:"+mongoNew+"@elsewhere:27017/db")
	pasted.Group = mongoRoot
	del, err := DeleteVariable(s, dbPlace("sweb"), "MONGO_TWO", now)
	if err != nil {
		t.Fatal(err)
	}
	del.Group = mongoRoot
	keep := Keep(dbPlace("sjob"), "MONGO_THREE", mongoRoot, now)
	pending := []changeset.Change{pasted, del, keep}

	cs := mustPasswordChanges(t, s, pending, mongoRoot, SourcePaste, mongoNew)
	db := cs[0]
	got := byTarget(t, cs, "sapi", "MONGO_SOURCE")
	if got.ID != pasted.ID || !reflect.DeepEqual(got.Requires, []string{db.ID}) {
		t.Errorf("pasted place: id %s requires %v", got.ID, got.Requires)
	}
	if v, _ := SetValue(got); !strings.Contains(v, "@elsewhere") {
		t.Error("the pasted URL was replaced by a rewrite")
	}
	for _, c := range []changeset.Change{byTarget(t, cs, "sweb", "MONGO_TWO"), byTarget(t, cs, "sjob", "MONGO_THREE")} {
		if len(c.Requires) != 0 {
			t.Errorf("%s %s requires %v", c.Kind, c.Target, c.Requires)
		}
	}
	if k := byTarget(t, cs, "sjob", "MONGO_THREE"); k.Kind != kindKeep {
		t.Errorf("keep became %s", k.Kind)
	}
}

func TestNewPasswordChangesHand(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoRoot, SourceHand, mongoNew)
	if src := byTarget(t, cs, "sapi", "MONGO_SOURCE"); !reflect.DeepEqual(src.Requires, []string{cs[0].ID}) {
		t.Errorf("hand URL requires %v", src.Requires)
	}
	g := compileDB(t, s, cs...)
	if d := g.Steps["patch:e1"].Deps(); !slices.Contains(d, "preflight:e1/smongo:root") {
		t.Errorf("hand patch deps = %v", d)
	}
}

// A decision again keeps the db.password id the set already holds, so the
// places requiring it stay true once the set merges the new change.
func TestNewPasswordChangesKeepsPendingID(t *testing.T) {
	s := dbStore(t)
	first := mustPasswordChanges(t, s, nil, mongoRoot, SourceGenerate, mongoNew)
	again := mustPasswordChanges(t, s, first, mongoRoot, SourcePaste, mongoNew+"x")
	if again[0].ID != first[0].ID {
		t.Errorf("db.password id %s, want %s", again[0].ID, first[0].ID)
	}
	if src := byTarget(t, again, "sapi", "MONGO_SOURCE"); !reflect.DeepEqual(src.Requires, []string{first[0].ID}) {
		t.Errorf("requires %v", src.Requires)
	}
}

func TestNewPasswordChangesUnclearPlace(t *testing.T) {
	odd := dbRec("sweb", "ODD", "mongodb://root:pw-0123@x?y@mongo.railway.internal:27017/db", "")
	s := dbStoreWith(t, odd)
	_, err := NewPasswordChanges(s, nil, dbRow(t, s, mongoRoot), SourceGenerate, mongoNew, now)
	if err == nil || err.Error() != "production · web ODD: set this place by hand" {
		t.Fatalf("err = %v", err)
	}
	noSecrets(t, "error", err.Error())

	set := mustSet(t, s, dbPlace("sweb"), "ODD", "mongodb://root:"+mongoNew+"@mongo.railway.internal:27017/db")
	set.Group = mongoRoot
	cs := mustPasswordChanges(t, s, []changeset.Change{set}, mongoRoot, SourceGenerate, mongoNew)
	if c := byTarget(t, cs, "sweb", "ODD"); c.ID != set.ID || len(c.Requires) != 1 {
		t.Errorf("decided ODD: %+v", c.Requires)
	}
}

func TestNewPasswordChangesRefusals(t *testing.T) {
	s := dbStore(t)
	if _, err := NewPasswordChanges(s, nil, dbRow(t, s, pgRoot), SourcePaste, "pässwort-123456", now); err == nil || !strings.Contains(err.Error(), "ASCII") {
		t.Errorf("non-ASCII Postgres paste: %v", err)
	}
	row := dbRow(t, s, mongoRoot)
	row.Problem = "root user unknown"
	if _, err := NewPasswordChanges(s, nil, row, SourceGenerate, mongoNew, now); err == nil || err.Error() != "root user unknown" {
		t.Errorf("problem: %v", err)
	}
}

func TestNewPasswordChangesKeyFile(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoKey, SourceGenerate, keyNew)
	if cs[0].Kind != kindDBKeyFile || len(cs) != 3 {
		t.Fatalf("changes = %d, first %s", len(cs), cs[0].Kind)
	}
	for _, svc := range []string{"smongo", "smongo2"} {
		c := byTarget(t, cs, svc, "KEYFILE")
		if v, _ := SetValue(c); v != keyNew || len(c.Requires) != 0 {
			t.Errorf("%s KEYFILE requires %v", svc, c.Requires)
		}
	}
	// The owner's and every member's KEYFILE land in one patch of the
	// owner's environment: a member is never written without the others.
	g := compileDB(t, s, cs...)
	var patches []string
	for k := range g.Steps {
		if strings.HasPrefix(k, "patch:") {
			patches = append(patches, k)
		}
	}
	if !slices.Equal(patches, []string{"patch:e1"}) {
		t.Fatalf("patch steps = %v, want one in the owner's environment", patches)
	}
	served := g.Steps["patch:e1"].Serves()
	for _, c := range cs[1:] {
		if c.Target == "KEYFILE" && !slices.Contains(served, c.ID) {
			t.Errorf("%s KEYFILE is not in the owner's patch", c.Place.Key())
		}
	}
	if n := len(cs) - 1; n != 2 {
		t.Errorf("KEYFILE writes = %d, want owner and member", n)
	}
}

// checkGroups runs CheckPasswordGroups on cs compiled against s.
func checkGroups(t *testing.T, s *store.Store, writeEnvs []string, set changeset.ChangeSet) error {
	t.Helper()
	g := compileDB(t, s, set.Changes...)
	var actions []apply.FrozenAction
	for _, k := range g.Order {
		if p, ok := strings.CutPrefix(k, "redeploy:"); ok {
			_, svc, _ := strings.Cut(p, "/")
			actions = append(actions, apply.FrozenAction{Key: k, Place: dbPlace(svc), Serves: g.Steps[k].Serves()})
		}
	}
	return CheckPasswordGroups(s, writeEnvs, set, set, actions)
}

func deployed(s *store.Store, svcs ...string) {
	var st []railway.DeploymentState
	for _, svc := range svcs {
		st = append(st, railway.DeploymentState{EnvID: "e1", ServiceID: svc, ActiveID: "d-" + svc, ActiveStatus: "SUCCESS", LatestStatus: "SUCCESS"})
	}
	s.ReplaceStates("p1", []string{"e1"}, st)
}

func TestCheckPasswordGroups(t *testing.T) {
	write := []string{"e1"}
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoRoot, SourceGenerate, mongoNew)
	if err := checkGroups(t, s, write, changeset.ChangeSet{Changes: cs}); err != nil {
		t.Errorf("runnable group: %v", err)
	}

	// The database's own environment read only: refused, keep or not.
	if err := checkGroups(t, s, nil, changeset.ChangeSet{Changes: cs}); err == nil || err.Error() != "production · mongo is read only · Settings w" {
		t.Errorf("read-only database: %v", err)
	}
	keep := Keep(dbPlace("sapi"), "MONGO_SOURCE", mongoRoot, now)
	kept, err := NewPasswordChanges(s, []changeset.Change{keep}, dbRow(t, s, mongoRoot), SourceGenerate, mongoNew, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGroups(t, s, write, changeset.ChangeSet{Changes: kept}); err != nil {
		t.Errorf("kept place: %v", err)
	}

	// A rewritten place left out of the run.
	partial := slices.DeleteFunc(slices.Clone(cs), func(c changeset.Change) bool { return c.Target == "MONGO_SOURCE" })
	if err := checkGroups(t, s, write, changeset.ChangeSet{Changes: partial}); err == nil || !strings.Contains(err.Error(), "production · api MONGO_SOURCE has no decision · select it or k") {
		t.Errorf("missing place: %v", err)
	}

	// A redeploy target without a deployment, once deployments are read.
	deployed(s, "sweb", "sjob")
	if err := checkGroups(t, s, write, changeset.ChangeSet{Changes: cs}); err == nil || err.Error() != "production · api has no deployment to redeploy" {
		t.Errorf("no deployment: %v", err)
	}
	deployed(s, "sweb", "sjob", "sapi")
	if err := checkGroups(t, s, write, changeset.ChangeSet{Changes: cs}); err != nil {
		t.Errorf("deployed: %v", err)
	}

	// A selection without the password change is not a password run.
	if err := checkGroups(t, s, nil, changeset.ChangeSet{Changes: []changeset.Change{keep}}); err != nil {
		t.Errorf("keep alone: %v", err)
	}
}

func TestCheckPasswordGroupsUnclearUndecided(t *testing.T) {
	s := dbStoreWith(t, dbRec("sweb", "ODD", "mongodb://root:pw-0123@x?y@mongo.railway.internal:27017/db", ""))
	// Built before ODD appeared: the store now holds an undecided place.
	cs := mustPasswordChanges(t, dbStore(t), nil, mongoRoot, SourceGenerate, mongoNew)
	if err := checkGroups(t, s, []string{"e1"}, changeset.ChangeSet{Changes: cs}); err == nil || !strings.Contains(err.Error(), "production · web ODD has no decision · select it or k") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckPasswordGroupsKeyFile(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoKey, SourceGenerate, keyNew)
	if err := checkGroups(t, s, []string{"e1"}, changeset.ChangeSet{Changes: cs}); err != nil {
		t.Errorf("keyfile: %v", err)
	}
	off := changeset.ChangeSet{Changes: cs, RedeployOff: []string{dbPlace("smongo2").Key()}}
	if err := checkGroups(t, s, []string{"e1"}, off); err == nil || err.Error() != "turn the redeploy of production · mongo-2 back on: a KEYFILE needs every member restarted" {
		t.Errorf("redeploy off: %v", err)
	}
}

func TestCheckPasswordGroupsProblem(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoKey, SourceGenerate, keyNew)
	// A KEYFILE copy outside the members appears after the decision.
	bad := dbStoreWith(t, dbRec("sapi", "KEYFILE", keyOld, ""))
	row, _ := rotation.RowOf(bad, mongoKey, rotation.Options{EnvIDs: []string{"e1"}})
	if row.Problem == "" {
		t.Fatal("fixture has no problem row")
	}
	err := checkGroups(t, bad, []string{"e1"}, changeset.ChangeSet{Changes: cs})
	if err == nil || err.Error() != row.Problem || !strings.Contains(err.Error(), "KEYFILE") {
		t.Errorf("err = %v, want %q", err, row.Problem)
	}
}

// Deciding again rebuilds every automatic change with the new password and
// keeps the places the user decided.
func TestNewPasswordChangesRedo(t *testing.T) {
	s := dbStore(t)
	first := mustPasswordChanges(t, s, nil, mongoRoot, SourceGenerate, mongoNew)
	for _, c := range first {
		if c.Origin == nil || c.Origin.Producer != "rotate" || c.Origin.Row != mongoRoot || c.Origin.Letter != changeset.LetterRewrite {
			t.Errorf("%s %s: origin %+v", c.Kind, c.Target, c.Origin)
		}
	}
	live := &changeset.ChangeSet{}
	for _, c := range first {
		if err := live.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	const second = "SECOND-mongo-root-pw-5t6y7u8i"
	again := mustPasswordChanges(t, s, live.Changes, mongoRoot, SourcePaste, second)
	for _, c := range again {
		if err := live.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	src := byTarget(t, live.Changes, "sapi", "MONGO_SOURCE")
	if v, _ := SetValue(src); v != "mongodb://root:"+second+"@mongo.railway.internal:27017/db?authSource=admin" {
		t.Error("MONGO_SOURCE still holds the earlier password")
	}
	if v, _ := SetValue(byTarget(t, live.Changes, "smongo", "MONGO_INITDB_ROOT_PASSWORD")); v != second {
		t.Error("own variable still holds the earlier password")
	}
	if !reflect.DeepEqual(src.Requires, []string{first[0].ID}) {
		t.Errorf("requires %v", src.Requires)
	}
	if err := checkGroups(t, s, []string{"e1"}, *live); err != nil {
		t.Errorf("redone group: %v", err)
	}
}

func TestNewPasswordChangesForeignPending(t *testing.T) {
	s := dbStore(t)
	edit := mustSet(t, s, dbPlace("sapi"), "MONGO_SOURCE", "mongodb://root:x@mongo.railway.internal:27017/db")
	_, err := NewPasswordChanges(s, []changeset.Change{edit}, dbRow(t, s, mongoRoot), SourceGenerate, mongoNew, now)
	if err == nil || err.Error() != "production · api MONGO_SOURCE has a pending edit · decide this place" {
		t.Errorf("ungrouped edit: %v", err)
	}
	edit.Group = rotation.GroupKey("fp1")
	_, err = NewPasswordChanges(s, []changeset.Change{edit}, dbRow(t, s, mongoRoot), SourceGenerate, mongoNew, now)
	if err == nil || err.Error() != "production · api MONGO_SOURCE has a pending rotation change · remove it first" {
		t.Errorf("value rotation change: %v", err)
	}
	own := mustSet(t, s, dbPlace("smongo"), "MONGO_INITDB_ROOT_PASSWORD", "typed-in-variables")
	_, err = NewPasswordChanges(s, []changeset.Change{own}, dbRow(t, s, mongoRoot), SourceGenerate, mongoNew, now)
	if err == nil || !strings.Contains(err.Error(), "MONGO_INITDB_ROOT_PASSWORD has a pending edit") {
		t.Errorf("own variable edit: %v", err)
	}
}

func TestNewPasswordChangesReferenceRefused(t *testing.T) {
	s := dbStore(t)
	for _, src := range []string{SourcePaste, SourceHand} {
		if _, err := NewPasswordChanges(s, nil, dbRow(t, s, mongoRoot), src, "abc${{mongo.X}}def", now); err == nil || !strings.Contains(err.Error(), "${{") {
			t.Errorf("%s: %v", src, err)
		}
	}
	if _, err := NewPasswordChanges(s, nil, dbRow(t, s, mongoKey), SourcePaste, "abc${{x}}", now); err == nil {
		t.Error("KEYFILE with ${{")
	}
}

// A Variables edit of a decided place keeps its tie to the password change
// (changeset.Put); a set that lost it is refused.
func TestCheckPasswordGroupsUntied(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoRoot, SourceGenerate, mongoNew)
	live := &changeset.ChangeSet{}
	for _, c := range cs {
		if err := live.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	edit := mustSet(t, s, dbPlace("sapi"), "MONGO_SOURCE", "mongodb://root:"+mongoNew+"@mongo.railway.internal:27017/edited")
	if err := live.Put(edit, nil); err != nil {
		t.Fatal(err)
	}
	if err := checkGroups(t, s, []string{"e1"}, *live); err != nil {
		t.Errorf("edited place: %v", err)
	}
	untied := slices.Clone(live.Changes)
	for i := range untied {
		if untied[i].Target == "MONGO_SOURCE" {
			untied[i].Requires = nil
		}
	}
	if err := checkGroups(t, s, []string{"e1"}, changeset.ChangeSet{Changes: untied}); err == nil || err.Error() != "production · api MONGO_SOURCE is not tied to the password change · decide it again" {
		t.Errorf("untied: %v", err)
	}
}

// The root's own variable edited in Variables after the decision keeps its
// tie, but must still set the database's new password (by fingerprint).
func TestCheckPasswordGroupsOwnVariableEdited(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoRoot, SourceGenerate, mongoNew)
	live := &changeset.ChangeSet{}
	for _, c := range cs {
		if err := live.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	edit := mustSet(t, s, dbPlace("smongo"), "MONGO_INITDB_ROOT_PASSWORD", "edited-by-hand-0123456789abcdef")
	if err := live.Put(edit, nil); err != nil {
		t.Fatal(err)
	}
	const want = "production · mongo MONGO_INITDB_ROOT_PASSWORD no longer matches the new password · decide again"
	if err := checkGroups(t, s, []string{"e1"}, *live); err == nil || err.Error() != want {
		t.Errorf("edited own variable: %v", err)
	}
	// left out of the run: the database would get a password no variable holds
	without := slices.DeleteFunc(slices.Clone(cs), func(c changeset.Change) bool { return c.Target == "MONGO_INITDB_ROOT_PASSWORD" })
	if err := checkGroups(t, s, []string{"e1"}, changeset.ChangeSet{Changes: without}); err == nil || err.Error() != want {
		t.Errorf("own variable left out: %v", err)
	}
}

// A decided place whose password change was removed, or left out.
func TestCheckPasswordGroupsMissingPassword(t *testing.T) {
	s := dbStore(t)
	set := mustSet(t, s, dbPlace("sapi"), "MONGO_SOURCE", "mongodb://root:"+mongoNew+"@elsewhere:27017/db")
	set.Group = mongoRoot
	cs := mustPasswordChanges(t, s, []changeset.Change{set}, mongoRoot, SourceGenerate, mongoNew)
	decided := byTarget(t, cs, "sapi", "MONGO_SOURCE")
	sel := changeset.ChangeSet{Changes: []changeset.Change{decided}}
	err := CheckPasswordGroups(s, []string{"e1"}, sel, sel, nil)
	if err == nil || err.Error() != "production · api MONGO_SOURCE follows a removed password change · decide again or remove it" {
		t.Errorf("removed: %v", err)
	}
	err = CheckPasswordGroups(s, []string{"e1"}, sel, changeset.ChangeSet{Changes: cs}, nil)
	if err == nil || err.Error() != "production · api MONGO_SOURCE needs its password change in this run · select the whole group" {
		t.Errorf("left out: %v", err)
	}
}

func TestPasswordFollowers(t *testing.T) {
	s := dbStore(t)
	keep := Keep(dbPlace("sapi"), "MONGO_SOURCE", mongoRoot, now)
	cs := mustPasswordChanges(t, s, []changeset.Change{keep}, mongoRoot, SourceGenerate, mongoNew)
	auto, decided := PasswordFollowers(cs, cs[0])
	own := byTarget(t, cs, "smongo", "MONGO_INITDB_ROOT_PASSWORD")
	if !reflect.DeepEqual(auto, []string{own.ID}) || decided != 1 {
		t.Errorf("followers = %v, %d decided", auto, decided)
	}
	if auto, n := PasswordFollowers(cs, own); auto != nil || n != 0 {
		t.Errorf("a variable has followers: %v %d", auto, n)
	}
}

// The pre-flight refuses a group whose redeploy target has no latest
// deployment (none, or the newest REMOVED); a crashed one passes.
func TestPreflightRedeployTargets(t *testing.T) {
	s := dbStore(t)
	cs := mustPasswordChanges(t, s, nil, mongoRoot, SourceGenerate, mongoNew)
	actions := []apply.FrozenAction{
		{Key: "redeploy:e1/sapi", Place: dbPlace("sapi"), Serves: []string{byTarget(t, cs, "sapi", "MONGO_SOURCE").ID}},
		{Key: "redeploy:e1/sweb", Place: dbPlace("sweb"), Serves: []string{byTarget(t, cs, "smongo", "MONGO_INITDB_ROOT_PASSWORD").ID}},
		{Key: "redeploy:e1/sjob", Place: dbPlace("sjob"), Serves: []string{"another-group"}},
	}
	run := func(f *railwaytest.Fake) error {
		r := dbRunner(t)
		onMongo(r, "", mongoOld, okOut("PRIMARY"))
		env := dbEnv(t, f, s, r, cs...)
		env.Actions = actions
		_, err := ownStep(t, cs[0], "preflight").Run(context.Background(), env, apply.StepRecord{})
		return err
	}
	f := dbRailway(t)
	f.AddDeployment("e1", "sapi", "d1", "CRASHED", now)
	if err := run(f); err == nil || !strings.Contains(err.Error(), "production · web has no deployment to redeploy") {
		t.Errorf("no deployment: %v", err)
	}
	f.AddDeployment("e1", "sweb", "d2", "REMOVED", now)
	if err := run(f); err == nil || !strings.Contains(err.Error(), "production · web has no deployment") {
		t.Errorf("removed: %v", err)
	}
	f = dbRailway(t)
	f.AddDeployment("e1", "sapi", "d1", "CRASHED", now)
	f.AddDeployment("e1", "sweb", "d2", "SUCCESS", now)
	if err := run(f); err != nil && strings.Contains(err.Error(), "deployment") {
		t.Errorf("deployed: %v", err)
	}
}

// A user row whose URLs hold different passwords runs only with every URL
// place decided by hand.
func TestCheckPasswordGroupsUserMixed(t *testing.T) {
	s := dbStoreWith(t, dbRec("sweb", "APP_DB", "mongodb://app:other-app-old-1@mongo.railway.internal:27017/app", ""))
	cs := mustPasswordChanges(t, s, nil, mongoApp, SourcePaste, appNew)
	err := checkGroups(t, s, []string{"e1"}, changeset.ChangeSet{Changes: cs})
	if err == nil || err.Error() != "the user's URLs hold different passwords · decide each place" {
		t.Fatalf("err = %v", err)
	}
	for i := range cs {
		if cs[i].Kind == kindSet {
			cs[i].Origin = &changeset.Origin{Producer: "rotate", Row: mongoApp}
		}
	}
	if err := checkGroups(t, s, []string{"e1"}, changeset.ChangeSet{Changes: cs}); err != nil {
		t.Errorf("decided by hand: %v", err)
	}
}

// A literal copy of the password in the owner's environment is set to the
// new password itself; an unrecognised place (a short copy) refuses the
// decision until decided, then goes with the group, tied when it sets.
func TestPasswordChangesCopiesAndUnrecognised(t *testing.T) {
	s := dbStoreWith(t, dbRec("sweb", "DB_PASS", mongoOld, ""))
	cs := mustPasswordChanges(t, s, nil, mongoRoot, SourceGenerate, mongoNew)
	cp := byTarget(t, cs, "sweb", "DB_PASS")
	if v, _ := SetValue(cp); v != mongoNew || !slices.Contains(cp.Requires, cs[0].ID) || !automatic(cp) {
		t.Errorf("copy: value is new %v, requires %v, automatic %v", v == mongoNew, cp.Requires, automatic(cp))
	}
	if err := checkGroups(t, s, []string{"e1"}, changeset.ChangeSet{Changes: cs}); err != nil {
		t.Errorf("copies: %v", err)
	}

	// A short password: its same-value literal is unrecognised.
	const short = "pw-short-01"
	u := dbStoreWith(t,
		dbRec("sapi", "SHORT_DB", "mongodb://short:"+short+"@mongo.railway.internal:27017/app", ""),
		dbRec("sweb", "SHORT_PASS", short, ""))
	group := rotation.DBGroupKey("e1/smongo", "short")
	if _, err := NewPasswordChanges(u, nil, dbRow(t, u, group), SourceGenerate, appNew, now); err == nil ||
		err.Error() != "production · web SHORT_PASS carries this password · p f c x or k" {
		t.Errorf("undecided unrecognised: %v", err)
	}
	mine := mustSet(t, u, dbPlace("sweb"), "SHORT_PASS", appNew)
	mine.Group = group
	got := mustPasswordChanges(t, u, []changeset.Change{mine}, group, SourceGenerate, appNew)
	d := byTarget(t, got, "sweb", "SHORT_PASS")
	if d.ID != mine.ID || !slices.Contains(d.Requires, got[0].ID) {
		t.Errorf("decided unrecognised: %+v", d)
	}
	if err := checkGroups(t, u, []string{"e1"}, changeset.ChangeSet{Changes: got}); err != nil {
		t.Errorf("decided: %v", err)
	}
	without := slices.DeleteFunc(slices.Clone(got), func(c changeset.Change) bool { return c.ID == mine.ID })
	if err := checkGroups(t, u, []string{"e1"}, changeset.ChangeSet{Changes: without}); err == nil ||
		err.Error() != "production · web SHORT_PASS has no decision · select it or k" {
		t.Errorf("left out: %v", err)
	}
}

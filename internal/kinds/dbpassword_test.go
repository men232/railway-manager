package kinds

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/dbops"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

func TestDBPasswordChange(t *testing.T) {
	s := dbStore(t)
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	if c.Kind != "db.password" || c.Place != dbPlace("smongo") || c.Target != "root" || c.Group != mongoRoot || !c.Created.Equal(now) {
		t.Errorf("change = %+v", c)
	}
	own := s.Variable(domain.LocationID("e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD"))
	if c.Base.Fingerprint != own.Fingerprint || c.Base.Value != "" {
		t.Errorf("base = %+v, want the own variable's fingerprint", c.Base)
	}
	k := NewRegistry()[c.Kind]
	if !k.Secret(c) || k.IsNoop(c) {
		t.Error("db.password must be secret and never a no-op")
	}
	if _, ok := k.(apply.Reopener); !ok || !k.(apply.Reopener).Reopen() {
		t.Error("db.password must reopen")
	}
	if got := k.Overlay(c); got.Short != "→ new password (26 chars)" || got.Long != got.Short || got.Tone != "new" {
		t.Errorf("overlay = %+v", got)
	}
	noSecrets(t, "overlay", k.Overlay(c).Short)
	if k.Redeploys(s, c) != nil {
		t.Errorf("Redeploys = %v", k.Redeploys(s, c))
	}
	if _, ok := k.Patch(c); ok {
		t.Error("db.password has no patch")
	}
	hand := mustDBPassword(t, s, mongoRoot, SourceHand, mongoNew)
	if got := k.Overlay(hand); got.Short != "changed by hand" || got.Tone != "danger" {
		t.Errorf("hand overlay = %+v", got)
	}
	if err := NewRegistry().CheckData(c); err != nil {
		t.Errorf("CheckData: %v", err)
	}

	user := mustDBPassword(t, s, mongoApp, SourcePaste, appNew)
	if user.Target != "app" || user.Group != mongoApp {
		t.Errorf("user change = %+v", user)
	}
	if got := k.Reads(user); !reflect.DeepEqual(got, k.Writes(c)) {
		t.Errorf("a user change reads the root password: Reads = %v, root Writes = %v", got, k.Writes(c))
	}
}

func TestDBPasswordValidate(t *testing.T) {
	s := dbStore(t)
	row := dbRow(t, s, mongoRoot)
	if _, err := DBPassword(s, row, SourceGenerate, mongoOld, now); err == nil || !strings.Contains(err.Error(), "current") {
		t.Errorf("the current password again: err = %v", err)
	}
	if _, err := DBPassword(s, row, "guess", mongoNew, now); err == nil {
		t.Error("an unknown source should be refused")
	}
	if _, err := DBPassword(s, row, SourceGenerate, "", now); err == nil {
		t.Error("an empty password should be refused")
	}

	pg := dbRow(t, s, pgRoot)
	const ascii = "Postgres password must be ASCII here (no SASLprep) · use g to generate"
	if _, err := DBPassword(s, pg, SourcePaste, "pässwörd-0123456789", now); err == nil || err.Error() != ascii {
		t.Errorf("paste non-ASCII: err = %v", err)
	}
	if _, err := DBPassword(s, pg, SourceRevert, pgNew, now); err == nil {
		t.Error("DBPassword must refuse a revert")
	}
	recs := dbVars()
	for i := range recs {
		if recs[i].ServiceID == "spg" && recs[i].Name == "POSTGRES_PASSWORD" {
			recs[i].Unrendered, recs[i].Rendered = "pässwörd-0123456789", "pässwörd-0123456789"
		}
	}
	sNon := dbStore(t)
	sNon.ReplaceVariables("p1", recs, dbSystem()...)
	if _, err := DBPasswordRevert(sNon, dbRow(t, sNon, pgRoot), pgNew, now); err == nil || err.Error() != ascii {
		t.Errorf("revert non-ASCII: err = %v", err)
	}
	if _, err := DBPassword(s, pg, SourceHand, "pässwörd-0123456789", now); err != nil {
		t.Errorf("hand non-ASCII is the database's own: %v", err)
	}
	if err := CheckPastedPassword("postgres", "pässwörd"); err == nil {
		t.Error("CheckPastedPassword postgres non-ASCII")
	}
	if err := CheckPastedPassword("mongo", "pässwörd"); err != nil {
		t.Errorf("CheckPastedPassword mongo: %v", err)
	}

	// The own variable turned into a reference: no longer a root password.
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	recs = dbVars()
	for i := range recs {
		if recs[i].ServiceID == "smongo" && recs[i].Name == "MONGO_INITDB_ROOT_PASSWORD" {
			recs[i].Unrendered = "${{shared.ROOT}}"
		}
	}
	s2 := dbStore(t)
	s2.ReplaceVariables("p1", recs, dbSystem()...)
	if err := NewRegistry()[c.Kind].Validate(s2, c); err == nil || !strings.Contains(err.Error(), "literal") {
		t.Errorf("own var not literal: err = %v", err)
	}

	// Corrupt data never validates.
	bad := c
	bad.New = json.RawMessage(`{"source":"generate","value":"x","db":{"kind":"mongo"},"extra":1}`)
	if err := NewRegistry().CheckData(bad); err == nil {
		t.Error("corrupt New should be refused")
	}
	noSecrets(t, "validate error", errString(NewRegistry().CheckData(bad)))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func stepKeys(steps []apply.Step) []string {
	var out []string
	for _, st := range steps {
		out = append(out, st.Key())
	}
	return out
}

func TestDBPasswordOwnSteps(t *testing.T) {
	s := dbStore(t)
	reg := NewRegistry()
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	steps := reg[c.Kind].OwnSteps(c)
	want := []string{"preflight:e1/smongo:root", "dbpw:e1/smongo:root", "dbverify:e1/smongo:root"}
	if got := stepKeys(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v", got)
	}
	pre, pw, ver := steps[0], steps[1], steps[2]
	if len(pre.Serves()) != 0 || !reflect.DeepEqual(pw.Serves(), []string{c.ID}) || len(ver.Serves()) != 0 {
		t.Errorf("serves = %v %v %v", pre.Serves(), pw.Serves(), ver.Serves())
	}
	if !reflect.DeepEqual(pw.Deps(), []string{pre.Key()}) || !slices.Contains(ver.Deps(), pw.Key()) {
		t.Errorf("deps = %v %v", pw.Deps(), ver.Deps())
	}
	for _, st := range steps {
		r, ok := st.(apply.Reopenable)
		if got := ok && r.Reopenable(); got != (st == pw) {
			t.Errorf("%s Reopenable = %v", st.Key(), got)
		}
		noSecrets(t, "label", st.Label())
	}
	// The verify waits for the redeploys of the row's URL places.
	for _, k := range []string{"redeploy:e1/sapi", "redeploy:e1/sweb"} {
		if !slices.Contains(ver.Deps(), k) {
			t.Errorf("verify deps %v lack %s", ver.Deps(), k)
		}
	}

	hand := mustDBPassword(t, s, mongoRoot, SourceHand, mongoNew)
	steps = reg[hand.Kind].OwnSteps(hand)
	if got := stepKeys(steps); !reflect.DeepEqual(got, []string{"preflight:e1/smongo:root", "dbverify:e1/smongo:root"}) {
		t.Fatalf("hand steps = %v", got)
	}
	if !reflect.DeepEqual(steps[0].Serves(), []string{hand.ID}) || !slices.Contains(steps[1].Deps(), steps[0].Key()) {
		t.Errorf("hand: preflight serves %v, verify deps %v", steps[0].Serves(), steps[1].Deps())
	}

	user := mustDBPassword(t, s, mongoApp, SourcePaste, appNew)
	pre = ownStep(t, user, "preflight")
	if !slices.Contains(pre.Deps(), "dbpw:e1/smongo:root") {
		t.Errorf("user preflight deps = %v, want the root dbpw", pre.Deps())
	}
	if d := ownStep(t, user, "dbpw").Deps(); !slices.Contains(d, "dbpw:e1/smongo:root") {
		t.Errorf("user dbpw deps = %v, want the root dbpw", d)
	}
}

// compileDB compiles a change set as the app does.
func compileDB(t *testing.T, s *store.Store, cs ...changeset.Change) apply.Graph {
	t.Helper()
	reg := NewRegistry()
	g, err := apply.Compile(changeset.ChangeSet{Changes: cs}, s, reg,
		func(envID string, changes []changeset.Change) apply.Step {
			return NewPatchStep(envID, changes, reg, changes[0].Place.ProjectID)
		}, NewRedeployStep)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// rewrite is the URL rewrite of a place requiring db.
func rewrite(t *testing.T, s *store.Store, db changeset.Change, svc, name, value string) changeset.Change {
	t.Helper()
	c := mustSet(t, s, dbPlace(svc), name, value)
	c.Group, c.Requires = db.Group, []string{db.ID}
	return c
}

func TestDBPasswordCompile(t *testing.T) {
	s := dbStore(t)
	db := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	own := rewrite(t, s, db, "smongo", "MONGO_INITDB_ROOT_PASSWORD", mongoNew)
	url := rewrite(t, s, db, "sapi", "MONGO_SOURCE", "mongodb://root:"+mongoNew+"@mongo.railway.internal:27017/db?authSource=admin")
	user := mustDBPassword(t, s, mongoApp, SourcePaste, appNew)
	appURL := rewrite(t, s, user, "sapi", "APP_DB", "mongodb://app:"+appNew+"@mongo.railway.internal:27017/app")
	g := compileDB(t, s, db, own, url, user, appURL)

	deps := func(k string) []string {
		st, ok := g.Steps[k]
		if !ok {
			t.Fatalf("no step %s in %v", k, g.Order)
		}
		return st.Deps()
	}
	if d := deps("patch:e1"); !slices.Contains(d, "dbpw:e1/smongo:root") || !slices.Contains(d, "dbpw:e1/smongo:app") {
		t.Errorf("patch deps = %v, want both dbpw steps", d)
	}
	if d := deps("preflight:e1/smongo:app"); !slices.Contains(d, "dbpw:e1/smongo:root") {
		t.Errorf("user preflight deps = %v", d)
	}
	if d := deps("dbverify:e1/smongo:root"); !slices.Contains(d, "redeploy:e1/sapi") || !slices.Contains(d, "redeploy:e1/sweb") {
		t.Errorf("verify deps = %v, want the redeploys of api and web", d)
	}
	// The own variable redeploys no database place; the follower does.
	for _, k := range []string{"redeploy:e1/smongo", "redeploy:e1/smongo2"} {
		if _, ok := g.Steps[k]; ok {
			t.Errorf("%s: a database is never redeployed for its own password", k)
		}
	}
	for _, k := range []string{"redeploy:e1/sapi", "redeploy:e1/sweb"} {
		st, ok := g.Steps[k]
		if !ok {
			t.Errorf("no %s", k)
			continue
		}
		if w, ok := asWaiter(st); !ok || !w {
			t.Errorf("%s does not wait for SUCCESS", k)
		}
	}
	if got := g.Reopen["dbpw:e1/smongo:root"]; !reflect.DeepEqual(got, []string{"patch:e1"}) {
		t.Errorf("Reopen = %v", g.Reopen)
	}
	if _, ok := g.Reopen["preflight:e1/smongo:root"]; ok {
		t.Error("preflight must not reopen")
	}

	// hand: no dbpw; the writes depend on the preflight.
	hand := mustDBPassword(t, s, mongoRoot, SourceHand, mongoNew)
	own = rewrite(t, s, hand, "smongo", "MONGO_INITDB_ROOT_PASSWORD", mongoNew)
	g = compileDB(t, s, hand, own)
	if _, ok := g.Steps["dbpw:e1/smongo:root"]; ok {
		t.Error("hand has no dbpw step")
	}
	if d := g.Steps["patch:e1"].Deps(); !slices.Contains(d, "preflight:e1/smongo:root") {
		t.Errorf("hand patch deps = %v", d)
	}
}

// asWaiter reads WaitsForSuccess through the compiler's wrapper.
func asWaiter(st apply.Step) (bool, bool) {
	for {
		if w, ok := st.(apply.SuccessWaiter); ok {
			return w.WaitsForSuccess(), true
		}
		u, ok := st.(interface{ Unwrap() apply.Step })
		if !ok {
			return false, false
		}
		st = u.Unwrap()
	}
}

func TestOwnPasswordVarRedeploysNoDatabase(t *testing.T) {
	s := dbStore(t)
	db := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	own := rewrite(t, s, db, "smongo", "MONGO_INITDB_ROOT_PASSWORD", mongoNew)
	var keys []string
	for _, p := range NewRegistry()[own.Kind].Redeploys(s, own) {
		keys = append(keys, p.Key())
	}
	// job copies the root password and REPLICA_SET_NAME but has no
	// KEYFILE: an app, not a member.
	if !reflect.DeepEqual(keys, []string{"e1/sjob", "e1/sweb"}) {
		t.Errorf("own var redeploys %v, want the followers e1/sjob and e1/sweb", keys)
	}
	// Out of a password group, it redeploys as any variable.
	plain := mustSet(t, s, dbPlace("smongo"), "MONGO_INITDB_ROOT_PASSWORD", mongoNew)
	if n := len(NewRegistry()[plain.Kind].Redeploys(s, plain)); n < 3 {
		t.Errorf("plain set redeploys %d places", n)
	}

	// The patch step's fresh redeploys skip the database places too.
	f := dbRailway(t)
	env, _ := newEnv(t, f, s)
	reg := NewRegistry()
	res, err := NewPatchStep("e1", []changeset.Change{own}, reg, "p1").Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if got := moreKeys(res); !reflect.DeepEqual(got, []string{"redeploy:e1/sjob", "redeploy:e1/sweb"}) {
		t.Errorf("fresh redeploys = %v", got)
	}
}

func TestPreflightMongoRoot(t *testing.T) {
	s := dbStore(t)
	f := dbRailway(t)
	r := dbRunner(t)
	onMongo(r, "updateUser", mongoOld, okOut("OK"), "RM_DRY_RUN=1")
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, f, s, r, c)
	st := ownStep(t, c, "preflight")
	res, err := st.Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outcomes) != 0 {
		t.Errorf("outcomes = %v", res.Outcomes)
	}
	for _, c := range r.Calls() {
		if c.Name == "mongosh" && !hasEnv(c, "RM_DRY_RUN=1") {
			t.Errorf("pre-flight ran a change: env %v", len(c.Env))
		}
	}
	if len(f.Mutations()) != 0 {
		t.Errorf("pre-flight wrote to Railway: %v", f.Mutations())
	}
	if p := r.Processes(); len(p) == 0 || !p[0].Stopped() {
		t.Error("the tunnel was not closed")
	}
	if d := env.Journal.Step(st.Key()).Data; strings.Contains(string(d), "rm-tunnel") {
		t.Errorf("tunnel records left after close: %s", d)
	}
	scanRun(t, env, r)
}

func TestPreflightMissingClient(t *testing.T) {
	s := dbStore(t)
	r := &proc.Fake{}
	r.SetPath("ssh", "/usr/bin/ssh")
	r.SetPath("brew", "/opt/homebrew/bin/brew")
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	_, err := ownStep(t, c, "preflight").Run(context.Background(), env, apply.StepRecord{})
	if err == nil || !strings.Contains(err.Error(), "mongosh is not installed") || !strings.Contains(err.Error(), "brew install mongosh") {
		t.Errorf("err = %v", err)
	}
	if len(r.Calls()) != 0 {
		t.Errorf("ran %v with a client missing", r.Calls())
	}
}

func TestPreflightWrongPassword(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	onMongo(r, "updateUser", mongoOld, authFailed(mongoOld))
	onMongo(r, "updateUser", mongoNew, authFailed(mongoNew))
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	_, err := ownStep(t, c, "preflight").Run(context.Background(), env, apply.StepRecord{})
	if err == nil {
		t.Fatal("a refused login must fail pre-flight")
	}
	noSecrets(t, "error", err.Error())
	scanRun(t, env, r)
}

func TestPreflightHand(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	onMongo(r, "connectionStatus", mongoNew, authFailed(mongoNew))
	c := mustDBPassword(t, s, mongoRoot, SourceHand, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	st := ownStep(t, c, "preflight")
	_, err := st.Run(context.Background(), env, apply.StepRecord{})
	if err == nil || !strings.HasPrefix(err.Error(), "the database does not accept the new password") {
		t.Errorf("err = %v", err)
	}
	if env.Journal.Step(st.Key()).Acted {
		t.Error("a refused hand preflight must not mark Acted")
	}
	noSecrets(t, "error", errString(err))

	r = dbRunner(t)
	onMongo(r, "connectionStatus", mongoNew, okOut("OK"))
	env = dbEnv(t, dbRailway(t), s, r, c)
	res, err := st.Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[c.ID].Status != "done" {
		t.Errorf("hand preflight outcome = %v", res.Outcomes)
	}
	if !env.Journal.Step(st.Key()).Acted {
		t.Error("a hand preflight whose new password logs in marks Acted")
	}
	for _, call := range r.Calls() {
		if call.Name == "mongosh" && strings.Contains(call.Args[len(call.Args)-1], "updateUser") {
			t.Error("hand never changes the database")
		}
	}
}

func TestDBPWMongoRoot(t *testing.T) {
	s := dbStore(t)
	f := dbRailway(t)
	r := dbRunner(t)
	var acted bool
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	var env *apply.Env
	r.On("mongosh", func(cmd proc.Cmd) bool {
		if strings.Contains(cmd.Args[len(cmd.Args)-1], "updateUser") && hasEnv(cmd, "RM_MONGO_PASSWORD="+mongoOld) && hasEnv(cmd, "RM_NEW_PASSWORD="+mongoNew) {
			acted = env.Journal.Step("dbpw:e1/smongo:root").Acted
			return true
		}
		return false
	}, okOut("OK"))
	env = dbEnv(t, f, s, r, c)
	st := ownStep(t, c, "dbpw")
	res, err := st.Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if !acted {
		t.Error("dbpw did not mark Acted before the change")
	}
	if res.Outcomes[c.ID].Status != "done" {
		t.Errorf("outcomes = %v", res.Outcomes)
	}
	// Mongo goes through both members: two tunnels, to each service.
	var sshTargets int
	for _, call := range r.Calls() {
		if call.Name == "railway" {
			sshTargets++
		}
	}
	if sshTargets != 2 {
		t.Errorf("ssh targets read = %d, want 2 (owner and member)", sshTargets)
	}
	if len(f.Mutations()) != 0 {
		t.Errorf("dbpw wrote to Railway: %v", f.Mutations())
	}
	scanRun(t, env, r)
}

// A reopened dbpw on a database that already holds the new password: the
// current password fails, the new one logs in and sets it again.
func TestDBPWReopenedAlreadyNew(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	onMongo(r, "updateUser", mongoOld, authFailed(mongoOld))
	onMongo(r, "updateUser", mongoNew, okOut("OK"), "RM_NEW_PASSWORD="+mongoNew)
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	res, err := ownStep(t, c, "dbpw").Run(context.Background(), env, apply.StepRecord{Status: apply.StatusPending, Acted: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[c.ID].Status != "done" {
		t.Errorf("outcomes = %v", res.Outcomes)
	}
	scanRun(t, env, r)
}

// A resumed dbpw re-checks the write environments: one taken out of them
// since the run was frozen gets no new database password (its variables
// could not follow).
func TestDBPWRefusedOutsideWriteEnvs(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	onMongo(r, "updateUser", mongoOld, okOut("OK"))
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	env.API.(*railway.Client).WriteAllowed = func(string) bool { return false }
	st := ownStep(t, c, "dbpw")
	_, err := st.Run(context.Background(), env, apply.StepRecord{Status: apply.StatusPending})
	var wna *railway.WriteNotAllowedError
	if !errors.As(err, &wna) || wna.EnvID != "e1" {
		t.Fatalf("err = %v, want a write refused for e1", err)
	}
	if len(r.Calls()) != 0 {
		t.Errorf("a refused dbpw ran %d commands", len(r.Calls()))
	}
	if env.Journal.Step(st.Key()).Acted {
		t.Error("a refused dbpw marked Acted")
	}
}

// A database behind an ssh host not trusted yet: preflight opens no tunnel,
// fails, and journals the host's keys for the Run screen to show.
func TestDBPreflightUntrustedHost(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	r.On("ssh-keyscan", nil, okOut("ssh.railway.test "+testHostKey))
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	env.KnownHosts = filepath.Join(t.TempDir(), "known_hosts")
	st := ownStep(t, c, "preflight")
	_, err := st.Run(context.Background(), env, apply.StepRecord{})
	var ue *dbops.UntrustedHostError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want an untrusted host", err)
	}
	for _, call := range r.Calls() {
		if call.Name == "ssh" {
			t.Fatal("ssh started for an untrusted host")
		}
	}
	hk, ok := UntrustedHost(env.Journal.Step(st.Key()))
	if !ok || hk.Host != "ssh.railway.test" || !slices.Equal(hk.Fingerprints, []string{testHostKeyFP}) {
		t.Errorf("journaled host keys = %+v, %v", hk, ok)
	}
}

func TestDBPWIncomplete(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	onMongo(r, "updateUser", mongoOld, proc.FakeReply{Stdout: "partial " + mongoNew, Err: exec.ErrWaitDelay})
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	st := ownStep(t, c, "dbpw")
	_, err := st.Run(context.Background(), env, apply.StepRecord{})
	if err == nil || !strings.Contains(err.Error(), "output may be incomplete") {
		t.Fatalf("err = %v", err)
	}
	if !env.Journal.Step(st.Key()).Acted {
		t.Error("an incomplete change must leave Acted")
	}
	noSecrets(t, "error", err.Error())
	scanRun(t, env, r)
}

func TestDBPWReapsRecordedTunnels(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	onMongo(r, "updateUser", mongoOld, okOut("OK"))
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	st := ownStep(t, c, "dbpw")
	// A killed run left a tunnel whose pid is now someone else's, and a
	// record Reap refuses (not a tunnel directory).
	r.SetCmdline(4242, "/usr/bin/vim notes.txt")
	data := json.RawMessage(`{"tunnels":[{"socket":"/tmp/rm-tunnel-gone/mongodb.sock","pid":4242},{"socket":"/etc/rm-tunnel-x/s","pid":4343}]}`)
	if err := env.Journal.Set(st.Key(), apply.StepRecord{Status: apply.StatusFailed, Data: data}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), env, env.Journal.Step(st.Key())); err != nil {
		t.Fatal(err)
	}
	var d tunnelData
	if err := json.Unmarshal(env.Journal.Step(st.Key()).Data, &d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Tunnels, []tunnelRecord{{Socket: "/etc/rm-tunnel-x/s", Pid: 4343}}) || d.Warning == "" {
		t.Errorf("data = %+v, want only the unreapable record, with a warning", d)
	}
}

// A tunnel whose close fails stays journaled with a warning on a step that
// still succeeds; the next step of the change reaps it.
func TestTunnelCloseFailureReapedBySibling(t *testing.T) {
	s := dbStore(t)
	r := &proc.Fake{}
	for _, n := range []string{"ssh", "mongosh", "psql"} {
		r.SetPath(n, "/usr/bin/"+n)
	}
	r.On("railway", nil, proc.FakeReply{Stdout: "Host railway\n  HostName ssh.railway.test\n  User svc-user\n"})
	r.On("psql", func(c proc.Cmd) bool { return slices.Contains(c.Args, "--version") }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
	var dirs []string
	r.On("ssh", func(c proc.Cmd) bool {
		sock := forwardSocket(c)
		listen(t, sock)
		dir := filepath.Dir(sock)
		dirs = append(dirs, dir)
		// The socket cannot be removed: Close fails.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Error(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700); _ = os.RemoveAll(dir) })
		return true
	}, proc.FakeReply{})
	r.On("psql", func(c proc.Cmd) bool { return strings.Contains(c.Stdin, "pg_roles") }, okOut("OK"))
	c := mustDBPassword(t, s, pgRoot, SourceGenerate, pgNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	pre := ownStep(t, c, "preflight")
	if _, err := pre.Run(context.Background(), env, apply.StepRecord{}); err != nil {
		t.Fatalf("a close failure must not fail the step: %v", err)
	}
	var d tunnelData
	_ = json.Unmarshal(env.Journal.Step(pre.Key()).Data, &d)
	if len(d.Tunnels) != 1 || d.Warning == "" {
		t.Fatalf("data = %+v, want the tunnel kept with a warning", d)
	}
	for _, dir := range dirs {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// dbpw of the same change reaps it at its start (its own client fails
	// here; the reap comes first).
	_, _ = ownStep(t, c, "dbpw").Run(context.Background(), env, apply.StepRecord{})
	if data := env.Journal.Step(pre.Key()).Data; len(data) != 0 {
		t.Errorf("preflight records after the sibling's reap: %s", data)
	}
	for _, dir := range dirs[:1] {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("tunnel directory %s not removed", dir)
		}
	}
}

// The tunnel is journaled before anything waits on it: a run killed while
// the tunnel opens leaves the record for the next run to reap.
func TestDBPWRecordsTunnelBeforeWait(t *testing.T) {
	s := dbStore(t)
	r := &proc.Fake{}
	for _, n := range []string{"ssh", "mongosh"} {
		r.SetPath(n, "/usr/bin/"+n)
	}
	r.On("railway", nil, proc.FakeReply{Stdout: "Host railway\n  HostName ssh.railway.test\n  User svc-user\n"})
	r.On("ssh", nil, proc.FakeReply{Pid: 777}) // never listens
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	ctx, cancel := context.WithCancel(context.Background())
	key := "dbpw:e1/smongo:root"
	go func() {
		for !strings.Contains(string(env.Journal.Step(key).Data), `"pid":777`) {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		cancel()
	}()
	_, err := ownStep(t, c, "dbpw").Run(ctx, env, apply.StepRecord{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestDBVerify(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	onMongo(r, "connectionStatus", mongoNew, okOut("OK"))
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	st := ownStep(t, c, "dbverify")
	if _, err := st.Run(context.Background(), env, apply.StepRecord{}); err != nil {
		t.Fatal(err)
	}

	r = dbRunner(t)
	onMongo(r, "connectionStatus", mongoNew, authFailed(mongoNew))
	env = dbEnv(t, dbRailway(t), s, r, c)
	_, err := st.Run(context.Background(), env, apply.StepRecord{})
	if err == nil {
		t.Fatal("a refused new password must fail verify")
	}
	noSecrets(t, "error", err.Error())
	scanRun(t, env, r)
}

// A user's change logs in as root with the new root password once the
// root's dbpw is done in the same run, with the current one otherwise.
func TestUserStepAdminPassword(t *testing.T) {
	s := dbStore(t)
	root := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	user := mustDBPassword(t, s, mongoApp, SourcePaste, appNew)
	for _, tc := range []struct {
		name     string
		rootDone bool
		admin    string
	}{{"root pending", false, mongoOld}, {"root done", true, mongoNew}} {
		r := dbRunner(t)
		// app's URL names database app: the change is to that user
		onMongo(r, "updateUser", tc.admin, okOut("OK"), "RM_TARGET_USER=app", "RM_TARGET_DB=app", "RM_NEW_PASSWORD="+appNew)
		env := dbEnv(t, dbRailway(t), s, r, root, user)
		if tc.rootDone {
			if err := env.Journal.Set("dbpw:e1/smongo:root", apply.StepRecord{Status: apply.StatusDone}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ownStep(t, user, "dbpw").Run(context.Background(), env, apply.StepRecord{}); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		scanRun(t, env, r)
	}
}

func TestPostgresProvesOnPrivateAddress(t *testing.T) {
	s := dbStore(t)
	r := dbRunner(t)
	r.On("psql", func(c proc.Cmd) bool { return hasEnv(c, "PGPASSWORD="+pgOld) && strings.Contains(c.Stdin, "pg_roles") }, okOut("OK"))
	c := mustDBPassword(t, s, pgRoot, SourceGenerate, pgNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	if _, err := ownStep(t, c, "preflight").Run(context.Background(), env, apply.StepRecord{}); err != nil {
		t.Fatal(err)
	}
	var fwd []string
	for _, call := range r.Calls() {
		if call.Name == "ssh" {
			fwd = append(fwd, forwardArg(call))
		}
	}
	if len(fwd) != 1 || !strings.HasSuffix(fwd[0], ":pg.railway.internal:5432") {
		t.Errorf("forwards = %v, want the private address", fwd)
	}
	scanRun(t, env, r)

	// A failing client whose stderr quotes both passwords.
	r = dbRunner(t)
	r.On("psql", func(c proc.Cmd) bool { return c.Stdin != "" }, proc.FakeReply{
		Stderr: "psql: error: FATAL: password authentication failed for user postgres " + pgOld + " " + pgNew + "\n",
		Err:    &proc.ExitError{Code: 2, LastStderr: "FATAL " + pgOld},
	})
	env = dbEnv(t, dbRailway(t), s, r, c)
	for _, prefix := range []string{"preflight", "dbpw", "dbverify"} {
		_, err := ownStep(t, c, prefix).Run(context.Background(), env, apply.StepRecord{})
		if err == nil {
			t.Errorf("%s: no error", prefix)
			continue
		}
		noSecrets(t, prefix+" error", err.Error())
	}
	scanRun(t, env, r)
	for _, call := range r.Calls() {
		noSecrets(t, "stdin of "+call.Name, call.Stdin)
	}
}

// A revert sets the database back to the own variable's value, logging in
// with the abandoned run's password, then with the value itself.
func TestDBPasswordRevert(t *testing.T) {
	s := dbStore(t)
	row := dbRow(t, s, mongoRoot)
	c, err := DBPasswordRevert(s, row, mongoNew, now)
	if err != nil {
		t.Fatal(err)
	}
	k := NewRegistry()[c.Kind]
	if err := k.Validate(s, c); err != nil {
		t.Errorf("Validate: %v", err)
	}
	noSecrets(t, "overlay", k.Overlay(c).Short)
	if _, err := DBPasswordRevert(s, dbRow(t, s, mongoApp), appNew, now); err == nil {
		t.Error("a user password is not reverted")
	}
	if _, err := DBPasswordRevert(s, row, mongoOld, now); err == nil {
		t.Error("a revert logging in with the value itself is refused")
	}
	// The variable changed since: the revert no longer validates.
	recs := dbVars()
	for i := range recs {
		if recs[i].ServiceID == "smongo" && recs[i].Name == "MONGO_INITDB_ROOT_PASSWORD" {
			recs[i].Unrendered, recs[i].Rendered = mongoNew, mongoNew
		}
	}
	s2 := dbStore(t)
	s2.ReplaceVariables("p1", recs, dbSystem()...)
	if err := k.Validate(s2, c); err == nil {
		t.Error("a revert to a value the variable no longer holds must be refused")
	}

	for _, tc := range []struct {
		name  string
		login string // the password the database takes
	}{{"holds the run's password", mongoNew}, {"already back", mongoOld}} {
		r := dbRunner(t)
		if tc.login == mongoOld {
			onMongo(r, "updateUser", mongoNew, authFailed(mongoNew))
		}
		onMongo(r, "updateUser", tc.login, okOut("OK"), "RM_NEW_PASSWORD="+mongoOld)
		onMongo(r, "connectionStatus", mongoOld, okOut("OK"))
		env := dbEnv(t, dbRailway(t), s, r, c)
		for _, prefix := range []string{"dbpw", "dbverify"} {
			if _, err := ownStep(t, c, prefix).Run(context.Background(), env, apply.StepRecord{}); err != nil {
				t.Errorf("%s: %s: %v", tc.name, prefix, err)
			}
		}
		scanRun(t, env, r)
	}
}

// A Mongo change reaches the writable primary even when a member's tunnel
// fails; the keyfile verify needs every member.
func TestMongoMemberTunnelFails(t *testing.T) {
	s := dbStore(t)
	r := &proc.Fake{}
	for _, n := range []string{"ssh", "mongosh"} {
		r.SetPath(n, "/usr/bin/"+n)
	}
	r.On("railway", func(c proc.Cmd) bool { return slices.Contains(c.Args, "smongo2") }, proc.FakeReply{Err: errors.New("exit status 1")})
	r.On("railway", nil, proc.FakeReply{Stdout: "Host railway\n  HostName ssh.railway.test\n  User svc-user\n"})
	r.On("ssh", func(c proc.Cmd) bool { listen(t, forwardSocket(c)); return true }, proc.FakeReply{})
	onMongo(r, "updateUser", mongoOld, okOut("OK"), "RM_NEW_PASSWORD="+mongoNew)
	onMongo(r, "replSetGetStatus", mongoOld, okOut("PRIMARY"))
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	if _, err := ownStep(t, c, "dbpw").Run(context.Background(), env, apply.StepRecord{}); err != nil {
		t.Errorf("dbpw with a member down: %v", err)
	}
	kf, err := DBKeyFile(s, dbRow(t, s, mongoKey), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownStep(t, kf, "dbverify").Run(context.Background(), env, apply.StepRecord{}); err == nil {
		t.Error("the keyfile verify needs every member")
	}
}

// A Postgres app with a same-name reference to the root password is no
// member: it redeploys after the password changes.
func TestPostgresAppFollowsOwnPassword(t *testing.T) {
	s := dbStore(t)
	db := mustDBPassword(t, s, pgRoot, SourceGenerate, pgNew)
	own := rewrite(t, s, db, "spg", "POSTGRES_PASSWORD", pgNew)
	var keys []string
	for _, p := range NewRegistry()[own.Kind].Redeploys(s, own) {
		keys = append(keys, p.Key())
	}
	if !reflect.DeepEqual(keys, []string{"e1/sjob"}) {
		t.Errorf("redeploys = %v, want e1/sjob only", keys)
	}
	env, _ := newEnv(t, dbRailway(t), s)
	res, err := NewPatchStep("e1", []changeset.Change{own}, NewRegistry(), "p1").Run(context.Background(), env, apply.StepRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if got := moreKeys(res); !reflect.DeepEqual(got, []string{"redeploy:e1/sjob"}) {
		t.Errorf("fresh redeploys = %v", got)
	}
}

// With a place's tunnel down, errors still number members by place.
func TestMongoMemberNumbering(t *testing.T) {
	s := dbStore(t)
	r := &proc.Fake{}
	for _, n := range []string{"ssh", "mongosh"} {
		r.SetPath(n, "/usr/bin/"+n)
	}
	r.On("railway", func(c proc.Cmd) bool { return slices.Contains(c.Args, "smongo") }, proc.FakeReply{Err: errors.New("exit status 1")})
	r.On("railway", nil, proc.FakeReply{Stdout: "Host railway\n  HostName ssh.railway.test\n  User svc-user\n"})
	r.On("ssh", func(c proc.Cmd) bool { listen(t, forwardSocket(c)); return true }, proc.FakeReply{})
	onMongo(r, "updateUser", mongoOld, okOut("NOT_PRIMARY"))
	c := mustDBPassword(t, s, mongoRoot, SourceGenerate, mongoNew)
	env := dbEnv(t, dbRailway(t), s, r, c)
	_, err := ownStep(t, c, "dbpw").Run(context.Background(), env, apply.StepRecord{})
	if err == nil || !strings.Contains(err.Error(), "member 2: not primary") || !strings.Contains(err.Error(), "member 1: railway ssh config") {
		t.Errorf("err = %v", err)
	}
	scanRun(t, env, r)
}

func TestPasswordValue(t *testing.T) {
	s := dbStore(t)
	c := mustDBPassword(t, s, mongoRoot, SourcePaste, mongoNew)
	if src, v, ok := PasswordValue(c); !ok || src != SourcePaste || v != mongoNew {
		t.Errorf("PasswordValue = %q, ok %v (value matches: %v)", src, ok, v == mongoNew)
	}
	if _, _, ok := PasswordValue(Keep(dbPlace("sapi"), "MONGO_URL", mongoRoot, now)); ok {
		t.Error("a keep has no password")
	}
	bad := c
	bad.New = json.RawMessage(`{"source":"paste"}`)
	if _, _, ok := PasswordValue(bad); ok {
		t.Error("a change without a value has no password")
	}
}

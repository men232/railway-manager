package apply_test

// Live checks of database passwords against the live target (see
// live_test.go): its Mongo, Postgres and Redis databases, each test skipped
// when the environment has no database of its kind. The two read-only tests
// need the live target only; TestLiveDBRotateRedis writes, and also needs
// LIVE_DB=1 and RM_LIVE_WORKER.
//
// They log a database kind and ok or not ok, never a password, a URL, a
// variable value or a client's output. A failure message may carry a client's
// error text, scrubbed of passwords by dbops and passed through text.Sanitize
// (login and run errors); it can name an address.
//
// RUN TIME: a password rotation redeploys places and waits for SUCCESS, twice.
// Run with a long timeout, e.g.
//
//	LIVE_RAILWAY=1 LIVE_DB=1 go test ./internal/apply -run TestLiveDBRotateRedis -v -timeout 60m
//
// (make live-db). The default 10 minute go test timeout kills the process
// and skips the cleanup.
//
// The original Redis password is held in memory only. If the process is
// killed (SIGKILL, the test timeout, a crash) it is lost, and the database may
// be left with the test password; the cleanup only runs on a normal failure.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/dbops"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/workdir"
)

// liveDB is a database of liveEnv a database password test may change.
type liveDB struct {
	db       rotation.DB
	owner    domain.Place
	kind     string
	rootUser string
	rootVar  string
}

// liveKnownHosts is the operator's own known hosts file (the app's, in its
// work dir): a live run trusts only an ssh host key the operator confirmed
// on the Run screen.
func liveKnownHosts() string {
	home, _ := os.UserHomeDir()
	return dbops.KnownHostsFile(workdir.Dir(os.Getenv, home))
}

func liveDBSkip(t *testing.T) {
	t.Helper()
	liveSkip(t)
	if os.Getenv("LIVE_DB") != "1" {
		t.Skip("set LIVE_DB=1 (with LIVE_RAILWAY=1) to change a database password")
	}
}

// liveOpts scopes the rotation analysis to liveEnv.
func liveOpts() rotation.Options {
	return rotation.Options{EnvIDs: []string{liveEnv}, AllEnvIDs: []string{liveEnv}}
}

// loadLiveDBStore reads liveEnv, system variables included (a Postgres login
// needs RAILWAY_PRIVATE_DOMAIN), into a fresh store.
func loadLiveDBStore(t *testing.T, api *railway.Client) *store.Store {
	t.Helper()
	st := store.New()
	refreshLiveDBStore(t, api, st)
	return st
}

func refreshLiveDBStore(t *testing.T, api *railway.Client, st *store.Store) {
	t.Helper()
	if err := readLiveDBStore(api, st); err != nil {
		t.Fatal(err)
	}
}

func readLiveDBStore(api *railway.Client, st *store.Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cat, err := api.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("ListProjects: %w", err)
	}
	st.ReplaceCatalog(cat)
	for _, p := range cat.Projects {
		if p.ID != liveProject {
			continue
		}
		names := map[string]string{}
		for _, s := range p.Services {
			names[s.ID] = s.Name
		}
		for _, e := range p.Environments {
			if e.ID != liveEnv {
				continue
			}
			res, err := api.Discover(ctx, railway.DiscoveryTarget{
				ProjectID: p.ID, ProjectName: p.Name, Environments: []railway.Environment{e}, ServiceNames: names,
			})
			if err != nil {
				return fmt.Errorf("Discover: %w", err)
			}
			st.ReplaceVariables(liveProject, res.Vars, res.System...)
			return nil
		}
	}
	return fmt.Errorf("environment %s not found in project %s", liveEnv, liveProject)
}

// detectLiveDBs lists the databases of liveEnv by kind; it fails when a kind
// is found twice.
func detectLiveDBs(t *testing.T, st *store.Store) map[string]liveDB {
	t.Helper()
	out := map[string]liveDB{}
	for _, db := range rotation.Analyze(st, liveOpts()).DBs {
		if db.Owner.EnvID != liveEnv || db.Owner.ProjectID != liveProject {
			continue
		}
		if _, dup := out[db.Kind]; dup {
			t.Fatalf("two %s databases in the test environment", db.Kind)
		}
		out[db.Kind] = liveDB{db: db, owner: db.Owner, kind: db.Kind, rootUser: db.RootUser, rootVar: rotation.RootVar(db.Kind)}
	}
	return out
}

// needLiveDB is the database of kind, skipping the test when the environment
// has none.
func needLiveDB(t *testing.T, dbs map[string]liveDB, kind string) liveDB {
	t.Helper()
	d, ok := dbs[kind]
	if !ok {
		t.Skipf("no %s database in the live environment", kind)
	}
	return d
}

// rootPassword is the database's own password variable's value, rendered;
// the caller must not log it.
func (d liveDB) rootPassword(t *testing.T, st *store.Store) string {
	t.Helper()
	v := st.Variable(domain.LocationID(d.owner.EnvID, d.owner.ServiceID, d.rootVar))
	if v == nil || v.Sealed || v.Rendered == "" {
		t.Fatalf("%s: the root password variable cannot be read", d.kind)
	}
	return v.Rendered
}

// login proves password is the root user's, through the real dbops path: the
// ssh target, the kind's forward (Postgres: the private address) and Login.
// loopback forwards Postgres to 127.0.0.1 instead, to see whether it trusts it.
func (d liveDB) login(ctx context.Context, st *store.Store, password string, loopback bool) error {
	r := proc.OS{}
	for _, k := range []string{"ssh", d.kind} {
		if install, missing, err := dbops.Missing(r, k); err != nil {
			return err
		} else if missing {
			return fmt.Errorf("the %s client is not installed · %s", k, install)
		}
	}
	userHost, err := dbops.SSHTarget(ctx, r, d.owner.ProjectID, d.owner.EnvID, d.owner.ServiceID)
	if err != nil {
		return err
	}
	private := st.SystemVar(d.owner.EnvID, d.owner.ServiceID, "RAILWAY_PRIVATE_DOMAIN")
	remote, socket, err := dbops.Forward(d.kind, d.db.Port, private)
	if err != nil {
		return err
	}
	if loopback {
		remote = "127.0.0.1"
	}
	tun, err := dbops.Open(ctx, r, liveKnownHosts(), userHost, d.db.Port, remote, socket, nil)
	if err != nil {
		return err
	}
	loginErr := dbops.Login(ctx, r, dbops.Target{DB: d.db, User: d.rootUser, Root: true}, tun, password)
	if err := errors.Join(loginErr, tun.Close()); err != nil {
		return errors.New(text.Sanitize(err.Error()))
	}
	return nil
}

// TestLiveDBPreflight proves, writing nothing, that each database of the
// live environment can be reached the way a password run
// reaches it: an ssh target, a tunnel and a login with the current root
// password (Postgres through the container's private address).
func TestLiveDBPreflight(t *testing.T) {
	liveSkip(t)
	api := liveReadClient(t)
	st := loadLiveDBStore(t, api)
	dbs := detectLiveDBs(t, st)
	if len(dbs) == 0 {
		t.Skip("no database in the live environment")
	}
	for kind, d := range dbs {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := d.login(ctx, st, d.rootPassword(t, st), false); err != nil {
				t.Fatalf("%s pre-flight failed: %v", kind, err)
			}
			t.Logf("%s: ssh target, tunnel and root login ok", kind)
		})
	}
}

// TestLivePostgresTrustsLoopback documents why proofs avoid loopback: Railway's Postgres
// template trusts 127.0.0.1, so a login there with a wrong password may
// succeed and prove nothing. It logs only whether loopback is trusted; the
// run itself forwards to the private address (checked by the pre-flight).
func TestLivePostgresTrustsLoopback(t *testing.T) {
	liveSkip(t)
	api := liveReadClient(t)
	st := loadLiveDBStore(t, api)
	d := needLiveDB(t, detectLiveDBs(t, st), "postgres")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	wrong := "wrong-" + randomValue(t)
	err := d.login(ctx, st, wrong, true)
	switch {
	case err == nil:
		t.Log("postgres: loopback 127.0.0.1 is TRUSTED: a login with a wrong password succeeded (so proofs must use the private address)")
	case strings.Contains(strings.ToLower(err.Error()), "server did not complete authentication"):
		// Seen live: the server let the login in without asking for a
		// password and psql's require_auth=scram-sha-256 refused that.
		t.Log("postgres: loopback 127.0.0.1 is TRUSTED (psql's require_auth=scram-sha-256 refused it)")
	// This depends on psql's wording ("password authentication failed for
	// user"); another client version may fall to the default case below.
	case strings.Contains(strings.ToLower(err.Error()), "authentication failed"):
		t.Log("postgres: loopback 127.0.0.1 is NOT trusted: a wrong password was refused")
	default:
		t.Fatalf("postgres: the loopback login failed before authentication, no answer on trust (error text not logged)")
	}
	// The private address must refuse the same wrong password.
	if err := d.login(ctx, st, wrong, false); err == nil {
		t.Error("postgres: the private address accepted a wrong password: a password proof proves nothing")
	}
}

// liveDBRig is liveRig with the wiring of a password run: the real clients
// (proc.OS), the password vet and the fingerprint, the write environment
// liveEnv only.
type liveDBRig struct {
	*liveRig
	api *railway.Client
}

func newLiveDBRig(t *testing.T) *liveDBRig {
	t.Helper()
	if liveWorker == "" {
		t.Skip("set RM_LIVE_WORKER to a second app service to run the database password tests")
	}
	rig := newLiveRig(t)
	refreshLiveDBStore(t, rig.api, rig.st)
	rig.place = rig.st.PlaceOf(liveEnv, rig.serviceID)
	rig.extraApps = map[string]bool{}
	cat, err := rig.api.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, p := range cat.Projects {
		for _, s := range p.Services {
			if p.ID == liveProject && s.Name == liveWorker {
				rig.extraApps[s.ID] = true
			}
		}
	}
	if len(rig.extraApps) != 1 {
		t.Fatalf("RM_LIVE_WORKER must name exactly one service, found %d", len(rig.extraApps))
	}
	// Only Redis may be changed: Mongo and Postgres are read-only here.
	rig.dbs = []liveDB{needLiveDB(t, detectLiveDBs(t, rig.st), "redis")}
	st, reg, api := rig.st, kinds.NewRegistry(), rig.api
	rig.runs = &apply.Runs{
		Dir: t.TempDir(), Reg: reg, Exec: &apply.Executor{},
		NewEnv: func(runID string, j *apply.Journal) *apply.Env {
			return &apply.Env{
				API: api, Store: st, Journal: j, RunID: runID, Proc: proc.OS{}, KnownHosts: liveKnownHosts(),
				Sleep: func(ctx context.Context, d time.Duration) error {
					select {
					case <-time.After(d):
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
				Now: time.Now,
			}
		},
		NewRedeploy: kinds.NewRedeployStep,
		Fingerprint: kinds.PasswordFingerprint,
		Snapshot:    func() *store.Store { return st },
		Compile: func(_ *store.Store, cs changeset.ChangeSet) (apply.Graph, error) {
			return apply.Compile(cs, st, reg,
				func(envID string, changes []changeset.Change) apply.Step {
					return kinds.NewPatchStep(envID, changes, reg, changes[0].Place.ProjectID)
				}, kinds.NewRedeployStep)
		},
		Vet: func(s *store.Store, sel, live changeset.ChangeSet, actions []apply.FrozenAction) error {
			return kinds.CheckPasswordGroups(s, []string{liveEnv}, sel, live, actions)
		},
	}
	return &liveDBRig{liveRig: rig, api: rig.api}
}

// prepareRotation prepares the run that sets d's root password to value
// (pasted): every change is checked by assertLiveSafe's rules (safe) before
// anything is frozen. Errors name no value.
func (r *liveDBRig) prepareRotation(d liveDB, value string) (string, rotation.PasswordRow, error) {
	group := rotation.DBGroupKey(d.owner.Key(), d.rootUser)
	row, ok := rotation.RowOf(r.st, group, liveOpts())
	if !ok {
		return "", row, fmt.Errorf("%s: no password row %s", d.kind, group)
	}
	if row.Problem != "" {
		return "", row, fmt.Errorf("%s: the password row has a problem: %s", d.kind, row.Problem)
	}
	cs, err := kinds.NewPasswordChanges(r.st, nil, row, kinds.SourcePaste, value, time.Now())
	if err != nil {
		return "", row, fmt.Errorf("%s: NewPasswordChanges: %w", d.kind, err)
	}
	if err := r.liveUnsafe(cs...); err != nil {
		return "", row, err
	}
	live := &changeset.ChangeSet{}
	for _, c := range cs {
		if err := live.Put(c, nil); err != nil {
			return "", row, err
		}
	}
	id, conflicts, err := r.runs.Prepare(live, nil)
	if err != nil || len(conflicts) > 0 {
		return "", row, fmt.Errorf("%s: Prepare: %v, conflicts %v", d.kind, err, conflicts)
	}
	return id, row, nil
}

// rotateTo is prepareRotation failing the test.
func (r *liveDBRig) rotateTo(t *testing.T, d liveDB, value string) (string, rotation.PasswordRow) {
	t.Helper()
	id, row, err := r.prepareRotation(d, value)
	if err != nil {
		t.Fatal(err)
	}
	return id, row
}

// run executes (or resumes) a prepared run and requires it finished. It does
// not read the environment back: the caller does, so a failed read never
// hides a finished run.
func (r *liveDBRig) run(ctx context.Context, id string) error {
	rep, err := r.runs.Execute(ctx, id, func(apply.Event) {})
	if err != nil {
		return fmt.Errorf("Execute %s: %s", id, text.Sanitize(err.Error()))
	}
	if rep.State != apply.RunFinished {
		return fmt.Errorf("run %s state = %s, want %s (done %v, kept %v)", id, rep.State, apply.RunFinished, rep.Done, rep.Kept)
	}
	return nil
}

// holds reports whether the variable at loc holds password inside its value,
// read live (never logged).
func (r *liveDBRig) holds(ctx context.Context, p domain.Place, name, password string) (bool, error) {
	vars, err := r.api.ReadVariables(ctx, liveProject, liveEnv, p.ServiceID, true)
	if err != nil {
		return false, err
	}
	return strings.Contains(vars[name], password), nil
}

// verify checks a finished rotation to password: the database's own variable
// and every rewritten place hold it, the database takes it, and (old != "")
// refuses the old one. It reports what failed by place kind, never a value.
func (r *liveDBRig) verify(ctx context.Context, d liveDB, row rotation.PasswordRow, password, old string) error {
	var errs []error
	if row.Own != nil {
		if ok, err := r.holds(ctx, row.Own.Place, row.Own.Name, password); err != nil || !ok {
			errs = append(errs, fmt.Errorf("the database's own variable does not hold the password (read error: %v)", err))
		}
	}
	for _, u := range row.URLs {
		if u.Mode != rotation.URLRewrite {
			continue
		}
		if ok, err := r.holds(ctx, u.Loc.Place, u.Loc.Name, password); err != nil || !ok {
			errs = append(errs, fmt.Errorf("a rewritten URL place does not hold the password (read error: %v)", err))
		}
	}
	if err := d.login(ctx, r.st, password, false); err != nil {
		errs = append(errs, fmt.Errorf("the database refuses the new password: %w", err))
	}
	if old != "" && old != password {
		if err := d.login(ctx, r.st, old, false); err == nil {
			errs = append(errs, errors.New("the database still takes the old password"))
		}
	}
	return errors.Join(errs...)
}

// TestLiveDBRotateRedis rotates the Redis root password of the test
// environment through the full pipeline (pre-flight, database change, the
// rewrite of the URL places, their redeploys, verify), checks it, then rotates
// back to the value read at the start. That value lives only in this
// function's memory, never in a log, a file or an error. A cleanup puts it
// back if the test fails after the first run was frozen.
func TestLiveDBRotateRedis(t *testing.T) {
	liveDBSkip(t)
	rig := newLiveDBRig(t)
	d := detectLiveDBs(t, rig.st)["redis"]
	original := d.rootPassword(t, rig.st)
	if err := kinds.CheckPastedPassword(d.kind, original); err != nil {
		t.Fatalf("the current Redis password cannot be pasted back: %v", err)
	}
	if v := rig.st.Variable(domain.LocationID(d.owner.EnvID, d.owner.ServiceID, d.rootVar)); v == nil || !v.Literal {
		t.Fatal("the Redis root password variable is not a literal; refusing to rotate it")
	}
	// The proof the rig can log in at all, before anything changes.
	pre, cancelPre := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := d.login(pre, rig.st, original, false); err != nil {
		cancelPre()
		t.Fatalf("redis login with the current password failed before any change: %v", err)
	}
	cancelPre()

	// State for the cleanup, set from run states only.
	var forwardID, backID string
	forwardDone, backDone := false, false
	t.Cleanup(func() {
		if forwardID == "" || backDone {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		var errs []error
		if !forwardDone {
			// Finish the forward run if it can finish: its pre-flight is a
			// no-op once the database holds the new password (login tries the
			// current password, then the new one).
			if err := rig.run(ctx, forwardID); err != nil {
				errs = append(errs, fmt.Errorf("resuming the forward run: %w", err))
			}
		}
		if err := readLiveDBStore(rig.api, rig.st); err != nil {
			errs = append(errs, fmt.Errorf("reading the environment: %w", err))
		}
		// Rotate back: resume the back run if one was frozen (a new run would
		// conflict with it), else prepare one.
		id := backID
		if id == "" {
			var err error
			if id, _, err = rig.prepareRotation(d, original); err != nil {
				errs = append(errs, fmt.Errorf("preparing the rotation back: %w", err))
			}
		}
		if id != "" {
			if err := rig.run(ctx, id); err != nil {
				errs = append(errs, fmt.Errorf("rotating back: %w", err))
			} else if err := readLiveDBStore(rig.api, rig.st); err != nil {
				errs = append(errs, fmt.Errorf("reading the environment: %w", err))
			} else if row, ok := rotation.RowOf(rig.st, rotation.DBGroupKey(d.owner.Key(), d.rootUser), liveOpts()); !ok {
				errs = append(errs, errors.New("verifying the original: the password row is gone"))
			} else if err := rig.verify(ctx, d, row, original, ""); err != nil {
				errs = append(errs, fmt.Errorf("verifying the original: %w", err))
			}
		}
		if len(errs) == 0 {
			t.Log("cleanup: the Redis root password is back to the original")
			return
		}
		state := "the database most likely holds the ORIGINAL password and the variables are as at the start"
		switch {
		case forwardDone && backID == "":
			state = "the database most likely holds the TEST password; REDIS_PASSWORD and the rewritten URLs hold the test password too"
		case !forwardDone:
			state = "the database holds the original or the test password (the forward run did not finish); REDIS_PASSWORD may hold either, and some URL places may differ from it"
		case backID != "":
			state = "the rotation back started: the database holds the original or the test password; REDIS_PASSWORD and the URL places may hold either"
		}
		t.Errorf("CLEANUP FAILED: %s. Fix by hand in the live environment, setting the Redis password, REDIS_PASSWORD and the URL places to the original value (not logged: the password it held before the test). Errors: %v", state, errors.Join(errs...))
	})

	next := randomValue(t)
	id, row := rig.rotateTo(t, d, next)
	forwardID = id
	fctx, cancelF := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancelF()
	if err := rig.run(fctx, id); err != nil {
		t.Fatalf("rotate redis: %v", err)
	}
	forwardDone = true
	if err := readLiveDBStore(rig.api, rig.st); err != nil {
		t.Fatalf("reading the environment: %v", err)
	}
	if err := rig.verify(fctx, d, row, next, original); err != nil {
		t.Fatalf("after rotating redis: %v", err)
	}
	t.Log("redis: rotated, places rewritten, redeployed and verified")

	bctx, cancelB := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancelB()
	id, row = rig.rotateTo(t, d, original)
	backID = id
	if err := rig.run(bctx, id); err != nil {
		t.Fatalf("rotate redis back: %v", err)
	}
	backDone = true
	if err := readLiveDBStore(rig.api, rig.st); err != nil {
		t.Fatalf("reading the environment: %v", err)
	}
	if err := rig.verify(bctx, d, row, original, next); err != nil {
		t.Fatalf("after rotating redis back: %v", err)
	}
	t.Log("redis: rotated back to the original and verified")
}

package apply_test

// Database password scenarios through the real pipeline:
// Runs.Prepare (Check, Vet, Freeze), Runs.Execute, the executor, the journal
// and resume, over the fake Railway and a proc.Fake that plays ssh,
// `railway ssh config` and the database clients (dbSim).

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
)

// Password fixtures, long enough for the scrubber to know any 8-character
// piece of them.
const (
	scMongoOld = "old-mongo-root-pw-1a2b3c4d"
	scMongoNew = "NEW-mongo-root-pw-9z8y7x6w"
	scPgOld    = "old-pg-root-pw-5e6f7g8h"
	scPgNew    = "NEW-pg-root-pw-1q2w3e4r"
	scKeyOld   = "old-keyfile-value-0123456789abcdef"
	scKeyNew   = "NEWkeyfileValue+abcdef0123456789="
	scWrong    = "WRONG-hand-pw-5t6y7u8i9o"
	scMongoNw2 = "NEW2-mongo-root-pw-3c4v5b6n"
)

var scSecrets = []string{scMongoOld, scMongoNew, scPgOld, scPgNew, scKeyOld, scKeyNew, scWrong, scMongoNw2}

const (
	scProxy  = "shuttle.proxy.rlwy.net:41234"
	scMongoH = "mongo.railway.internal:27017"
)

var scEnvs = []string{"e1", "e2"}

var (
	scMongoRoot = rotation.DBGroupKey("e1/smongo", "root")
	scMongoKey  = rotation.DBGroupKey("e1/smongo", rotation.KeyFileUser)
	scPgRoot    = rotation.DBGroupKey("e1/spg", "postgres")
)

func scCatalog(single bool) railway.Catalog {
	prod := []string{"smongo", "smongo2", "spg", "sapi", "sweb", "sjob"}
	if single {
		prod = []string{"smongo", "sapi"}
	}
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{
			{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: prod},
			{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"sapi"}},
		},
		Services: []railway.Service{
			{ID: "smongo", Name: "mongo"}, {ID: "smongo2", Name: "mongo-2"}, {ID: "spg", Name: "pg"},
			{ID: "sapi", Name: "api"}, {ID: "sweb", Name: "web"}, {ID: "sjob", Name: "job"},
		},
	}}}
}

func scRec(env, svc, name, unrendered, rendered string) railway.VarRecord {
	if rendered == "" {
		rendered = unrendered
	}
	return railway.VarRecord{EnvID: env, ServiceID: svc, Name: name, Unrendered: unrendered, Rendered: rendered}
}

func rootURL(pw, host, path string) string { return "mongodb://root:" + pw + "@" + host + path }

// scVars: in production a Mongo replica set (mongo, mongo-2), a Postgres,
// an api with a literal root URL, a web following mongo's MONGO_URL and a
// job referencing both root passwords; in staging the api reaching the
// production Mongo through its TCP proxy (the cross-environment case).
func scVars() []railway.VarRecord {
	url := rootURL(scMongoOld, scMongoH, "")
	return []railway.VarRecord{
		scRec("e1", "smongo", "MONGO_INITDB_ROOT_USERNAME", "root", ""),
		scRec("e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", scMongoOld, ""),
		scRec("e1", "smongo", "MONGO_URL", "mongodb://${{MONGO_INITDB_ROOT_USERNAME}}:${{MONGO_INITDB_ROOT_PASSWORD}}@"+scMongoH, url),
		scRec("e1", "smongo", "REPLICA_SET_NAME", "rs0", ""),
		scRec("e1", "smongo", "KEYFILE", scKeyOld, ""),
		scRec("e1", "smongo2", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}", scMongoOld),
		scRec("e1", "smongo2", "REPLICA_SET_NAME", "rs0", ""),
		scRec("e1", "smongo2", "KEYFILE", "${{mongo.KEYFILE}}", scKeyOld),
		scRec("e1", "sapi", "MONGO_SOURCE", rootURL(scMongoOld, scMongoH, "/db?authSource=admin"), ""),
		scRec("e1", "sweb", "MONGO_URL", "${{mongo.MONGO_URL}}", url),
		scRec("e1", "sjob", "POSTGRES_PASSWORD", "${{pg.POSTGRES_PASSWORD}}", scPgOld),
		scRec("e1", "sjob", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}", scMongoOld),
		scRec("e1", "spg", "POSTGRES_USER", "postgres", ""),
		scRec("e1", "spg", "POSTGRES_PASSWORD", scPgOld, ""),
		scRec("e2", "sapi", "MONGO_SOURCE", rootURL(scMongoOld, scProxy, "/db?authSource=admin"), ""),
	}
}

// scSingleVars: a single-node Mongo set and an api on it.
func scSingleVars() []railway.VarRecord {
	return []railway.VarRecord{
		scRec("e1", "smongo", "MONGO_INITDB_ROOT_USERNAME", "root", ""),
		scRec("e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", scMongoOld, ""),
		scRec("e1", "smongo", "REPLICA_SET_NAME", "rs0", ""),
		scRec("e1", "smongo", "KEYFILE", scKeyOld, ""),
		scRec("e1", "sapi", "MONGO_SOURCE", rootURL(scMongoOld, scMongoH, "/db?authSource=admin"), ""),
	}
}

func scSystem() []railway.SystemRecord {
	return []railway.SystemRecord{
		{EnvID: "e1", ServiceID: "smongo", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "mongo.railway.internal"},
		{EnvID: "e1", ServiceID: "smongo", Name: "RAILWAY_TCP_PROXY_DOMAIN", Value: "shuttle.proxy.rlwy.net"},
		{EnvID: "e1", ServiceID: "smongo", Name: "RAILWAY_TCP_PROXY_PORT", Value: "41234"},
		{EnvID: "e1", ServiceID: "smongo2", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "mongo-2.railway.internal"},
		{EnvID: "e1", ServiceID: "spg", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "pg.railway.internal"},
	}
}

// dbSim is the database behind the fake clients: the password it takes, its
// writable primary and how a verify answers. It records what changed it.
type dbSim struct {
	mu      sync.Mutex
	pw      string // the password the database takes now
	next    string // what a psql ALTER USER sets (the verifier hides it)
	primary string // the service of the writable primary
	verify  string // the verify script's answer: PRIMARY or REPAIRED
	// incomplete changes the password that many times with the client's
	// output cut (exec.ErrWaitDelay): the outcome is unknown to the step.
	incomplete int
	// alterFails makes psql refuse the ALTER USER, its stderr quoting the
	// passwords and the statement.
	alterFails bool
	sockets    map[string]string // tunnel socket → service
	log        []string          // "change <service>" or "not primary <service>", in order
	alters     []string          // psql statements that changed the password
}

func (d *dbSim) takes(pw string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return pw == d.pw
}

// service is the service a client call reaches, through its tunnel socket.
func (d *dbSim) service(c proc.Cmd) string {
	where := strings.Join(c.Args, " ")
	for _, e := range c.Env {
		if v, ok := strings.CutPrefix(e, "RM_MONGO_URI="); ok {
			where, _ = url.PathUnescape(v)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for sock, svc := range d.sockets {
		if strings.Contains(where, filepath.Dir(sock)) {
			return svc
		}
	}
	return ""
}

func envOf(c proc.Cmd, name string) string {
	for _, e := range c.Env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v
		}
	}
	return ""
}

func script(c proc.Cmd) string { return c.Args[len(c.Args)-1] }

// refused is a client refusing a login, its stderr quoting the password
// as a careless client might.
func refused(pw string) proc.FakeReply {
	msg := "MongoServerError: Authentication failed for " + pw
	return proc.FakeReply{Stderr: msg + "\n", Err: &proc.ExitError{Code: 1, LastStderr: msg}}
}

func reply(out string) proc.FakeReply { return proc.FakeReply{Stdout: out + "\n"} }

// scRunner is a proc.Fake with ssh and every client installed: `railway
// ssh config` answers per service, ssh listens on its forward's socket,
// mongosh and psql play d. The first matching rule answers: the rules
// before a state-changing one only read d, and a state-changing matcher
// changes d exactly when it matches.
func scRunner(t *testing.T, d *dbSim) *proc.Fake {
	t.Helper()
	r := &proc.Fake{}
	for _, n := range []string{"ssh", "mongosh", "psql", "redis-cli"} {
		r.SetPath(n, "/usr/bin/"+n)
	}
	for _, svc := range []string{"smongo", "smongo2", "spg"} {
		r.On("railway", func(c proc.Cmd) bool { return slices.Contains(c.Args, svc) },
			proc.FakeReply{Stdout: "Host railway\n  HostName ssh.railway.test\n  User u-" + svc + "\n"})
	}
	r.On("ssh", func(c proc.Cmd) bool {
		i := slices.Index(c.Args, "-L")
		sock, _, _ := strings.Cut(c.Args[i+1], ":")
		user, _, _ := strings.Cut(c.Args[len(c.Args)-1], "@")
		d.mu.Lock()
		d.sockets[sock] = strings.TrimPrefix(user, "u-")
		d.mu.Unlock()
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Errorf("listen %s: %v", sock, err)
			return true
		}
		t.Cleanup(func() { _ = l.Close() })
		return true
	}, proc.FakeReply{})

	// mongosh: a refused login, per password.
	for _, pw := range scSecrets {
		r.On("mongosh", func(c proc.Cmd) bool {
			return envOf(c, "RM_MONGO_PASSWORD") == pw && !d.takes(pw)
		}, refused(pw))
	}
	r.On("mongosh", func(c proc.Cmd) bool {
		return strings.Contains(script(c), "updateUser") && d.service(c) != d.primary
	}, reply("NOT_PRIMARY"))
	r.On("mongosh", func(c proc.Cmd) bool {
		return strings.Contains(script(c), "updateUser") && envOf(c, "RM_DRY_RUN") == "1"
	}, reply("OK"))
	r.On("mongosh", func(c proc.Cmd) bool {
		if !strings.Contains(script(c), "updateUser") {
			return false
		}
		svc := d.service(c)
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.incomplete == 0 {
			return false
		}
		// The database takes the password, the client's output is cut.
		d.incomplete--
		d.pw = envOf(c, "RM_NEW_PASSWORD")
		d.log = append(d.log, "change "+svc)
		return true
	}, proc.FakeReply{Stdout: "partial", Err: exec.ErrWaitDelay})
	r.On("mongosh", func(c proc.Cmd) bool {
		if !strings.Contains(script(c), "updateUser") {
			return false
		}
		svc := d.service(c)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.pw = envOf(c, "RM_NEW_PASSWORD")
		d.log = append(d.log, "change "+svc)
		return true
	}, reply("OK"))
	r.On("mongosh", func(c proc.Cmd) bool { return strings.Contains(script(c), "connectionStatus") }, reply("OK"))
	for _, word := range []string{"PRIMARY", "REPAIRED"} {
		r.On("mongosh", func(c proc.Cmd) bool {
			d.mu.Lock()
			defer d.mu.Unlock()
			return strings.Contains(script(c), "replSetReconfig") && d.verify == word
		}, reply(word))
	}

	// psql: the version, a refused login per password, then the statements.
	r.On("psql", func(c proc.Cmd) bool { return slices.Contains(c.Args, "--version") }, reply("psql (PostgreSQL) 16.2"))
	for _, pw := range scSecrets {
		r.On("psql", func(c proc.Cmd) bool { return envOf(c, "PGPASSWORD") == pw && !d.takes(pw) }, proc.FakeReply{
			// psql quotes the failing statement; stderr is never trusted.
			Stderr: "psql: error: FATAL: password authentication failed for user \"postgres\" " + pw + "\nLINE 1: ALTER USER x WITH PASSWORD '" + scPgNew + "'\n",
			Err:    &proc.ExitError{Code: 2, LastStderr: "FATAL " + pw},
		})
	}
	r.On("psql", func(c proc.Cmd) bool { return strings.Contains(c.Stdin, "pg_roles") }, reply("OK"))
	r.On("psql", func(c proc.Cmd) bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.alterFails && strings.HasPrefix(c.Stdin, "ALTER USER")
	}, proc.FakeReply{
		Stderr: "psql:<stdin>:1: ERROR: permission denied to alter role (current " + scPgOld + ", new " + scPgNew + ")\nLINE 1: " + "ALTER USER \"postgres\" WITH PASSWORD '" + scPgNew + "';\n        ^\n",
		Err:    &proc.ExitError{Code: 3, LastStderr: "ERROR " + scPgNew},
	})
	r.On("psql", func(c proc.Cmd) bool {
		if !strings.HasPrefix(c.Stdin, "ALTER USER") {
			return false
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.pw = d.next
		d.alters = append(d.alters, c.Stdin)
		d.log = append(d.log, "change spg")
		return true
	}, reply("ALTER ROLE"))
	r.On("psql", func(c proc.Cmd) bool { return strings.HasPrefix(c.Stdin, "SELECT 1") }, reply("1"))
	return r
}

// scenario is one database password run's world.
type scenario struct {
	st   *store.Store
	fake *railwaytest.Fake
	r    *proc.Fake
	db   *dbSim
	dir  string
	// known is the known hosts file, trusting the fake ssh host.
	known string

	mu     sync.Mutex
	events []apply.Event
}

func newScenario(t *testing.T, single bool, d *dbSim) *scenario {
	t.Helper()
	recs := scVars()
	if single {
		recs = scSingleVars()
	}
	st := store.New()
	st.ReplaceCatalog(scCatalog(single))
	st.ReplaceVariables("p1", recs, scSystem()...)
	f := railwaytest.New(t)
	f.SetCatalog(scCatalog(single))
	for _, r := range recs {
		f.SetVar(r.EnvID, r.ServiceID, r.Name, r.Unrendered, false)
	}
	for _, e := range scCatalog(single).Projects[0].Environments {
		for _, svc := range e.ServiceIDs {
			f.AddDeployment(e.ID, svc, "old-"+e.ID+"-"+svc, "SUCCESS", time.Now().Add(-time.Hour))
		}
	}
	d.sockets = map[string]string{}
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, []byte("ssh.railway.test ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIC/3HMGJU6ZTAKmx4j6OvVVZMxv/6Wx2e+nrJWCfN5xu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &scenario{st: st, fake: f, r: scRunner(t, d), db: d, dir: t.TempDir(), known: known}
}

// runs is the app's wiring (internal/app/build.go) on the fakes: a fresh
// value per call, as after a restart.
func (sc *scenario) runs() *apply.Runs {
	reg := kinds.NewRegistry()
	api := &railway.Client{
		T:            &railway.Transport{Endpoint: sc.fake.URL, Tokens: staticTokens{}},
		WriteAllowed: func(string) bool { return true },
	}
	compile := func(s *store.Store, cs changeset.ChangeSet) (apply.Graph, error) {
		return apply.Compile(cs, s, reg,
			func(envID string, changes []changeset.Change) apply.Step {
				return kinds.NewPatchStep(envID, changes, reg, changes[0].Place.ProjectID)
			}, kinds.NewRedeployStep)
	}
	return &apply.Runs{
		Dir: sc.dir, Reg: reg, Exec: &apply.Executor{},
		NewEnv: func(runID string, j *apply.Journal) *apply.Env {
			return &apply.Env{
				API: api, Store: sc.st, Journal: j, RunID: runID, Proc: sc.r, KnownHosts: sc.known,
				Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
				Now:   time.Now,
			}
		},
		NewRedeploy: kinds.NewRedeployStep,
		Snapshot:    func() *store.Store { return sc.st },
		Compile:     compile,
		Vet: func(s *store.Store, sel, live changeset.ChangeSet, actions []apply.FrozenAction) error {
			return kinds.CheckPasswordGroups(s, scEnvs, sel, live, actions)
		},
	}
}

// decide is the password decision of group, as the Rotate screen makes it.
func (sc *scenario) decide(t *testing.T, group, source, value string) *changeset.ChangeSet {
	t.Helper()
	row, ok := rotation.RowOf(sc.st, group, rotation.Options{EnvIDs: scEnvs, AllEnvIDs: scEnvs})
	if !ok {
		t.Fatalf("no row %s", group)
	}
	cs, err := kinds.NewPasswordChanges(sc.st, nil, row, source, value, time.Now())
	if err != nil {
		t.Fatalf("NewPasswordChanges: %v", err)
	}
	live := &changeset.ChangeSet{}
	for _, c := range cs {
		if err := live.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	return live
}

func (sc *scenario) prepare(t *testing.T, live *changeset.ChangeSet) string {
	t.Helper()
	id, conflicts, err := sc.runs().Prepare(live, nil)
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("Prepare: %v, conflicts %v", err, conflicts)
	}
	return id
}

// execute runs (or resumes) the run on a fresh Runs; onEvent, if any, sees
// every event after it is logged.
func (sc *scenario) execute(ctx context.Context, runID string, onEvent func(apply.Event)) (apply.Report, error) {
	return sc.runs().Execute(ctx, runID, func(e apply.Event) {
		sc.mu.Lock()
		sc.events = append(sc.events, e)
		sc.mu.Unlock()
		if onEvent != nil {
			onEvent(e)
		}
	})
}

func (sc *scenario) journal(t *testing.T, runID string) *apply.Journal {
	t.Helper()
	return journalOf(t, sc.dir, runID)
}

// at is the index of the first event of key with status, -1 when none.
func (sc *scenario) at(key, status string) int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return slices.IndexFunc(sc.events, func(e apply.Event) bool { return e.StepKey == key && e.Status == status })
}

// before fails unless a's event comes before b's.
func (sc *scenario) before(t *testing.T, aKey, aStatus, bKey, bStatus string) {
	t.Helper()
	a, b := sc.at(aKey, aStatus), sc.at(bKey, bStatus)
	if a < 0 || b < 0 || a > b {
		t.Errorf("%s %s (event %d) must come before %s %s (event %d)", aKey, aStatus, a, bKey, bStatus, b)
	}
}

// redeployed are the places Railway was asked to redeploy, in order.
func (sc *scenario) redeployed() []string {
	var out []string
	for _, m := range sc.fake.Mutations() {
		if m.Op == "Redeploy" {
			out = append(out, m.Vars["environmentId"].(string)+"/"+m.Vars["serviceId"].(string))
		}
	}
	return out
}

// patchesBeforeRedeploys fails when a redeploy of an environment comes
// before every patch committed to it.
func (sc *scenario) patchesBeforeRedeploys(t *testing.T) {
	t.Helper()
	lastPatch := map[string]int{}
	for i, m := range sc.fake.Mutations() {
		if m.Op == "CommitPatch" {
			lastPatch[m.Vars["environmentId"].(string)] = i
		}
	}
	for i, m := range sc.fake.Mutations() {
		if env, _ := m.Vars["environmentId"].(string); m.Op == "Redeploy" {
			if p, ok := lastPatch[env]; ok && p > i {
				t.Errorf("redeploy of %s/%v before its patch", env, m.Vars["serviceId"])
			}
		}
	}
}

// newestIs fails unless the newest deployment of a service, read now, has
// status want.
func (sc *scenario) newestIs(t *testing.T, env, svc, want string) {
	t.Helper()
	api := &railway.Client{T: &railway.Transport{Endpoint: sc.fake.URL, Tokens: staticTokens{}}}
	ds, err := api.Deployments(context.Background(), env, svc)
	if err != nil || len(ds) == 0 || ds[0].Status != want {
		t.Errorf("%s/%s newest deployment: %v, %v; want %s", env, svc, ds, err, want)
	}
}

func (sc *scenario) varIs(t *testing.T, env, svc, name, want string) {
	t.Helper()
	got, _ := sc.fake.Var(env, svc, name)
	if got != want {
		t.Errorf("%s/%s %s is not the expected value", env, svc, name)
	}
}

// scan fails when a secret is in an argv or stdin-free field of any client
// call, in the run dir's journal or other non-value files, in an event or
// in errs.
func (sc *scenario) scan(t *testing.T, errs ...error) {
	t.Helper()
	check := func(what, s string) {
		t.Helper()
		for _, sec := range scSecrets {
			if strings.Contains(s, sec) {
				t.Errorf("%s holds a secret", what)
				return
			}
		}
	}
	for _, c := range sc.r.Calls() {
		check("argv of "+c.Name, strings.Join(c.Args, " "))
		if c.Name == "psql" {
			check("psql stdin", c.Stdin)
		}
	}
	runs, _ := filepath.Glob(filepath.Join(sc.dir, "runs", "*"))
	for _, dir := range runs {
		for _, name := range []string{"journal.json", "actions.json", "reopen.json"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err == nil {
				check(name, string(data))
			}
		}
	}
	sc.mu.Lock()
	data, _ := json.Marshal(sc.events)
	sc.mu.Unlock()
	check("events", string(data))
	for _, err := range errs {
		if err != nil {
			check("error", err.Error())
		}
	}
}

func mustFinish(t *testing.T, rep apply.Report, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rep.State != apply.RunFinished || len(rep.Kept) > 0 {
		t.Fatalf("report: state %s, kept %v", rep.State, rep.Kept)
	}
}

const (
	dbpwRoot    = "dbpw:e1/smongo:root"
	preRoot     = "preflight:e1/smongo:root"
	verifyRoot  = "dbverify:e1/smongo:root"
	mongoProdAp = "redeploy:e1/sapi"
	stagingApi  = "redeploy:e2/sapi"
)

var mongoRedeploys = []string{"e1/sapi", "e1/sjob", "e1/sweb", "e2/sapi"}

func sortedCopy(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }

// A Mongo root through the primary.
func TestScenarioMongoRoot(t *testing.T) {
	d := &dbSim{pw: scMongoOld, primary: "smongo2", verify: "PRIMARY"}
	sc := newScenario(t, false, d)
	sc.fake.DeployStatuses("e1", "sapi", "BUILDING", "DEPLOYING", "SUCCESS")
	sc.fake.DeployStatuses("e2", "sapi", "BUILDING", "SUCCESS")
	id := sc.prepare(t, sc.decide(t, scMongoRoot, kinds.SourceGenerate, scMongoNew))

	rep, err := sc.execute(context.Background(), id, nil)
	mustFinish(t, rep, err)

	// 1. the change through the primary, a non-primary member first.
	if want := []string{"change smongo2"}; !slices.Equal(d.log, want) {
		t.Errorf("database changes = %v, want %v", d.log, want)
	}
	change := func(svc string) func(proc.Cmd) bool {
		return func(c proc.Cmd) bool {
			return c.Name == "mongosh" && strings.Contains(script(c), "updateUser") && envOf(c, "RM_DRY_RUN") == "" && d.service(c) == svc
		}
	}
	calls := sc.r.Calls()
	if first, primary := slices.IndexFunc(calls, change("smongo")), slices.IndexFunc(calls, change("smongo2")); first < 0 || primary < 0 || first > primary {
		t.Errorf("change calls: non-primary at %d, primary at %d; want the non-primary first", first, primary)
	}
	// 2–3. the own variable and every root URL, staging's through the proxy.
	sc.varIs(t, "e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", scMongoNew)
	sc.varIs(t, "e1", "sapi", "MONGO_SOURCE", rootURL(scMongoNew, scMongoH, "/db?authSource=admin"))
	sc.varIs(t, "e2", "sapi", "MONGO_SOURCE", rootURL(scMongoNew, scProxy, "/db?authSource=admin"))
	sc.varIs(t, "e1", "sweb", "MONGO_URL", "${{mongo.MONGO_URL}}")
	if n := countOps(sc.fake, "CommitPatch"); n != 2 {
		t.Errorf("patches = %d, want one per environment", n)
	}
	// 4–5. apps redeploy after their patch, Mongo does not.
	if got := sortedCopy(sc.redeployed()); !slices.Equal(got, mongoRedeploys) {
		t.Errorf("redeployed %v, want %v (never a Mongo place)", got, mongoRedeploys)
	}
	sc.patchesBeforeRedeploys(t)
	sc.before(t, dbpwRoot, apply.StatusDone, "patch:e1", apply.StatusStarted)
	sc.before(t, dbpwRoot, apply.StatusDone, "patch:e2", apply.StatusStarted)
	for _, p := range mongoRedeploys {
		// The verify waits for every redeploy's SUCCESS.
		sc.before(t, "redeploy:"+p, apply.StatusDone, verifyRoot, apply.StatusStarted)
	}
	// The redeploys waited through the scripted statuses: the newest
	// deployment has reached SUCCESS (a read advances it one status).
	sc.newestIs(t, "e1", "sapi", "SUCCESS")
	sc.newestIs(t, "e2", "sapi", "SUCCESS")
	if r := sc.journal(t, id).Step(verifyRoot); r.Status != apply.StatusDone {
		t.Errorf("verify = %+v", r)
	}
	sc.scan(t)
}

// A KEYFILE on a single-node set, which comes
// back lost from its own config and is repaired.
func TestScenarioKeyFileSingleNodeRepaired(t *testing.T) {
	d := &dbSim{pw: scMongoOld, primary: "smongo", verify: "REPAIRED"}
	sc := newScenario(t, true, d)
	sc.fake.DeployStatuses("e1", "smongo", "BUILDING", "DEPLOYING", "SUCCESS")
	live := sc.decide(t, scMongoKey, kinds.SourceGenerate, scKeyNew)
	kf := live.Changes[0]
	id := sc.prepare(t, live)

	rep, err := sc.execute(context.Background(), id, nil)
	mustFinish(t, rep, err)

	sc.varIs(t, "e1", "smongo", "KEYFILE", scKeyNew)
	if got := sc.redeployed(); !slices.Equal(got, []string{"e1/smongo"}) {
		t.Errorf("redeployed %v, want the member", got)
	}
	sc.patchesBeforeRedeploys(t)
	sc.before(t, "redeploy:e1/smongo", apply.StatusDone, "dbverify:e1/smongo:@keyfile", apply.StatusStarted)
	sc.newestIs(t, "e1", "smongo", "SUCCESS")
	if len(d.log) != 0 {
		t.Errorf("a KEYFILE rotation changed a password: %v", d.log)
	}
	o := sc.journal(t, id).Outcome(kf.ID)
	if o.Status != apply.StatusDone || !strings.HasPrefix(o.Reason, "repaired") {
		t.Errorf("keyfile outcome = %+v, want done, reported repaired", o)
	}
	sc.scan(t)
}

// The run is killed right
// after the database step; the resume, on a fresh Runs, re-runs it as a
// no-op (current, then new), writes, redeploys and verifies.
func TestScenarioKilledAfterDatabaseStep(t *testing.T) {
	d := &dbSim{pw: scMongoOld, primary: "smongo2", verify: "PRIMARY"}
	sc := newScenario(t, false, d)
	id := sc.prepare(t, sc.decide(t, scMongoRoot, kinds.SourceGenerate, scMongoNew))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := sc.execute(ctx, id, func(e apply.Event) {
		if e.StepKey == dbpwRoot && e.Status == apply.StatusDone {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("killed run: err = %v", err)
	}
	j := sc.journal(t, id)
	if r := j.Step(dbpwRoot); r.Status != apply.StatusDone || !r.Acted {
		t.Fatalf("after the kill dbpw = %+v, want done and Acted", r)
	}
	if j.State != apply.RunRunning {
		t.Errorf("state after the kill = %s", j.State)
	}
	if n := len(sc.fake.Mutations()); n != 0 {
		t.Fatalf("the killed run wrote %d mutations", n)
	}
	if d.pw != scMongoNew {
		t.Fatal("the database did not take the new password")
	}

	// Resume: dbpw is reopened, its Acted stays visible while it runs.
	sc.mu.Lock()
	sc.events = nil
	sc.mu.Unlock()
	var acted []bool
	calls := len(sc.r.Calls())
	rep, err := sc.execute(context.Background(), id, func(e apply.Event) {
		if e.StepKey == dbpwRoot {
			acted = append(acted, journalOf(t, sc.dir, id).Step(dbpwRoot).Acted)
		}
	})
	mustFinish(t, rep, err)
	if len(acted) != 2 || slices.Contains(acted, false) {
		t.Errorf("dbpw on resume: %d events, Acted %v; want started and done, Acted throughout", len(acted), acted)
	}
	// The no-op change: the current password (from Railway, still the old
	// one) is refused, the new one logs in and sets itself again.
	var logins []string
	for _, c := range sc.r.Calls()[calls:] {
		if c.Name == "mongosh" && strings.Contains(script(c), "updateUser") && d.service(c) == "smongo2" {
			logins = append(logins, map[string]string{scMongoOld: "current", scMongoNew: "new"}[envOf(c, "RM_MONGO_PASSWORD")])
		}
	}
	if !slices.Equal(logins, []string{"current", "new"}) {
		t.Errorf("resume logins on the primary = %v, want current then new", logins)
	}
	if want := []string{"change smongo2", "change smongo2"}; !slices.Equal(d.log, want) {
		t.Errorf("database changes = %v, want %v", d.log, want)
	}
	if sc.at(preRoot, apply.StatusStarted) >= 0 {
		t.Error("the pre-flight ran again on resume")
	}
	sc.before(t, dbpwRoot, apply.StatusDone, "patch:e1", apply.StatusStarted)
	sc.varIs(t, "e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", scMongoNew)
	sc.varIs(t, "e2", "sapi", "MONGO_SOURCE", rootURL(scMongoNew, scProxy, "/db?authSource=admin"))
	if got := sortedCopy(sc.redeployed()); !slices.Equal(got, mongoRedeploys) {
		t.Errorf("redeployed %v, want %v", got, mongoRedeploys)
	}
	sc.patchesBeforeRedeploys(t)
	if r := sc.journal(t, id).Step(verifyRoot); r.Status != apply.StatusDone {
		t.Errorf("verify = %+v", r)
	}
	sc.scan(t, err)
}

// The database takes the new password but the client's output is cut:
// dbpw fails with Acted set (MarkActed before the change), nothing is
// written; continue logs in with the current password, then the new one,
// and finishes the run.
func TestScenarioChangeOutcomeUnknown(t *testing.T) {
	d := &dbSim{pw: scMongoOld, primary: "smongo2", verify: "PRIMARY", incomplete: 1}
	sc := newScenario(t, false, d)
	id := sc.prepare(t, sc.decide(t, scMongoRoot, kinds.SourceGenerate, scMongoNew))

	rep, err := sc.execute(context.Background(), id, nil)
	if err == nil || rep.State != apply.RunFailed {
		t.Fatalf("state %s, err %v", rep.State, err)
	}
	j := sc.journal(t, id)
	if r := j.Step(dbpwRoot); r.Status != apply.StatusFailed || !r.Acted || !strings.Contains(r.Error, "may already have the new password") {
		t.Errorf("dbpw = %+v, want failed, Acted, naming the unknown outcome", r)
	}
	for _, k := range []string{"patch:e1", "patch:e2", verifyRoot} {
		if got := j.Step(k).Status; got != apply.StatusBlocked {
			t.Errorf("%s = %s, want blocked", k, got)
		}
	}
	if n := len(sc.fake.Mutations()); n != 0 {
		t.Errorf("wrote %d mutations after an unknown change", n)
	}
	sc.scan(t, err)

	rep, err = sc.execute(context.Background(), id, nil)
	mustFinish(t, rep, err)
	if r := sc.journal(t, id).Step(dbpwRoot); r.Status != apply.StatusDone || !r.Acted {
		t.Errorf("dbpw after continue = %+v", r)
	}
	if want := []string{"change smongo2", "change smongo2"}; !slices.Equal(d.log, want) {
		t.Errorf("database changes = %v, want %v", d.log, want)
	}
	sc.varIs(t, "e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", scMongoNew)
	if got := sortedCopy(sc.redeployed()); !slices.Equal(got, mongoRedeploys) {
		t.Errorf("redeployed %v, want %v", got, mongoRedeploys)
	}
	sc.scan(t, err)
}

// Staging's patch fails after the database step. Every place
// that got the new value redeploys, only staging's redeploy is blocked and
// dbpw stays Acted; a continue once staging is fixed reopens dbpw, writes
// staging and redeploys it.
func TestScenarioStagingPatchFails(t *testing.T) {
	d := &dbSim{pw: scMongoOld, primary: "smongo2", verify: "PRIMARY"}
	sc := newScenario(t, false, d)
	sc.fake.FailPatchesIn("e2", "staging is locked")
	id := sc.prepare(t, sc.decide(t, scMongoRoot, kinds.SourceGenerate, scMongoNew))

	rep, err := sc.execute(context.Background(), id, nil)
	if err == nil || rep.State != apply.RunFailed {
		t.Fatalf("run with staging failing: state %s, err %v", rep.State, err)
	}
	j := sc.journal(t, id)
	for key, want := range map[string]string{
		dbpwRoot: apply.StatusDone, "patch:e1": apply.StatusDone, "patch:e2": apply.StatusFailed,
		"redeploy:e1/sapi": apply.StatusDone, "redeploy:e1/sweb": apply.StatusDone, "redeploy:e1/sjob": apply.StatusDone,
		stagingApi: apply.StatusBlocked, verifyRoot: apply.StatusBlocked,
	} {
		if got := j.Step(key).Status; got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	if got := sortedCopy(sc.redeployed()); !slices.Equal(got, []string{"e1/sapi", "e1/sjob", "e1/sweb"}) {
		t.Errorf("redeployed %v, want production's apps only", got)
	}
	sc.varIs(t, "e2", "sapi", "MONGO_SOURCE", rootURL(scMongoOld, scProxy, "/db?authSource=admin"))
	sc.scan(t, err)

	// A second password for the same database user while the run is
	// stalled: refused as a conflict with it, nothing written.
	again := sc.decide(t, scMongoRoot, kinds.SourceGenerate, scMongoNw2)
	pw := again.Changes[0]
	if w := kinds.NewRegistry().Writes(pw); !slices.Equal(w, []string{"db:e1/smongo:root"}) {
		t.Errorf("db.password writes %v, want its database and user", w)
	}
	runsBefore, _ := os.ReadDir(filepath.Join(sc.dir, "runs"))
	second, conflicts, err := sc.runs().Prepare(again, []string{pw.ID})
	if err != nil || second != "" || !slices.ContainsFunc(conflicts, func(c changeset.Conflict) bool {
		return c.OtherRun == id && c.Candidate.ID == pw.ID
	}) {
		t.Errorf("second password while stalled: run %q, err %v, conflicts %+v; want a conflict with %s", second, err, conflicts, id)
	}
	if runsAfter, _ := os.ReadDir(filepath.Join(sc.dir, "runs")); len(runsAfter) != len(runsBefore) {
		t.Error("a refused second password wrote a run")
	}
	if got, _ := again.Get(pw.ID); got.RunID != "" {
		t.Error("a refused second password was locked")
	}

	// Staging fixed: continue.
	sc.fake.FailPatchesIn("e2", "")
	before := len(sc.fake.Mutations())
	sc.mu.Lock()
	sc.events = nil
	sc.mu.Unlock()
	rep, err = sc.execute(context.Background(), id, nil)
	mustFinish(t, rep, err)
	if sc.at(dbpwRoot, apply.StatusStarted) < 0 {
		t.Error("continue did not reopen dbpw")
	}
	sc.before(t, dbpwRoot, apply.StatusDone, "patch:e2", apply.StatusStarted)
	if want := []string{"change smongo2", "change smongo2"}; !slices.Equal(d.log, want) {
		t.Errorf("database changes = %v, want %v", d.log, want)
	}
	sc.varIs(t, "e2", "sapi", "MONGO_SOURCE", rootURL(scMongoNew, scProxy, "/db?authSource=admin"))
	var after []string
	for _, m := range sc.fake.Mutations()[before:] {
		after = append(after, m.Op+" "+m.Vars["environmentId"].(string))
	}
	if !slices.Equal(after, []string{"CommitPatch e2", "Redeploy e2"}) {
		t.Errorf("continue sent %v, want staging's patch and redeploy", after)
	}
	sc.before(t, stagingApi, apply.StatusDone, verifyRoot, apply.StatusStarted)
	sc.scan(t, err)
}

// A Postgres root: an app referencing POSTGRES_PASSWORD redeploys; the
// change carries the SCRAM verifier, never the password; every login that
// proves a password goes to the private address.
func TestScenarioPostgresRoot(t *testing.T) {
	d := &dbSim{pw: scPgOld, next: scPgNew}
	sc := newScenario(t, false, d)
	id := sc.prepare(t, sc.decide(t, scPgRoot, kinds.SourceGenerate, scPgNew))

	rep, err := sc.execute(context.Background(), id, nil)
	mustFinish(t, rep, err)

	if len(d.alters) != 1 || !strings.Contains(d.alters[0], "PASSWORD 'SCRAM-SHA-256$4096:") {
		t.Errorf("password changes = %d, want one ALTER USER with a SCRAM verifier", len(d.alters))
	}
	sc.varIs(t, "e1", "spg", "POSTGRES_PASSWORD", scPgNew)
	sc.varIs(t, "e1", "sjob", "POSTGRES_PASSWORD", "${{pg.POSTGRES_PASSWORD}}")
	if got := sc.redeployed(); !slices.Equal(got, []string{"e1/sjob"}) {
		t.Errorf("redeployed %v, want the app only", got)
	}
	for _, c := range sc.r.Calls() {
		if c.Name == "ssh" {
			if fwd := c.Args[slices.Index(c.Args, "-L")+1]; !strings.HasSuffix(fwd, ":pg.railway.internal:5432") {
				t.Errorf("forward %s does not reach the private address", fwd)
			}
		}
	}
	sc.before(t, "redeploy:e1/sjob", apply.StatusDone, "dbverify:e1/spg:postgres", apply.StatusStarted)
	sc.scan(t)
}

// A password changed by hand: no dbpw; the pre-flight proves the new
// password before anything is written, and a wrong one is refused with
// nothing written and no Acted. (Its MarkActed after the login is the
// step's last act, so no failure can follow it in a run; kinds'
// TestPreflightHand checks the mark without the executor's backstop.)
func TestScenarioHandSource(t *testing.T) {
	t.Run("wrong", func(t *testing.T) {
		d := &dbSim{pw: scMongoNew, primary: "smongo2", verify: "PRIMARY"}
		sc := newScenario(t, false, d)
		id := sc.prepare(t, sc.decide(t, scMongoRoot, kinds.SourceHand, scWrong))
		rep, err := sc.execute(context.Background(), id, nil)
		if err == nil || rep.State != apply.RunFailed {
			t.Fatalf("state %s, err %v", rep.State, err)
		}
		j := sc.journal(t, id)
		if r := j.Step(preRoot); r.Status != apply.StatusFailed || r.Acted ||
			!strings.Contains(r.Error, "the database does not accept the new password") {
			t.Errorf("preflight = %+v", r)
		}
		if _, ok := j.Steps[dbpwRoot]; ok {
			t.Error("hand has a dbpw step")
		}
		if n := len(sc.fake.Mutations()); n != 0 {
			t.Errorf("a refused hand password wrote %d mutations", n)
		}
		sc.scan(t, err)
	})
	t.Run("right", func(t *testing.T) {
		d := &dbSim{pw: scMongoNew, primary: "smongo2", verify: "PRIMARY"}
		sc := newScenario(t, false, d)
		id := sc.prepare(t, sc.decide(t, scMongoRoot, kinds.SourceHand, scMongoNew))
		rep, err := sc.execute(context.Background(), id, nil)
		mustFinish(t, rep, err)
		if len(d.log) != 0 {
			t.Errorf("hand changed the database: %v", d.log)
		}
		sc.varIs(t, "e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", scMongoNew)
		if got := sortedCopy(sc.redeployed()); !slices.Equal(got, mongoRedeploys) {
			t.Errorf("redeployed %v, want %v", got, mongoRedeploys)
		}
		sc.before(t, preRoot, apply.StatusDone, "patch:e1", apply.StatusStarted)
		sc.scan(t)
	})
}

// The revert of a stalled root password whose row has
// rewritten URL places. The revert is a lone change: its places already
// hold the value it sets, so Vet takes it without a decision per place; it
// logs in with the abandoned run's password and sets the variable's value
// back, writing and redeploying nothing.
func TestScenarioRevertWithRewrittenURLs(t *testing.T) {
	d := &dbSim{pw: scMongoNew, primary: "smongo2", verify: "PRIMARY"}
	sc := newScenario(t, false, d)
	row, ok := rotation.RowOf(sc.st, scMongoRoot, rotation.Options{EnvIDs: scEnvs, AllEnvIDs: scEnvs})
	if !ok {
		t.Fatal("no mongo root row")
	}
	if !slices.ContainsFunc(row.URLs, func(u rotation.URLPlace) bool { return u.Mode == rotation.URLRewrite }) {
		t.Fatal("fixture: the row has no rewritten URL")
	}
	rv, err := kinds.DBPasswordRevert(sc.st, row, scMongoNew, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	live := &changeset.ChangeSet{}
	if err := live.Put(rv, nil); err != nil {
		t.Fatal(err)
	}
	id := sc.prepare(t, live)

	rep, err := sc.execute(context.Background(), id, nil)
	mustFinish(t, rep, err)
	if !d.takes(scMongoOld) {
		t.Error("the database was not set back to the variable's value")
	}
	if n := len(sc.fake.Mutations()); n != 0 {
		t.Errorf("a revert sent %d mutations", n)
	}
	if r := sc.journal(t, id).Step(verifyRoot); r.Status != apply.StatusDone {
		t.Errorf("verify = %+v", r)
	}
	sc.scan(t, err)
}

// decideWith is decide with pending decisions of the group's places.
func (sc *scenario) decideWith(t *testing.T, group, value string, pending ...changeset.Change) *changeset.ChangeSet {
	t.Helper()
	row, ok := rotation.RowOf(sc.st, group, rotation.Options{EnvIDs: scEnvs, AllEnvIDs: scEnvs})
	if !ok {
		t.Fatalf("no row %s", group)
	}
	cs, err := kinds.NewPasswordChanges(sc.st, pending, row, kinds.SourceGenerate, value, time.Now())
	if err != nil {
		t.Fatalf("NewPasswordChanges: %v", err)
	}
	live := &changeset.ChangeSet{}
	for _, c := range cs {
		if err := live.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	return live
}

// handSet is the user's own value for a place of group (p).
func (sc *scenario) handSet(t *testing.T, group, env, svc, name, value string) changeset.Change {
	t.Helper()
	c, err := kinds.SetVariable(sc.st, nil, sc.st.PlaceOf(env, svc), name, value, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c.Group = group
	return c
}

// I1: a URL changed in Railway after the database step. An automatic
// rewrite follows the current URL (re-based at the patch) and the run
// finishes; a place the user decided fails the patch, naming it, and keeps
// failing on continue (the row stays stalled until the user abandons).
func TestScenarioDriftAfterDatabaseStep(t *testing.T) {
	moved := rootURL(scMongoOld, scMongoH, "/db2?authSource=admin&replicaSet=rs0")
	t.Run("automatic rewrite re-based", func(t *testing.T) {
		d := &dbSim{pw: scMongoOld, primary: "smongo2", verify: "PRIMARY"}
		sc := newScenario(t, false, d)
		id := sc.prepare(t, sc.decide(t, scMongoRoot, kinds.SourceGenerate, scMongoNew))
		rep, err := sc.execute(context.Background(), id, func(e apply.Event) {
			if e.StepKey == dbpwRoot && e.Status == apply.StatusDone {
				sc.fake.SetVar("e1", "sapi", "MONGO_SOURCE", moved, false)
			}
		})
		mustFinish(t, rep, err)
		sc.varIs(t, "e1", "sapi", "MONGO_SOURCE", rootURL(scMongoNew, scMongoH, "/db2?authSource=admin&replicaSet=rs0"))
		sc.varIs(t, "e2", "sapi", "MONGO_SOURCE", rootURL(scMongoNew, scProxy, "/db?authSource=admin"))
		if got := sortedCopy(sc.redeployed()); !slices.Equal(got, mongoRedeploys) {
			t.Errorf("redeployed %v, want %v", got, mongoRedeploys)
		}
		sc.scan(t, err)
	})
	t.Run("decided place fails the step", func(t *testing.T) {
		d := &dbSim{pw: scMongoOld, primary: "smongo2", verify: "PRIMARY"}
		sc := newScenario(t, false, d)
		mine := sc.handSet(t, scMongoRoot, "e1", "sapi", "MONGO_SOURCE", rootURL(scMongoNew, scMongoH, "/mine"))
		id := sc.prepare(t, sc.decideWith(t, scMongoRoot, scMongoNew, mine))
		rep, err := sc.execute(context.Background(), id, func(e apply.Event) {
			if e.StepKey == dbpwRoot && e.Status == apply.StatusDone {
				sc.fake.SetVar("e1", "sapi", "MONGO_SOURCE", moved, false)
			}
		})
		if err == nil || rep.State != apply.RunFailed {
			t.Fatalf("state %s, err %v; want failed", rep.State, err)
		}
		j := sc.journal(t, id)
		const note = "production · api MONGO_SOURCE: changed in Railway since the decision · abandon to keep it, or set it in Railway to the new password"
		if r := j.Step("patch:e1"); r.Status != apply.StatusFailed || !strings.Contains(r.Error, note) {
			t.Errorf("patch:e1 = %+v, want failed naming the place", r)
		}
		if r := j.Step(dbpwRoot); !r.Acted {
			t.Errorf("dbpw = %+v, want Acted (the row stays stalled)", r)
		}
		if o := j.Outcome(mine.ID); o.Status != apply.StatusFailed || o.Reason != note {
			t.Errorf("decided place outcome = %+v", o)
		}
		sc.varIs(t, "e1", "sapi", "MONGO_SOURCE", moved)
		sc.scan(t, err)

		// continue: fails the same way, never as a stop
		rep, err = sc.execute(context.Background(), id, nil)
		if err == nil || rep.State != apply.RunFailed || errors.Is(err, apply.ErrStopped) {
			t.Fatalf("continue: state %s, err %v; want failed again", rep.State, err)
		}
		if r := sc.journal(t, id).Step("patch:e1"); !strings.Contains(r.Error, note) {
			t.Errorf("continue: patch:e1 = %+v", r)
		}
		sc.varIs(t, "e1", "sapi", "MONGO_SOURCE", moved)
		sc.scan(t, err)
	})
}

// I1: a place the user decided to delete (x) that changed in Railway after
// the database step fails the patch with a note saying how to finish.
func TestScenarioDriftDecidedDelete(t *testing.T) {
	d := &dbSim{pw: scMongoOld, primary: "smongo2", verify: "PRIMARY"}
	sc := newScenario(t, false, d)
	del, err := kinds.DeleteVariable(sc.st, sc.st.PlaceOf("e1", "sapi"), "MONGO_SOURCE", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	del.Group = scMongoRoot
	id := sc.prepare(t, sc.decideWith(t, scMongoRoot, scMongoNew, del))
	rep, err := sc.execute(context.Background(), id, func(e apply.Event) {
		if e.StepKey == dbpwRoot && e.Status == apply.StatusDone {
			sc.fake.SetVar("e1", "sapi", "MONGO_SOURCE", rootURL(scMongoOld, scMongoH, "/db2"), false)
		}
	})
	if err == nil || rep.State != apply.RunFailed {
		t.Fatalf("state %s, err %v; want failed", rep.State, err)
	}
	const note = "production · api MONGO_SOURCE: deleted in the decision · delete it in Railway, or abandon to keep it"
	if r := sc.journal(t, id).Step("patch:e1"); !strings.Contains(r.Error, note) {
		t.Errorf("patch:e1 = %+v, want %q", r, note)
	}
	sc.scan(t, err)
}

// I1: a write of the group that drifted before the run is refused by the
// pre-flight, naming its place, before the database is touched; an
// automatic rewrite still on the same user and hosts is not.
func TestScenarioPreflightChecksGroupWrites(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
		decided           bool
	}{
		{"decided", rootURL(scMongoOld, scMongoH, "/db2"), "production · api MONGO_SOURCE changed in Railway since the decision · decide it again", true},
		{"automatic, other host", rootURL(scMongoOld, "other.railway.internal:27017", "/db"), "production · api MONGO_SOURCE changed in Railway and no longer has this user and host · decide it again", false},
		{"automatic, same host", rootURL(scMongoOld, scMongoH, "/db2"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &dbSim{pw: scMongoOld, primary: "smongo2", verify: "PRIMARY"}
			sc := newScenario(t, false, d)
			live := sc.decide(t, scMongoRoot, kinds.SourceGenerate, scMongoNew)
			if tc.decided {
				live = sc.decideWith(t, scMongoRoot, scMongoNew, sc.handSet(t, scMongoRoot, "e1", "sapi", "MONGO_SOURCE", rootURL(scMongoNew, scMongoH, "/mine")))
			}
			id := sc.prepare(t, live)
			sc.fake.SetVar("e1", "sapi", "MONGO_SOURCE", tc.value, false)
			rep, err := sc.execute(context.Background(), id, nil)
			if tc.want == "" {
				mustFinish(t, rep, err)
				return
			}
			if err == nil || rep.State != apply.RunFailed {
				t.Fatalf("state %s, err %v; want failed", rep.State, err)
			}
			j := sc.journal(t, id)
			if r := j.Step(preRoot); r.Status != apply.StatusFailed || r.Error != tc.want || r.Acted {
				t.Errorf("preflight = %+v, want failed with %q", r, tc.want)
			}
			if len(d.log) != 0 || len(sc.fake.Mutations()) != 0 {
				t.Errorf("acted: database %v, %d mutations", d.log, len(sc.fake.Mutations()))
			}
			sc.scan(t, err)
		})
	}
}

// A Postgres change refused with stderr quoting both passwords and the
// statement: the run fails at its database step, and no secret reaches the
// journal, an event or the error (M8).
func TestScenarioPostgresFailureScrubbed(t *testing.T) {
	d := &dbSim{pw: scPgOld, next: scPgNew, alterFails: true}
	sc := newScenario(t, false, d)
	id := sc.prepare(t, sc.decide(t, scPgRoot, kinds.SourceGenerate, scPgNew))
	rep, err := sc.execute(context.Background(), id, nil)
	if err == nil || rep.State != apply.RunFailed {
		t.Fatalf("state %s, err %v; want failed", rep.State, err)
	}
	r := sc.journal(t, id).Step("dbpw:e1/spg:postgres")
	if r.Status != apply.StatusFailed || !strings.Contains(r.Error, "permission denied") || !strings.Contains(r.Error, "[redacted]") {
		t.Errorf("dbpw = %+v, want failed in psql's words, redacted", r)
	}
	if len(d.alters) != 0 || len(sc.fake.Mutations()) != 0 {
		t.Errorf("changed: %d alters, %d mutations", len(d.alters), len(sc.fake.Mutations()))
	}
	sc.scan(t, err)
}

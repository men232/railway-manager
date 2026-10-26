package kinds

import (
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
)

// Password fixtures. Each is long enough that the scrubber recognises it in
// any 8-character piece.
const (
	mongoOld = "old-mongo-root-pw-1a2b3c4d"
	mongoNew = "NEW-mongo-root-pw-9z8y7x6w"
	pgOld    = "old-pg-root-pw-5e6f7g8h"
	pgNew    = "NEW-pg-root-pw-1q2w3e4r"
	appOld   = "old-app-user-pw-2k3j4h5g"
	appNew   = "NEW-app-user-pw-6t7y8u9i"
	keyOld   = "old-keyfile-value-0123456789abcdef"
	keyNew   = "NEWkeyfileValue+abcdef0123456789="
)

var dbSecrets = []string{mongoOld, mongoNew, pgOld, pgNew, appOld, appNew, keyOld, keyNew}

func dbCatalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true,
			ServiceIDs: []string{"smongo", "smongo2", "spg", "sapi", "sweb", "sjob"}}},
		Services: []railway.Service{
			{ID: "smongo", Name: "mongo"}, {ID: "smongo2", Name: "mongo-2"}, {ID: "spg", Name: "pg"},
			{ID: "sapi", Name: "api"}, {ID: "sweb", Name: "web"}, {ID: "sjob", Name: "job"},
		},
	}}}
}

func dbRec(svc, name, unrendered, rendered string) railway.VarRecord {
	if rendered == "" {
		rendered = unrendered
	}
	return railway.VarRecord{EnvID: "e1", ServiceID: svc, Name: name, Unrendered: unrendered, Rendered: rendered}
}

// dbVars: a Mongo replica set (mongo, mongo-2 referencing its root
// password, KEYFILE copied literally), a Postgres, an api with a literal
// root URL and a user URL, and a web following mongo's MONGO_URL.
func dbVars() []railway.VarRecord {
	rootURL := "mongodb://root:" + mongoOld + "@mongo.railway.internal:27017"
	return []railway.VarRecord{
		dbRec("smongo", "MONGO_INITDB_ROOT_USERNAME", "root", ""),
		dbRec("smongo", "MONGO_INITDB_ROOT_PASSWORD", mongoOld, ""),
		dbRec("smongo", "MONGO_URL", "mongodb://${{MONGO_INITDB_ROOT_USERNAME}}:${{MONGO_INITDB_ROOT_PASSWORD}}@mongo.railway.internal:27017", rootURL),
		dbRec("smongo", "REPLICA_SET_NAME", "rs0", ""),
		dbRec("smongo", "KEYFILE", keyOld, ""),
		dbRec("smongo2", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}", mongoOld),
		dbRec("smongo2", "REPLICA_SET_NAME", "rs0", ""),
		dbRec("smongo2", "KEYFILE", keyOld, ""),
		dbRec("sapi", "MONGO_SOURCE", rootURL+"/db?authSource=admin", ""),
		dbRec("sapi", "APP_DB", "mongodb://app:"+appOld+"@mongo.railway.internal:27017/app", ""),
		dbRec("sweb", "MONGO_URL", "${{mongo.MONGO_URL}}", rootURL),
		// An app copying both root passwords and REPLICA_SET_NAME by
		// reference, without a KEYFILE: no member of either database.
		dbRec("sjob", "POSTGRES_PASSWORD", "${{pg.POSTGRES_PASSWORD}}", pgOld),
		dbRec("sjob", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}", mongoOld),
		dbRec("sjob", "REPLICA_SET_NAME", "${{mongo.REPLICA_SET_NAME}}", "rs0"),
		dbRec("spg", "POSTGRES_USER", "postgres", ""),
		dbRec("spg", "POSTGRES_PASSWORD", pgOld, ""),
	}
}

func dbSystem() []railway.SystemRecord {
	return []railway.SystemRecord{
		{EnvID: "e1", ServiceID: "smongo", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "mongo.railway.internal"},
		{EnvID: "e1", ServiceID: "smongo2", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "mongo-2.railway.internal"},
		{EnvID: "e1", ServiceID: "spg", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "pg.railway.internal"},
	}
}

func dbStore(t *testing.T) *store.Store {
	t.Helper()
	s := store.New()
	s.ReplaceCatalog(dbCatalog())
	s.ReplaceVariables("p1", dbVars(), dbSystem()...)
	return s
}

func dbRailway(t *testing.T) *railwaytest.Fake {
	t.Helper()
	f := railwaytest.New(t)
	f.SetCatalog(dbCatalog())
	for _, r := range dbVars() {
		f.SetVar(r.EnvID, r.ServiceID, r.Name, r.Unrendered, false)
	}
	return f
}

func dbPlace(svc string) domain.Place {
	return domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: svc}
}

// dbRow is the password row of group.
func dbRow(t *testing.T, s *store.Store, group string) rotation.PasswordRow {
	t.Helper()
	o := rotation.Options{EnvIDs: []string{"e1"}}
	rows := rotation.PasswordRows(s, rotation.Analyze(s, o), o)
	for _, r := range rows {
		if r.Key == group {
			return r
		}
	}
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	t.Fatalf("no row %s in %v", group, keys)
	return rotation.PasswordRow{}
}

var (
	mongoRoot = rotation.DBGroupKey("e1/smongo", "root")
	mongoApp  = rotation.DBGroupKey("e1/smongo", "app")
	mongoKey  = rotation.DBGroupKey("e1/smongo", rotation.KeyFileUser)
	pgRoot    = rotation.DBGroupKey("e1/spg", "postgres")
)

func mustDBPassword(t *testing.T, s *store.Store, group, source, value string) changeset.Change {
	t.Helper()
	c, err := DBPassword(s, dbRow(t, s, group), source, value, now)
	if err != nil {
		t.Fatalf("DBPassword %s: %v", group, err)
	}
	return c
}

// dbRunner is a fake runner with every client installed, ssh config
// answering for every service and ssh tunnels that listen on their socket.
// testHostKey is a dummy ed25519 host key; testHostKeyFP its fingerprint.
const (
	testHostKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIC/3HMGJU6ZTAKmx4j6OvVVZMxv/6Wx2e+nrJWCfN5xu"
	testHostKeyFP = "SHA256:u1QuOrYHORJv/vPkg03o7Lm+Tnjj+8j0VzPomvJuDOU"
)

func dbRunner(t *testing.T) *proc.Fake {
	t.Helper()
	r := &proc.Fake{}
	for _, n := range []string{"ssh", "mongosh", "psql", "redis-cli"} {
		r.SetPath(n, "/usr/bin/"+n)
	}
	r.On("railway", nil, proc.FakeReply{Stdout: "Host railway\n  HostName ssh.railway.test\n  User svc-user\n"})
	r.On("ssh", func(c proc.Cmd) bool {
		listen(t, forwardSocket(c))
		return true
	}, proc.FakeReply{})
	r.On("psql", func(c proc.Cmd) bool { return slices.Contains(c.Args, "--version") }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
	return r
}

// forwardSocket is the socket of an ssh -L argument.
func forwardSocket(c proc.Cmd) string {
	for i, a := range c.Args {
		if a == "-L" && i+1 < len(c.Args) {
			sock, _, _ := strings.Cut(c.Args[i+1], ":")
			return sock
		}
	}
	return ""
}

func forwardArg(c proc.Cmd) string {
	for i, a := range c.Args {
		if a == "-L" && i+1 < len(c.Args) {
			return c.Args[i+1]
		}
	}
	return ""
}

func listen(t *testing.T, socket string) {
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Errorf("listen %s: %v", socket, err)
		return
	}
	t.Cleanup(func() { _ = l.Close() })
}

func hasEnv(c proc.Cmd, kv string) bool { return slices.Contains(c.Env, kv) }

// onMongo scripts mongosh runs of the script holding marker, logged in
// with password.
func onMongo(r *proc.Fake, marker, password string, reply proc.FakeReply, extra ...string) {
	r.On("mongosh", func(c proc.Cmd) bool {
		if !strings.Contains(c.Args[len(c.Args)-1], marker) || !hasEnv(c, "RM_MONGO_PASSWORD="+password) {
			return false
		}
		for _, e := range extra {
			if !hasEnv(c, e) {
				return false
			}
		}
		return true
	}, reply)
}

// authFailed is a client refusing a login, its stderr quoting the
// password as a careless client might.
func authFailed(password string) proc.FakeReply {
	return proc.FakeReply{Stderr: "MongoServerError: Authentication failed for " + password + "\n", Err: &proc.ExitError{Code: 1, LastStderr: "Authentication failed for " + password}}
}

func okOut(out string) proc.FakeReply { return proc.FakeReply{Stdout: out + "\n"} }

// dbEnv is an Env on the fake Railway and runner, with the run's changes.
func dbEnv(t *testing.T, f *railwaytest.Fake, s *store.Store, r *proc.Fake, cs ...changeset.Change) *apply.Env {
	t.Helper()
	env, _ := newEnv(t, f, s)
	env.Proc = r
	env.Changes = cs
	return env
}

// ownStep returns c's own step with key prefix (preflight, dbpw, dbverify).
func ownStep(t *testing.T, c changeset.Change, prefix string) apply.Step {
	t.Helper()
	for _, st := range NewRegistry()[c.Kind].OwnSteps(c) {
		if strings.HasPrefix(st.Key(), prefix+":") {
			return st
		}
	}
	t.Fatalf("no %s step for %s", prefix, c.Kind)
	return nil
}

// noSecrets fails when any secret is in what: argv, env-free journal file,
// errors.
func noSecrets(t *testing.T, what, s string) {
	t.Helper()
	for _, sec := range dbSecrets {
		if strings.Contains(s, sec) {
			t.Errorf("%s holds a secret: %q", what, s)
		}
	}
}

// scanRun checks the runner's argv and the journal for secrets.
func scanRun(t *testing.T, env *apply.Env, r *proc.Fake) {
	t.Helper()
	for _, c := range r.Calls() {
		noSecrets(t, "argv of "+c.Name, strings.Join(c.Args, " "))
	}
	if env.Journal != nil {
		data, err := json.Marshal(env.Journal)
		if err != nil {
			t.Fatal(err)
		}
		noSecrets(t, "journal", string(data))
	}
}

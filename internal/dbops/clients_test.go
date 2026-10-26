package dbops

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/rotation"
)

// Fixture passwords. Each is long enough that any 8-character piece of it in
// an error counts as a leak.
const (
	curPw   = "cur-pw-0001-Aa'\"$"
	newPw   = "new-pw-0002-Bb'\"$"
	adminPw = "adm-pw-0003-Cc!%"
)

var allPw = []string{curPw, newPw, adminPw}

var (
	mongoTun = []Tunnel{
		{Socket: "/tmp/rm-tunnel-m0/mongodb.sock"},
		{Socket: "/tmp/rm-tunnel-m1/mongodb.sock"},
		{Socket: "/tmp/rm-tunnel-m2/mongodb.sock"},
	}
	pgTun    = Tunnel{Socket: "/tmp/rm-tunnel-p/.s.PGSQL.5432"}
	redisTun = Tunnel{Socket: "/tmp/rm-tunnel-r/s"}
)

func target(kind, user string, root bool) Target {
	return Target{DB: rotation.DB{Kind: kind, RootUser: "root"}, User: user, Root: root}
}

func rootCreds() Creds { return Creds{AdminUser: "root", Current: curPw, New: newPw} }

// instantRetry makes transient retries free and counts them.
func instantRetry(t *testing.T) *int {
	t.Helper()
	n := 0
	old := retrySleep
	t.Cleanup(func() { retrySleep = old })
	retrySleep = func(context.Context, time.Duration) error { n++; return nil }
	return &n
}

func envOf(c proc.Cmd, key string) (string, bool) {
	for _, kv := range c.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func envIs(key, val string) func(proc.Cmd) bool {
	return func(c proc.Cmd) bool { v, _ := envOf(c, key); return v == val }
}

// noSecretInArgv fails when any recorded call carries a password, or any
// 8-character piece of one, in its argv.
func noSecretInArgv(t *testing.T, f *proc.Fake) {
	t.Helper()
	for _, c := range f.Calls() {
		argv := c.Name + " " + strings.Join(c.Args, " ")
		if leaked(argv) {
			t.Errorf("secret in argv: %s", c.Name)
		}
	}
}

func leaked(s string) bool {
	for _, pw := range allPw {
		for _, v := range []string{pw, encodeURIComponent(pw)} {
			for i := 0; i+8 <= len(v); i++ {
				if strings.Contains(s, v[i:i+8]) {
					return true
				}
			}
		}
	}
	return false
}

func noSecretInErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	if leaked(err.Error()) {
		t.Fatalf("secret in error: %d bytes", len(err.Error()))
	}
}

// redisReject is redis-cli refusing a password: like the real client, it
// exits 0, says so on stderr and prints the error reply on stdout.
var redisReject = proc.FakeReply{Stdout: "NOAUTH Authentication required.\n", Stderr: "AUTH failed: WRONGPASS invalid username-password pair or user is disabled.\n"}

// pwIs matches a redis-cli call logged in with pw (and more, when given).
func pwIs(pw string, more func(proc.Cmd) bool) func(proc.Cmd) bool {
	return func(c proc.Cmd) bool {
		v, _ := envOf(c, "REDISCLI_AUTH")
		return v == pw && (more == nil || more(c))
	}
}

// exitErr is a failing client whose stderr echoes the password, as psql does
// with the failing statement.
func exitErr(stderr string) proc.FakeReply {
	return proc.FakeReply{Stderr: stderr, Err: &proc.ExitError{Code: 1, LastStderr: lastLine(stderr)}}
}

func TestMissing(t *testing.T) {
	cases := []struct {
		kind    string
		on      []string
		install string
		missing bool
	}{
		{"mongo", []string{"mongosh"}, "", false},
		{"postgres", []string{"psql", "brew"}, "", false},
		{"redis", []string{"redis-cli"}, "", false},
		{"ssh", []string{"ssh"}, "", false},
		{"mongo", []string{"brew"}, "brew install mongosh", true},
		{"mongo", []string{"apt-get"}, "npm install -g mongosh", true},
		{"mongo", []string{"dnf"}, "npm install -g mongosh", true},
		{"mongo", []string{"apk"}, "npm install -g mongosh", true},
		{"mongo", nil, "install mongosh", true},
		{"postgres", []string{"brew"}, "brew install libpq && brew link --force libpq", true},
		{"postgres", []string{"apt-get"}, "apt-get install postgresql-client", true},
		{"postgres", []string{"dnf"}, "dnf install postgresql", true},
		{"postgres", []string{"apk"}, "apk add postgresql", true},
		{"postgres", nil, "install psql", true},
		{"redis", []string{"brew"}, "brew install redis", true},
		{"redis", []string{"apt-get"}, "apt-get install redis-tools", true},
		{"redis", []string{"dnf"}, "dnf install redis", true},
		{"redis", []string{"apk"}, "apk add redis", true},
		{"redis", nil, "install redis-cli", true},
		{"ssh", []string{"apt-get"}, "apt-get install openssh-client", true},
		{"redis", []string{"apk", "apt-get", "brew"}, "brew install redis", true}, // detection order
		{"postgres", []string{"apk", "dnf"}, "dnf install postgresql", true},
	}
	for _, c := range cases {
		f := &proc.Fake{}
		for _, p := range c.on {
			f.SetPath(p, "/usr/bin/"+p)
		}
		install, missing, err := Missing(f, c.kind)
		if install != c.install || missing != c.missing || err != nil {
			t.Errorf("%s with %v: got %q %v, want %q %v", c.kind, c.on, install, missing, c.install, c.missing)
		}
	}
	if _, _, err := Missing(&proc.Fake{}, "oracle"); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestForward(t *testing.T) {
	cases := []struct {
		kind, private, host, sock string
		err                       bool
	}{
		{"mongo", "mongo.railway.internal", "127.0.0.1", "mongodb.sock", false},
		{"redis", "redis.railway.internal", "127.0.0.1", "s", false},
		{"postgres", "postgres.railway.internal", "postgres.railway.internal", ".s.PGSQL.5433", false},
		{"postgres", "", "", "", true},
		{"postgres", "-oProxyCommand=x", "", "", true},
		{"postgres", "a:b", "", "", true},
		{"oracle", "x", "", "", true},
	}
	for _, c := range cases {
		host, sock, err := Forward(c.kind, 5433, c.private)
		if host != c.host || sock != c.sock || (err != nil) != c.err {
			t.Errorf("%s %q: got %q %q %v", c.kind, c.private, host, sock, err)
		}
	}
}

// A Postgres password proof goes through a forward to the
// container's private address, never 127.0.0.1, which the template trusts.
func TestPostgresProofUsesPrivateAddress(t *testing.T) {
	instantRetry(t)
	fakeNet(t, func(string) error { return nil })
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{Pid: 7})
	f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.4\n"})
	f.On("psql", nil, proc.FakeReply{Stdout: "1\n"})
	host, sock, err := Forward("postgres", 5432, "postgres.railway.internal")
	if err != nil {
		t.Fatal(err)
	}
	tun, err := Open(context.Background(), f, knownHosts(t, "ssh.railway.com"), "u@ssh.railway.com", 5432, host, sock, noRecord)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	if err := Login(context.Background(), f, target("postgres", "postgres", true), tun, curPw); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	ssh := calls[0]
	if !slices.Contains(ssh.Args, tun.Socket+":postgres.railway.internal:5432") {
		t.Fatalf("ssh forward %v", ssh.Args)
	}
	psql := calls[len(calls)-1]
	if got := psql.Args[len(psql.Args)-1]; !strings.Contains(got, "host='"+tun.Dir()+"' hostaddr='' port=5432 ") {
		t.Fatalf("conninfo %q", got)
	}
	noSecretInArgv(t, f)
}

func TestLoginEachClient(t *testing.T) {
	instantRetry(t)
	cases := []struct {
		kind  string
		tun   Tunnel
		reply proc.FakeReply
		check func(t *testing.T, c proc.Cmd)
	}{
		{"mongo", mongoTun[0], proc.FakeReply{Stdout: "OK\n"}, func(t *testing.T, c proc.Cmd) {
			if want := []string{"--nodb", "--norc", "--quiet", "--eval", mongoLoginScript}; !slices.Equal(c.Args, want) {
				t.Errorf("argv %q", c.Args)
			}
			uri, _ := envOf(c, "RM_MONGO_URI")
			if uri != "mongodb://app@%2Ftmp%2Frm-tunnel-m0%2Fmongodb.sock/?directConnection=true&authSource=admin&connectTimeoutMS=10000&serverSelectionTimeoutMS=10000" {
				t.Errorf("uri %q", uri)
			}
			if pw, _ := envOf(c, "RM_MONGO_PASSWORD"); pw != curPw {
				t.Error("password not in env")
			}
			// an RM_DRY_RUN=1 left in our shell would make a change a no-op
			for _, v := range []string{"RM_DRY_RUN", "RM_MONGO_URI", "RM_MONGO_PASSWORD", "RM_TARGET_USER", "RM_TARGET_DB", "RM_NEW_PASSWORD"} {
				if !slices.Contains(c.Unset, v) {
					t.Errorf("our %s reaches mongosh", v)
				}
			}
		}},
		{"postgres", pgTun, proc.FakeReply{Stdout: "1\n"}, func(t *testing.T, c proc.Cmd) {
			want := []string{"-X", "-q", "-t", "-A", "-v", "ON_ERROR_STOP=1", "-d",
				"host='/tmp/rm-tunnel-p' hostaddr='' port=5432 user='app' dbname='postgres' connect_timeout=10 sslmode=disable gssencmode=disable target_session_attrs=any require_auth=scram-sha-256"}
			if !slices.Equal(c.Args, want) {
				t.Errorf("argv %q", c.Args)
			}
			if pw, _ := envOf(c, "PGPASSWORD"); pw != curPw || c.Stdin != "SELECT 1;\n" {
				t.Errorf("env/stdin wrong: stdin %q", c.Stdin)
			}
			for _, v := range []string{"PGHOSTADDR", "PGHOST", "PGPORT", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS", "PGSSLMODE",
				"PGREQUIREAUTH", "PGTARGETSESSIONATTRS", "PGDATABASE", "PGUSER",
				"PGCHANNELBINDING", "PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT", "PGCLIENTENCODING", "PGSYSCONFDIR"} {
				if !slices.Contains(c.Unset, v) {
					t.Errorf("%s reaches psql", v)
				}
			}
		}},
		{"redis", redisTun, proc.FakeReply{Stdout: "PONG\n"}, func(t *testing.T, c proc.Cmd) {
			if want := []string{"-s", "/tmp/rm-tunnel-r/s", "--user", "app", "--no-auth-warning"}; !slices.Equal(c.Args, want) {
				t.Errorf("argv %q", c.Args)
			}
			if pw, _ := envOf(c, "REDISCLI_AUTH"); pw != curPw || c.Stdin != "PING\n" {
				t.Errorf("env/stdin wrong: stdin %q", c.Stdin)
			}
			if !slices.Contains(c.Unset, "REDISCLI_RCFILE") {
				t.Error("REDISCLI_RCFILE reaches redis-cli")
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			f := &proc.Fake{}
			f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 17.0 (Homebrew)\n"})
			if c.kind == "redis" {
				f.On("redis-cli", pwIs(curPw, nil), c.reply)
				f.On("redis-cli", nil, redisReject)
			}
			f.On(clientOf[c.kind], nil, c.reply)
			tun := c.tun
			if err := Login(context.Background(), f, target(c.kind, "app", false), &tun, curPw); err != nil {
				t.Fatal(err)
			}
			calls := f.Calls()
			c.check(t, calls[len(calls)-1])
			noSecretInArgv(t, f)
		})
	}
}

func TestLoginRefusals(t *testing.T) {
	instantRetry(t)
	cases := []struct {
		kind  string
		tun   Tunnel
		reply proc.FakeReply
	}{
		{"mongo", mongoTun[0], proc.FakeReply{Stdout: "NO_AUTH\n", Err: &proc.ExitError{Code: 2}}},
		{"mongo", Tunnel{Socket: "/tmp/rm-tunnel-m0/s"}, proc.FakeReply{Stdout: "OK\n"}}, // not a .sock
		{"postgres", Tunnel{Socket: "/tmp/rm-tunnel-p/s"}, proc.FakeReply{Stdout: "1\n"}},
		{"redis", redisTun, redisReject},
	}
	for _, c := range cases {
		f := &proc.Fake{}
		f.On(clientOf[c.kind], nil, c.reply)
		tun := c.tun
		if err := Login(context.Background(), f, target(c.kind, "app", false), &tun, curPw); err == nil {
			t.Errorf("%s %s: want an error", c.kind, c.tun.Socket)
		}
	}
	if err := Login(context.Background(), &proc.Fake{}, target("redis", "app", false), nil, curPw); err == nil {
		t.Error("nil tunnel accepted")
	}
}

func TestPsqlRequireAuthByVersion(t *testing.T) {
	instantRetry(t)
	for _, v := range []string{"psql (PostgreSQL) 16.0\n", "psql (PostgreSQL) 18beta1\n"} {
		f := &proc.Fake{}
		f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: v})
		f.On("psql", nil, proc.FakeReply{Stdout: "1\n"})
		tun := pgTun
		if err := Login(context.Background(), f, target("postgres", "app", false), &tun, curPw); err != nil {
			t.Fatal(err)
		}
		calls := f.Calls()
		if !strings.Contains(calls[len(calls)-1].Args[7], "require_auth=scram-sha-256") {
			t.Errorf("%q: no require_auth", v)
		}
		// Probed once per runner.
		if err := Login(context.Background(), f, target("postgres", "app", false), &tun, curPw); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range f.Calls() {
			if c.Args[0] == "--version" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("psql --version run %d times", n)
		}
	}
	// A psql without require_auth is refused before any login or change,
	// naming the install of a newer one.
	cases := []struct {
		version string
		on      []string
		want    string
	}{
		{"psql (PostgreSQL) 15.8 (Ubuntu 15.8-1)\n", []string{"apt-get"}, "psql 15 is too old: 16 or newer is needed to insist on a password login · apt-get install postgresql-client-16"},
		{"psql (PostgreSQL) 14.2\n", []string{"brew"}, "psql 14 is too old: 16 or newer is needed to insist on a password login · brew install libpq && brew link --force libpq"},
		{"psql (PostgreSQL) 9.6.24\n", nil, "psql 9 is too old: 16 or newer is needed to insist on a password login · install psql 16 or newer"},
	}
	for _, c := range cases {
		f := &proc.Fake{}
		for _, p := range c.on {
			f.SetPath(p, "/usr/bin/"+p)
		}
		f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: c.version})
		f.On("psql", nil, proc.FakeReply{Stdout: "OK\n"})
		_, err := SetPassword(context.Background(), f, target("postgres", "app", false), []Tunnel{pgTun}, Creds{AdminUser: "root", Admin: adminPw, New: newPw})
		if err == nil || err.Error() != c.want || len(f.Calls()) != 1 {
			t.Errorf("%q: err=%v calls=%d", c.version, err, len(f.Calls()))
		}
		noSecretInErr(t, err)
	}
	// A failed or unreadable probe is an error, not a downgrade.
	for _, bad := range []proc.FakeReply{{Stdout: "garbage\n"}, {Err: errors.New("boom")}} {
		f := &proc.Fake{}
		f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, bad)
		f.On("psql", nil, proc.FakeReply{Stdout: "1\n"})
		err := Login(context.Background(), f, target("postgres", "app", false), &pgTun, curPw)
		if err == nil || !strings.Contains(err.Error(), "psql --version") || len(f.Calls()) != 1 {
			t.Errorf("%q: err=%v calls=%d", bad.Stdout, err, len(f.Calls()))
		}
	}
}

// A role name with ' and \ reaches psql as the variable u, never spliced
// into the statement: psql quotes it for the server's string rules.
func TestPgUserLookupQuotesName(t *testing.T) {
	instantRetry(t)
	name := `o'k\'; DROP ROLE x; --`
	f := &proc.Fake{}
	f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
	f.On("psql", nil, proc.FakeReply{Stdout: "OK\n"})
	if _, err := Check(context.Background(), f, target("postgres", name, false), []Tunnel{pgTun}, Creds{AdminUser: "root", Admin: adminPw, New: newPw}); err != nil {
		t.Fatal(err)
	}
	c := f.Calls()[1]
	if !slices.Equal(c.Args[8:], []string{"-v", "u=" + name}) {
		t.Errorf("args %q", c.Args)
	}
	if c.Stdin != "SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'u') THEN 'OK' ELSE 'NO_USER' END;\n" {
		t.Errorf("stdin %q", c.Stdin)
	}
}

// Every client fails with stderr that echoes the password (raw, URL-encoded,
// quoted in a statement, cut short): the error names the client and holds
// no piece of any password.
func TestFailingClientsAreScrubbed(t *testing.T) {
	instantRetry(t)
	// echo is stderr quoting each password the call knows in every way a
	// client might.
	echo := func(pws ...string) string {
		var lines []string
		for _, pw := range pws {
			lines = append(lines,
				"MongoServerError: bad auth mongodb://root:"+encodeURIComponent(pw)+"@x",
				"ERROR:  syntax error at or near \""+pw+"\"",
				"LINE 1: ALTER USER \"app\" WITH PASSWORD '"+pw[:12]+"...",
				"                                        ^",
				"AUTH failed: WRONGPASS "+pw[3:15])
		}
		return strings.Join(lines, "\n")
	}
	type tc struct {
		known []string
		do    func(f *proc.Fake) error
	}
	run := map[string]tc{
		"login mongo": {[]string{curPw}, func(f *proc.Fake) error {
			return Login(context.Background(), f, target("mongo", "app", false), &mongoTun[0], curPw)
		}},
		"login postgres": {[]string{curPw}, func(f *proc.Fake) error {
			return Login(context.Background(), f, target("postgres", "app", false), &pgTun, curPw)
		}},
		"login redis": {[]string{curPw}, func(f *proc.Fake) error {
			return Login(context.Background(), f, target("redis", "app", false), &redisTun, curPw)
		}},
		"set mongo": {[]string{curPw, newPw}, func(f *proc.Fake) error {
			_, err := SetPassword(context.Background(), f, target("mongo", "root", true), mongoTun, rootCreds())
			return err
		}},
		"set postgres": {[]string{curPw, newPw}, func(f *proc.Fake) error {
			_, err := SetPassword(context.Background(), f, target("postgres", "root", true), []Tunnel{pgTun}, rootCreds())
			return err
		}},
		"set redis user": {allPw, func(f *proc.Fake) error {
			c := Creds{AdminUser: "default", Admin: adminPw, Current: curPw, New: newPw}
			_, err := SetPassword(context.Background(), f, target("redis", "app", false), []Tunnel{redisTun}, c)
			return err
		}},
		"verify mongo": {[]string{curPw}, func(f *proc.Fake) error {
			_, err := VerifyMongo(context.Background(), f, "root", mongoTun[:1], curPw, func(context.Context, time.Duration) error { return nil })
			return err
		}},
	}
	for name, c := range run {
		t.Run(name, func(t *testing.T) {
			f := &proc.Fake{}
			f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.1\n"})
			for _, client := range []string{"mongosh", "psql", "redis-cli"} {
				e := echo(c.known...)
				reply := proc.FakeReply{Stdout: "NOT OK " + c.known[0] + "\n", Stderr: e, Err: &proc.ExitError{Code: 1, LastStderr: e}}
				if client == "redis-cli" {
					reply.Err = nil // redis-cli exits 0 on a failed AUTH
				}
				f.On(client, nil, reply)
			}
			err := c.do(f)
			noSecretInErr(t, err)
			if !strings.Contains(err.Error(), "[redacted]") {
				t.Errorf("client words lost: %v", err)
			}
			noSecretInArgv(t, f)
		})
	}
}

func TestSetPasswordCurrentThenNew(t *testing.T) {
	instantRetry(t)
	cases := []struct {
		kind, user, envKey string
		tuns               []Tunnel
		ok                 proc.FakeReply
	}{
		{"mongo", "root", "RM_MONGO_PASSWORD", mongoTun[:1], proc.FakeReply{Stdout: "OK\n"}},
		{"postgres", "root", "PGPASSWORD", []Tunnel{pgTun}, proc.FakeReply{}},
		{"redis", "default", "REDISCLI_AUTH", []Tunnel{redisTun}, proc.FakeReply{Stdout: "OK\n"}},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			for _, dbHas := range []string{curPw, newPw} {
				f := &proc.Fake{}
				f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
				f.On(clientOf[c.kind], envIs(c.envKey, dbHas), c.ok)
				if c.kind == "redis" {
					f.On("redis-cli", nil, redisReject)
				}
				f.On(clientOf[c.kind], nil, exitErr("authentication failed"))
				usedNew, err := SetPassword(context.Background(), f, target(c.kind, c.user, true), c.tuns, rootCreds())
				if err != nil || usedNew != (dbHas == newPw) {
					t.Fatalf("db has new=%v: usedNew=%v err=%v", dbHas == newPw, usedNew, err)
				}
				var tried []string
				for _, call := range f.Calls() {
					if v, ok := envOf(call, c.envKey); ok {
						tried = append(tried, v)
					}
				}
				want := []string{curPw}
				if dbHas == newPw {
					want = []string{curPw, newPw}
				}
				if !slices.Equal(tried, want) {
					t.Fatalf("tried %d logins, want %d", len(tried), len(want))
				}
				noSecretInArgv(t, f)
			}
		})
	}
}

func TestSetPasswordBothFail(t *testing.T) {
	instantRetry(t)
	f := &proc.Fake{}
	f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
	f.On("psql", nil, exitErr("FATAL: password authentication failed for user \"root\""))
	_, err := SetPassword(context.Background(), f, target("postgres", "root", true), []Tunnel{pgTun}, rootCreds())
	noSecretInErr(t, err)
	if !strings.Contains(err.Error(), "current password: psql: FATAL") || !strings.Contains(err.Error(), "new password: psql: FATAL") {
		t.Fatalf("err %v", err)
	}
}

func TestSetPasswordStatements(t *testing.T) {
	instantRetry(t)
	t.Run("postgres", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
		f.On("psql", func(c proc.Cmd) bool { return strings.Contains(c.Stdin, "pg_roles") }, proc.FakeReply{Stdout: "OK\n"})
		f.On("psql", nil, proc.FakeReply{})
		c := Creds{AdminUser: "root", Admin: adminPw, New: newPw}
		if _, err := SetPassword(context.Background(), f, target("postgres", `ap"p`, false), []Tunnel{pgTun}, c); err != nil {
			t.Fatal(err)
		}
		calls := f.Calls()
		if len(calls) != 3 || !slices.Equal(calls[1].Args[8:], []string{"-v", `u=ap"p`}) {
			t.Fatalf("user not looked up first: %d calls", len(calls))
		}
		call := calls[2]
		want := regexp.MustCompile(`^ALTER USER "ap""p" WITH PASSWORD 'SCRAM-SHA-256\$4096:[A-Za-z0-9+/]{22}==\$[A-Za-z0-9+/]{43}=:[A-Za-z0-9+/]{43}=';\n$`)
		if !want.MatchString(call.Stdin) || leaked(call.Stdin) {
			t.Error("statement wrong")
		}
		if pw, _ := envOf(call, "PGPASSWORD"); pw != adminPw || !strings.Contains(call.Args[7], "user='root'") {
			t.Errorf("admin login wrong: %q", call.Args[7])
		}
	})
	t.Run("redis root default", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("redis-cli", nil, proc.FakeReply{Stdout: "OK\n"})
		c := Creds{AdminUser: "default", Current: curPw, New: newPw}
		if _, err := SetPassword(context.Background(), f, target("redis", "default", true), []Tunnel{redisTun}, c); err != nil {
			t.Fatal(err)
		}
		calls := f.Calls()
		if len(calls) != 1 || calls[0].Stdin != "CONFIG SET requirepass "+redisArg(newPw)+"\n" {
			t.Errorf("calls %d", len(calls))
		}
	})
	t.Run("redis user", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("redis-cli", func(c proc.Cmd) bool { return strings.HasPrefix(c.Stdin, "ACL GETUSER") }, proc.FakeReply{Stdout: "flags\non\npasswords\n"})
		f.On("redis-cli", nil, proc.FakeReply{Stdout: "OK\n"})
		c := Creds{AdminUser: "default", Admin: adminPw, New: newPw}
		if _, err := SetPassword(context.Background(), f, target("redis", "app", false), []Tunnel{redisTun}, c); err != nil {
			t.Fatal(err)
		}
		calls := f.Calls()
		if len(calls) != 2 || calls[0].Stdin != "ACL GETUSER \"app\"\n" ||
			calls[1].Stdin != `ACL SETUSER "app" resetpass `+redisArg(">"+newPw)+"\n" {
			t.Errorf("calls %d", len(calls))
		}
		if pw, _ := envOf(calls[1], "REDISCLI_AUTH"); pw != adminPw || calls[1].Args[3] != "default" {
			t.Error("admin login wrong")
		}
		noSecretInArgv(t, f)
	})
	t.Run("redis user missing is not created", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("redis-cli", func(c proc.Cmd) bool { return strings.HasPrefix(c.Stdin, "ACL GETUSER") }, proc.FakeReply{Stdout: "\n"})
		f.On("redis-cli", nil, proc.FakeReply{Stdout: "OK\n"})
		c := Creds{AdminUser: "default", Admin: adminPw, New: newPw}
		_, err := SetPassword(context.Background(), f, target("redis", "app", false), []Tunnel{redisTun}, c)
		if err == nil || !strings.Contains(err.Error(), "user app not found") || len(f.Calls()) != 1 {
			t.Fatalf("err=%v calls=%d", err, len(f.Calls()))
		}
	})
}

func TestRedisArg(t *testing.T) {
	cases := map[string]string{
		`abc`:      `"abc"`,
		`a"b\c`:    `"a\"b\\c"`,
		"a\nb\x01": `"a\x0ab\x01"`,
	}
	for in, want := range cases {
		if got := redisArg(in); got != want {
			t.Errorf("redisArg(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestMongoSetPasswordFindsPrimary(t *testing.T) {
	instantRetry(t)
	sock := func(i int) func(proc.Cmd) bool {
		return func(c proc.Cmd) bool {
			uri, _ := envOf(c, "RM_MONGO_URI")
			return strings.Contains(uri, encodeURIComponent(mongoTun[i].Socket))
		}
	}
	f := &proc.Fake{}
	f.On("mongosh", sock(0), proc.FakeReply{Stdout: "NOT_PRIMARY\n"})
	f.On("mongosh", sock(1), exitErr("MongoNetworkError: boom "+adminPw))
	f.On("mongosh", sock(2), proc.FakeReply{Stdout: "OK\n"})
	c := Creds{AdminUser: "root", Admin: adminPw, New: newPw}
	usedNew, err := SetPassword(context.Background(), f, target("mongo", "app", false), mongoTun, c)
	if err != nil || usedNew {
		t.Fatalf("usedNew=%v err=%v", usedNew, err)
	}
	calls := f.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls %d", len(calls))
	}
	last := calls[2]
	if u, _ := envOf(last, "RM_TARGET_USER"); u != "app" {
		t.Error("target user")
	}
	if pw, _ := envOf(last, "RM_NEW_PASSWORD"); pw != newPw {
		t.Error("new password not in env")
	}
	if pw, _ := envOf(last, "RM_MONGO_PASSWORD"); pw != adminPw {
		t.Error("admin password not in env")
	}
	if _, dry := envOf(last, "RM_DRY_RUN"); dry || !slices.Equal(last.Args, []string{"--nodb", "--norc", "--quiet", "--eval", mongoChangeScript}) {
		t.Errorf("argv %q", last.Args)
	}
	noSecretInArgv(t, f)

	// No member is the writable primary.
	f = &proc.Fake{}
	f.On("mongosh", sock(1), exitErr("MongoNetworkError: boom "+adminPw))
	f.On("mongosh", nil, proc.FakeReply{Stdout: "NOT_PRIMARY\n"})
	_, err = SetPassword(context.Background(), f, target("mongo", "app", false), mongoTun, c)
	noSecretInErr(t, err)
	if !strings.Contains(err.Error(), "no writable primary") || !strings.Contains(err.Error(), "member 1: not primary") ||
		!strings.Contains(err.Error(), "member 2: mongosh: MongoNetworkError") {
		t.Fatalf("err %v", err)
	}
}

// A Mongo user is the user of one database, the one its URLs
// authenticate against: the change looks up exactly that user and the
// login proves the password there. No auth database means admin.
func TestMongoTargetAuthDB(t *testing.T) {
	instantRetry(t)
	c := Creds{AdminUser: "root", Admin: adminPw, New: newPw}
	for _, tc := range []struct{ authDB, want string }{{"appdb", "appdb"}, {"", "admin"}} {
		f := &proc.Fake{}
		f.On("mongosh", nil, proc.FakeReply{Stdout: "OK\n"})
		tg := target("mongo", "app", false)
		tg.AuthDB = tc.authDB
		if _, err := SetPassword(context.Background(), f, tg, mongoTun[:1], c); err != nil {
			t.Fatal(err)
		}
		change := f.Calls()[0]
		if db, _ := envOf(change, "RM_TARGET_DB"); db != tc.want {
			t.Errorf("auth db %q: RM_TARGET_DB = %q, want %q", tc.authDB, db, tc.want)
		}
		if uri, _ := envOf(change, "RM_MONGO_URI"); !strings.Contains(uri, "authSource=admin") {
			t.Errorf("the admin logs in against admin: %q", uri)
		}
		tun := mongoTun[0]
		if err := Login(context.Background(), f, tg, &tun, newPw); err != nil {
			t.Fatal(err)
		}
		login := f.Calls()[1]
		if uri, _ := envOf(login, "RM_MONGO_URI"); !strings.Contains(uri, "authSource="+tc.want+"&") {
			t.Errorf("auth db %q: login uri %q, want authSource=%s", tc.authDB, uri, tc.want)
		}
	}
	if !strings.Contains(mongoChangeScript, "db: process.env.RM_TARGET_DB") || strings.Contains(mongoChangeScript, "findOne({ user: name }") {
		t.Error("the change must look the user up in its own database only")
	}
}

func TestMongoNoUser(t *testing.T) {
	instantRetry(t)
	f := &proc.Fake{}
	f.On("mongosh", nil, proc.FakeReply{Stdout: "NO_USER\n", Err: &proc.ExitError{Code: 3}})
	c := Creds{AdminUser: "root", Admin: adminPw, New: newPw}
	_, err := SetPassword(context.Background(), f, target("mongo", "app", false), mongoTun, c)
	if err == nil || !strings.Contains(err.Error(), "user app not found") || len(f.Calls()) != 1 {
		t.Fatalf("err=%v calls=%d", err, len(f.Calls()))
	}
}

// An exec.ErrWaitDelay after the change: the database may hold the new
// password, so nothing else is tried and the error says the output may be
// incomplete, not that the change failed.
func TestSetPasswordIncompleteOutput(t *testing.T) {
	instantRetry(t)
	wd := fmt.Errorf("run x: %w", exec.ErrWaitDelay)
	for _, kind := range []string{"mongo", "postgres", "redis"} {
		t.Run(kind, func(t *testing.T) {
			f := &proc.Fake{}
			f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
			f.On(clientOf[kind], nil, proc.FakeReply{Stdout: "O", Err: wd})
			tuns := map[string][]Tunnel{"mongo": mongoTun, "postgres": {pgTun}, "redis": {redisTun}}[kind]
			_, err := SetPassword(context.Background(), f, target(kind, "root", true), tuns, rootCreds())
			if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "output may be incomplete") ||
				strings.Contains(err.Error(), "failed") {
				t.Fatalf("err %v", err)
			}
			n := 0
			for _, c := range f.Calls() {
				if c.Name == clientOf[kind] && (len(c.Args) == 0 || c.Args[0] != "--version") {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d client runs after an incomplete one", n)
			}
		})
	}
}

func TestSetPasswordNoCreds(t *testing.T) {
	cases := []struct {
		t Target
		c Creds
	}{
		{target("postgres", "root", true), Creds{AdminUser: "root", New: newPw}},
		{target("postgres", "root", true), Creds{Current: curPw, New: newPw}},
		{target("postgres", "app", false), Creds{AdminUser: "root", Current: curPw, New: newPw}},
		{target("postgres", "root", true), Creds{AdminUser: "root", Current: curPw}},
	}
	for i, c := range cases {
		f := &proc.Fake{}
		_, err := SetPassword(context.Background(), f, c.t, []Tunnel{pgTun}, c.c)
		if err == nil || err.Error() != "current credentials could not be read from Railway" || len(f.Calls()) != 0 {
			t.Errorf("%d: err=%v", i, err)
		}
	}
	if _, err := SetPassword(context.Background(), &proc.Fake{}, target("postgres", "root", true), nil, rootCreds()); err == nil {
		t.Error("no tunnel accepted")
	}
}

func TestCheckChangesNothing(t *testing.T) {
	instantRetry(t)
	t.Run("mongo", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("mongosh", nil, proc.FakeReply{Stdout: "OK\n"})
		if _, err := Check(context.Background(), f, target("mongo", "root", true), mongoTun, rootCreds()); err != nil {
			t.Fatal(err)
		}
		if v, _ := envOf(f.Calls()[0], "RM_DRY_RUN"); v != "1" {
			t.Error("mongo check not dry")
		}
	})
	t.Run("postgres", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
		f.On("psql", nil, proc.FakeReply{Stdout: "OK\n"})
		if _, err := Check(context.Background(), f, target("postgres", `app`, false), []Tunnel{pgTun}, Creds{AdminUser: "root", Admin: adminPw, New: newPw}); err != nil {
			t.Fatal(err)
		}
		if c := f.Calls()[1]; strings.Contains(c.Stdin, "ALTER") || !strings.Contains(c.Stdin, "rolname = :'u'") || !slices.Equal(c.Args[8:], []string{"-v", "u=app"}) {
			t.Errorf("stdin %q args %q", c.Stdin, c.Args)
		}
		f = &proc.Fake{}
		f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
		f.On("psql", nil, proc.FakeReply{Stdout: "NO_USER\n"})
		if _, err := Check(context.Background(), f, target("postgres", `app`, false), []Tunnel{pgTun}, Creds{AdminUser: "root", Admin: adminPw, New: newPw}); err == nil || !strings.Contains(err.Error(), "user app not found") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("redis", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("redis-cli", func(c proc.Cmd) bool { return strings.HasPrefix(c.Stdin, "ACL GETUSER") }, proc.FakeReply{Stdout: "flags\n"})
		f.On("redis-cli", pwIs(curPw, func(c proc.Cmd) bool { return c.Stdin == "PING\n" }), proc.FakeReply{Stdout: "PONG\n"})
		f.On("redis-cli", nil, redisReject)
		if _, err := Check(context.Background(), f, target("redis", "app", false), []Tunnel{redisTun}, Creds{AdminUser: "default", Admin: adminPw, New: newPw}); err != nil {
			t.Fatal(err)
		}
		if _, err := Check(context.Background(), f, target("redis", "default", true), []Tunnel{redisTun}, Creds{AdminUser: "default", Current: curPw, New: newPw}); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.Calls() {
			if strings.Contains(c.Stdin, "SET") {
				t.Errorf("check wrote: %q", strings.Fields(c.Stdin)[0])
			}
		}
	})
}

func TestClientTransientRetryAndTimeout(t *testing.T) {
	waits := instantRetry(t)
	f := &proc.Fake{}
	n := 0
	f.On("redis-cli", func(proc.Cmd) bool { n++; return n == 1 }, proc.FakeReply{Stderr: "Could not connect to Redis at /tmp/x/s: Connection refused\n"})
	f.On("redis-cli", pwIs(curPw, nil), proc.FakeReply{Stdout: "PONG\n"})
	f.On("redis-cli", nil, redisReject)
	if err := Login(context.Background(), f, target("redis", "app", false), &redisTun, curPw); err != nil || *waits != 1 {
		t.Fatalf("err=%v waits=%d", err, *waits)
	}

	f = &proc.Fake{}
	f.On("redis-cli", nil, proc.FakeReply{Err: context.DeadlineExceeded})
	err := Login(context.Background(), f, target("redis", "app", false), &redisTun, curPw)
	if err == nil || !strings.Contains(err.Error(), "redis-cli timed out after 60 s") {
		t.Fatalf("err %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = Login(ctx, &proc.Fake{}, target("redis", "app", false), &redisTun, curPw)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestScrub(t *testing.T) {
	if scrub(nil, curPw) != nil {
		t.Fatal("nil error made non-nil")
	}
	raw := errors.New("a " + curPw + " b " + encodeURIComponent(newPw) + " c " + newPw[2:11] + " d " + strings.ReplaceAll(adminPw, "'", "''"))
	err := scrub(raw, allPw...)
	if leaked(err.Error()) || !strings.HasPrefix(err.Error(), "a [redacted] b [redacted] c [redacted] d [redacted]") {
		t.Fatalf("scrubbed to %d bytes", len(err.Error()))
	}
	if errors.Unwrap(err) != nil {
		t.Error("a scrubbed error must not unwrap to the raw one")
	}
	for _, sentinel := range []error{ErrIncomplete, context.Canceled, context.DeadlineExceeded} {
		if err := scrub(fmt.Errorf("x %s: %w", curPw, sentinel), curPw); !errors.Is(err, sentinel) || leaked(err.Error()) {
			t.Errorf("%v lost", sentinel)
		}
	}
	// A short password is redacted whole; its pieces are not hunted.
	if got := scrub(errors.New("pw abc1 and ab"), "abc1").Error(); got != "pw [redacted] and ab" {
		t.Errorf("got %q", got)
	}
	// Control characters are gone too.
	if got := scrub(errors.New("a\x1b[31mb\nc")).Error(); got != "ab c" {
		t.Errorf("got %q", got)
	}
}

func TestEncodeURIComponent(t *testing.T) {
	if got := encodeURIComponent("/tmp/a b@c:d?e#f%g!~*'()_.-"); got != "%2Ftmp%2Fa%20b%40c%3Ad%3Fe%23f%25g!~*'()_.-" {
		t.Errorf("got %s", got)
	}
}

func TestTunnelDirInConninfoQuoted(t *testing.T) {
	got, err := pgConninfo(Tunnel{Socket: `/tmp/rm-tunnel-'x\/.s.PGSQL.6543`}, `o'k`)
	if err != nil || got != `host='/tmp/rm-tunnel-\'x\\' hostaddr='' port=6543 user='o\'k' dbname='postgres' connect_timeout=10 sslmode=disable gssencmode=disable target_session_attrs=any require_auth=scram-sha-256` {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func errorsWaitDelay() error { return fmt.Errorf("run mongosh: %w", exec.ErrWaitDelay) }

// psql's echo of the failing statement is dropped even when no password is
// known to look for; a long message is cut to 400 characters.
func TestDetailDropsStatementEcho(t *testing.T) {
	got := detail("", "ERROR:  bad\nLINE 1: ALTER USER \"a\" WITH PASSWORD 'x\n                    ^\n", errors.New("exit 3"), nil)
	if got != "ERROR:  bad" {
		t.Errorf("got %q", got)
	}
	if got := detail(strings.Repeat("é", 500), "", errors.New("x"), nil); len([]rune(got)) != 401 {
		t.Errorf("not cut: %d", len([]rune(got)))
	}
	if got := detail("", "", errors.New("exit status 2"), nil); got != "exit status 2" {
		t.Errorf("got %q", got)
	}
}

// Known answer: computed independently with Python's hashlib and hmac,
//
//	sp = hashlib.pbkdf2_hmac("sha256", b"pencil", bytes(range(16)), 4096, 32)
//	ck = hmac.new(sp, b"Client Key", sha256).digest(); stored = sha256(ck)
//	server = hmac.new(sp, b"Server Key", sha256).digest()
//
// printed as SCRAM-SHA-256$4096:<b64 salt>$<b64 stored>:<b64 server>, the
// format psql's \password sends (RFC 5802/7677, PostgreSQL scram_build_secret).
func TestSCRAMVerifierKnownAnswer(t *testing.T) {
	got, err := scramVerifier("pencil", []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	want := "SCRAM-SHA-256$4096:AAECAwQFBgcICQoLDA0ODw==$zHCdol2044/ZyWzPLi7oxApCkamKw9Z+E4U/QApd/5Y=:dd5peBOitVnLNFu7VmwP+HiDaaw4OUCv396eVCWhYiE="
	if err != nil || got != want {
		t.Fatalf("got %q err=%v", got, err)
	}
	a, _ := scramVerifier("pencil", nil)
	b, _ := scramVerifier("pencil", nil)
	if a == b || !strings.HasPrefix(a, "SCRAM-SHA-256$4096:") {
		t.Error("salt not random")
	}
}

// No SASLprep here: a non-ASCII password is refused, never sent in plain text.
func TestPostgresRefusesNonASCII(t *testing.T) {
	instantRetry(t)
	f := &proc.Fake{}
	f.On("psql", func(c proc.Cmd) bool { return c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
	f.On("psql", nil, proc.FakeReply{})
	c := Creds{AdminUser: "root", Current: curPw, New: "nouveau-mot-de-passé"}
	_, err := SetPassword(context.Background(), f, target("postgres", "root", true), []Tunnel{pgTun}, c)
	if err == nil || !strings.Contains(err.Error(), "non-ASCII") {
		t.Fatalf("err %v", err)
	}
	if len(f.Calls()) != 0 || err.Error() != "postgres: a password with non-ASCII characters is not supported" {
		t.Fatalf("calls=%d err=%v", len(f.Calls()), err)
	}
}

// redis-cli exits 0 when it cannot connect, when AUTH fails and on error
// replies; those are failures all the same.
func TestRedisExitZeroFailures(t *testing.T) {
	waits := instantRetry(t)
	f := &proc.Fake{}
	f.On("redis-cli", nil, proc.FakeReply{Stderr: "Could not connect to Redis at /tmp/x/s: Connection refused\n"})
	err := Login(context.Background(), f, target("redis", "app", false), &redisTun, curPw)
	if err == nil || !strings.Contains(err.Error(), "Could not connect") || *waits != 3 {
		t.Fatalf("err=%v waits=%d", err, *waits)
	}

	// A Redis without a password accepts any: the login proves nothing.
	f = &proc.Fake{}
	f.On("redis-cli", nil, proc.FakeReply{Stdout: "PONG\n"})
	err = Login(context.Background(), f, target("redis", "default", true), &redisTun, curPw)
	if err == nil || !strings.Contains(err.Error(), "accepts any password") {
		t.Fatalf("err %v", err)
	}

	f = &proc.Fake{}
	f.On("redis-cli", nil, proc.FakeReply{Stderr: "Error: odd failure\n"})
	_, err = SetPassword(context.Background(), f, target("redis", "default", true), []Tunnel{redisTun}, Creds{AdminUser: "default", Current: curPw, New: newPw})
	if err == nil || !strings.Contains(err.Error(), "Error: odd failure") {
		t.Fatalf("err %v", err)
	}

	f = &proc.Fake{}
	f.On("redis-cli", nil, proc.FakeReply{Stderr: "something on stderr only\n"})
	_, err = SetPassword(context.Background(), f, target("redis", "default", true), []Tunnel{redisTun}, Creds{AdminUser: "default", Current: curPw, New: newPw})
	if err == nil || !strings.Contains(err.Error(), "something on stderr only") {
		t.Fatalf("err %v", err)
	}
}

// The "may already have the new password" note is only for the change itself.
func TestIncompleteNoteOnlyForTheChange(t *testing.T) {
	instantRetry(t)
	wd := fmt.Errorf("run x: %w", exec.ErrWaitDelay)
	f := &proc.Fake{}
	f.On("redis-cli", func(c proc.Cmd) bool { return strings.HasPrefix(c.Stdin, "ACL GETUSER") }, proc.FakeReply{Err: wd})
	_, err := SetPassword(context.Background(), f, target("redis", "app", false), []Tunnel{redisTun}, Creds{AdminUser: "default", Admin: adminPw, New: newPw})
	if !errors.Is(err, ErrIncomplete) || strings.Contains(err.Error(), "new password") {
		t.Fatalf("err %v", err)
	}
	f = &proc.Fake{}
	f.On("mongosh", nil, proc.FakeReply{Err: wd})
	_, err = Check(context.Background(), f, target("mongo", "root", true), mongoTun, rootCreds())
	if !errors.Is(err, ErrIncomplete) || strings.Contains(err.Error(), "new password") {
		t.Fatalf("err %v", err)
	}
	f = &proc.Fake{}
	f.On("mongosh", nil, proc.FakeReply{Err: wd})
	_, err = SetPassword(context.Background(), f, target("mongo", "root", true), mongoTun, rootCreds())
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "may already have the new password") {
		t.Fatalf("err %v", err)
	}
}

func TestMongoCheckHasNoNewPassword(t *testing.T) {
	instantRetry(t)
	f := &proc.Fake{}
	f.On("mongosh", nil, proc.FakeReply{Stdout: "OK\n"})
	if _, err := Check(context.Background(), f, target("mongo", "root", true), mongoTun, rootCreds()); err != nil {
		t.Fatal(err)
	}
	if _, ok := envOf(f.Calls()[0], "RM_NEW_PASSWORD"); ok {
		t.Error("new password given to a dry run")
	}
}

// Our own timeout on the password-changing call is no transient: the change
// may have reached the database, so it is sent once and says so (M3). A
// timeout on a login is still retried.
func TestChangeTimeoutNotRetried(t *testing.T) {
	waits := instantRetry(t)
	for _, kind := range []string{"mongo", "postgres", "redis"} {
		t.Run(kind, func(t *testing.T) {
			changing := map[string]func(proc.Cmd) bool{
				"mongo":    func(c proc.Cmd) bool { _, ok := envOf(c, "RM_NEW_PASSWORD"); return ok },
				"postgres": func(c proc.Cmd) bool { return strings.HasPrefix(c.Stdin, "ALTER USER") },
				"redis":    func(c proc.Cmd) bool { return strings.HasPrefix(c.Stdin, "CONFIG SET") },
			}[kind]
			f := &proc.Fake{}
			f.On("psql", func(c proc.Cmd) bool { return len(c.Args) > 0 && c.Args[0] == "--version" }, proc.FakeReply{Stdout: "psql (PostgreSQL) 16.2\n"})
			f.On(clientOf[kind], changing, proc.FakeReply{Err: context.DeadlineExceeded})
			user := map[string]string{"mongo": "root", "postgres": "root", "redis": "default"}[kind]
			tuns := map[string][]Tunnel{"mongo": mongoTun[:1], "postgres": {pgTun}, "redis": {redisTun}}[kind]
			*waits = 0
			_, err := SetPassword(context.Background(), f, target(kind, user, true), tuns, rootCreds())
			if err == nil || !strings.Contains(err.Error(), "timed out after 60 s") || !strings.Contains(err.Error(), "may already have the new password") {
				t.Fatalf("err %v", err)
			}
			n := 0
			for _, c := range f.Calls() {
				if changing(c) {
					n++
				}
			}
			if n != 1 || *waits != 0 {
				t.Errorf("change sent %d times after %d retry waits, want once", n, *waits)
			}
			noSecretInErr(t, err)
		})
	}
	f := &proc.Fake{}
	n := 0
	f.On("redis-cli", func(proc.Cmd) bool { n++; return n == 2 }, proc.FakeReply{Err: context.DeadlineExceeded})
	f.On("redis-cli", pwIs(curPw, nil), proc.FakeReply{Stdout: "PONG\n"})
	f.On("redis-cli", nil, redisReject)
	*waits = 0
	if err := Login(context.Background(), f, target("redis", "app", false), &redisTun, curPw); err != nil || *waits != 1 {
		t.Errorf("login timeout: err=%v waits=%d, want one retry", err, *waits)
	}
}

// Mongo's change waits for a majority of members (M5); when that wait times
// out the primary has the new password but its outcome is unknown: nothing
// else is tried and the error says the database may already have it.
func TestMongoChangeMajority(t *testing.T) {
	instantRetry(t)
	if !strings.Contains(mongoChangeScript, "{ w: 'majority', wtimeout: 10000 }") {
		t.Error("the change does not wait for a majority")
	}
	f := &proc.Fake{}
	f.On("mongosh", nil, proc.FakeReply{Stdout: "WC_TIMEOUT\n"})
	_, err := SetPassword(context.Background(), f, target("mongo", "root", true), mongoTun, rootCreds())
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "may already have the new password") || len(f.Calls()) != 1 {
		t.Fatalf("err %v after %d calls", err, len(f.Calls()))
	}
	noSecretInErr(t, err)
}

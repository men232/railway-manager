package rotation

import (
	"reflect"
	"testing"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

func TestDetectDB(t *testing.T) {
	s := newStore(
		rec("smongo", "MONGO_INITDB_ROOT_PASSWORD", "mongo-root-password-0123456789"),
		rec("smongo", "MONGO_INITDB_ROOT_USERNAME", "mongo"),
		rec("smongo", "RAILWAY_PRIVATE_DOMAIN", "mongo.railway.internal"),
		rec("smongo", "RAILWAY_TCP_PROXY_DOMAIN", "a.proxy.rlwy.net"),
		rec("smongo", "RAILWAY_TCP_PROXY_PORT", "11111"),
		rec("smongo2", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"),
		rec("smongo2", "REPLICA_SET_NAME", "rs0"),
		rec("smongo2", "KEYFILE", "${{mongo.KEYFILE}}"),
		rec("smongo2", "RAILWAY_PRIVATE_DOMAIN", "mongo-2.railway.internal"),
		rec("sweb", "REPLICA_SET_NAME", "rs0"),
		rec("sapi", "MONGO_URL", "${{mongo.MONGO_URL}}"),
		// An app reading the root password by reference is no member.
		rec("sapi", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"),
	)
	a := Analyze(s, prodOnly())
	if len(a.DBs) != 1 {
		t.Fatalf("DBs = %d, want 1", len(a.DBs))
	}
	owner := domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "smongo"}
	member := domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "smongo2"}
	want := DB{
		Key: "e1/smongo", Kind: "mongo", RootUser: "mongo", Port: 27017,
		Owner: owner, Members: []domain.Place{member},
		Hosts: []Host{
			{Addr: "a.proxy.rlwy.net:11111", EnvID: "e1", Proxy: true},
			{Addr: "mongo.railway.internal:27017", EnvID: "e1"},
			{Addr: "mongo-2.railway.internal:27017", EnvID: "e1"},
		},
	}
	if !reflect.DeepEqual(a.DBs[0], want) {
		t.Errorf("DB = %+v\nwant %+v", a.DBs[0], want)
	}

	for _, c := range []struct {
		service string
		want    bool
	}{{"smongo", true}, {"smongo2", true}, {"sweb", true}, {"sapi", false}, {"spg", false}} {
		p := domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: c.service}
		if got := a.IsDBPlace(p); got != c.want {
			t.Errorf("IsDBPlace(%s) = %v, want %v", c.service, got, c.want)
		}
	}

	g := oneGroup(t, a, "MONGO_INITDB_ROOT_PASSWORD")
	if g.Class != ClassDB || g.DB == nil || g.DB.Key != "e1/smongo" || len(g.Locs) != 1 {
		t.Errorf("root password group: Class=%v DB=%v locs=%d", g.Class, g.DB, len(g.Locs))
	}
}

func TestDetectDBRootUserFallbacks(t *testing.T) {
	s := newStore(
		rec("spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
		rec("spg", "PGUSER", "pgadmin"),
		rec("spg", "PGPORT", "6543"),
		rec("spg", "RAILWAY_PRIVATE_DOMAIN", "pg.railway.internal"),
	)
	a := Analyze(s, prodOnly())
	if len(a.DBs) != 1 {
		t.Fatalf("DBs = %d, want 1", len(a.DBs))
	}
	db := a.DBs[0]
	if db.Kind != "postgres" || db.RootUser != "pgadmin" || db.Port != 6543 || !reflect.DeepEqual(db.Hosts, []Host{{Addr: "pg.railway.internal:6543", EnvID: "e1"}}) {
		t.Errorf("DB = %+v", db)
	}
}

// Railway's system values (private domain, TCP proxy) arrive only as store
// system values, never as variables: the hosts come from there, so a literal
// credential URL naming the private host is the database's URL.
func TestDBHostsFromSystemValues(t *testing.T) {
	s := store.New()
	s.ReplaceCatalog(fixtureCatalog())
	s.ReplaceVariables("p1",
		[]railway.VarRecord{
			rec("spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
			rec("spg", "POSTGRES_USER", "postgres"),
			rec("sapi", "DATABASE_URL", "postgres://app:apppw@pg.railway.internal:5432/x"),
		},
		railway.SystemRecord{EnvID: "e1", ServiceID: "spg", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "pg.railway.internal"},
		railway.SystemRecord{EnvID: "e1", ServiceID: "spg", Name: "RAILWAY_TCP_PROXY_DOMAIN", Value: "pg.proxy.rlwy.net"},
		railway.SystemRecord{EnvID: "e1", ServiceID: "spg", Name: "RAILWAY_TCP_PROXY_PORT", Value: "23456"},
	)
	a := Analyze(s, prodOnly())
	if len(a.DBs) != 1 {
		t.Fatalf("DBs = %d, want 1", len(a.DBs))
	}
	if want := []Host{{Addr: "pg.proxy.rlwy.net:23456", EnvID: "e1", Proxy: true}, {Addr: "pg.railway.internal:5432", EnvID: "e1"}}; !reflect.DeepEqual(a.DBs[0].Hosts, want) {
		t.Errorf("Hosts = %v, want %v", a.DBs[0].Hosts, want)
	}
	g := oneGroup(t, a, "DATABASE_URL")
	if g.Class != ClassURL || g.DB == nil || g.DB.Owner.ServiceID != "spg" {
		t.Errorf("DATABASE_URL: Class = %v DB = %v, want URL on spg", g.Class, g.DB)
	}
}

// Postgres and Redis have no members: an app with a same-name reference to
// the root password is an ordinary place.
func TestDetectDBNoMembersForApps(t *testing.T) {
	s := newStore(
		rec("spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
		rec("spg", "POSTGRES_USER", "postgres"),
		rec("sapi", "POSTGRES_PASSWORD", "${{pg.POSTGRES_PASSWORD}}"),
		rec("sapi", "REPLICA_SET_NAME", "rs0"),
	)
	a := Analyze(s, prodOnly())
	if len(a.DBs) != 1 || len(a.DBs[0].Members) != 0 {
		t.Fatalf("DBs = %+v, want postgres without members", a.DBs)
	}
}

// Neither the template's init helper (the root password under another
// name, REPLICA_SET_NAME by reference, no KEYFILE) nor an app copying the
// root password and REPLICA_SET_NAME without a KEYFILE is a member.
func TestDetectDBMongoMembersNeedKeyFile(t *testing.T) {
	s := newStore(
		rec("smongo", "MONGO_INITDB_ROOT_PASSWORD", "mongo-root-password-0123456789"),
		rec("smongo", "MONGO_INITDB_ROOT_USERNAME", "mongo"),
		rec("smongo", "REPLICA_SET_NAME", "rs0"),
		rec("smongo", "KEYFILE", "keyfile-value-0123456789abcdef"),
		rec("sweb", "MONGOPASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"),
		rec("sweb", "REPLICA_SET_NAME", "${{mongo.REPLICA_SET_NAME}}"),
		rec("sapi", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"),
		rec("sapi", "REPLICA_SET_NAME", "${{mongo.REPLICA_SET_NAME}}"),
	)
	a := Analyze(s, prodOnly())
	if len(a.DBs) != 1 || len(a.DBs[0].Members) != 0 {
		t.Fatalf("DBs = %+v, want mongo without members", a.DBs)
	}
}

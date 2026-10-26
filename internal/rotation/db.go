package rotation

import (
	"strconv"
	"strings"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

// DB is a database detected from its root-password variable. It never holds
// the password.
type DB struct {
	// Key is the owner's place key.
	Key string
	// Kind is "mongo", "postgres" or "redis".
	Kind     string
	RootUser string
	// Port is the database's port inside its container.
	Port  int
	Owner domain.Place
	// Members are the other places referencing the owner's root password.
	Members []domain.Place
	// Hosts identify the database in connection strings, used only to
	// recognise URLs.
	Hosts []Host
}

// Host is one address of a database: "host:port" with where it is reachable
// from. A private address is reachable only inside its environment, and the
// same private domain repeats across environments; a TCP proxy address is
// public and unique.
type Host struct {
	Addr  string
	EnvID string
	Proxy bool
}

// matches reports whether a URL host, read in a variable of environment
// envID, names this address.
func (h Host) matches(addr, envID string) bool {
	return strings.EqualFold(h.Addr, addr) && (h.Proxy || h.EnvID == envID)
}

type dbKind struct {
	kind, rootVar string
	port          int
	portVar       string // in-container port variable, "" for none
}

// dbKinds in detection order: only the first matching kind counts.
var dbKinds = []dbKind{
	{kind: "mongo", rootVar: "MONGO_INITDB_ROOT_PASSWORD", port: 27017},
	{kind: "postgres", rootVar: "POSTGRES_PASSWORD", port: 5432, portVar: "PGPORT"},
	{kind: "redis", rootVar: "REDIS_PASSWORD", port: 6379, portVar: "REDISPORT"},
}

// RootVar is the root-password variable of a database kind ("mongo",
// "postgres", "redis"), "" for any other.
func RootVar(kind string) string { return rootVar(kind) }

func rootVar(kind string) string {
	for _, k := range dbKinds {
		if k.kind == kind {
			return k.rootVar
		}
	}
	return ""
}

// rendered maps a place's variable names to their rendered values.
func rendered(s *store.Store, p domain.Place) map[string]string {
	out := map[string]string{}
	for _, v := range s.VariablesAt(p) {
		out[v.Name] = v.Rendered
	}
	return out
}

func firstSet(vars map[string]string, names ...string) string {
	for _, n := range names {
		if v := vars[n]; v != "" {
			return v
		}
	}
	return ""
}

// detectDBs finds the databases among places and the keys of every place
// that is a database (owner, member, or REPLICA_SET_NAME).
func detectDBs(s *store.Store, places []domain.Place) ([]DB, map[string]bool) {
	isDB := map[string]bool{}
	var dbs []DB
	for _, p := range places {
		if s.Variable(domain.LocationID(p.EnvID, p.ServiceID, "REPLICA_SET_NAME")) != nil {
			isDB[p.Key()] = true
		}
		if p.Shared() {
			continue
		}
		for _, k := range dbKinds {
			v := s.Variable(domain.LocationID(p.EnvID, p.ServiceID, k.rootVar))
			if v == nil || !v.Literal {
				continue
			}
			dbs = append(dbs, newDB(s, places, p, k, v.ID))
			break
		}
	}
	for _, db := range dbs {
		isDB[db.Owner.Key()] = true
		for _, m := range db.Members {
			isDB[m.Key()] = true
		}
	}
	return dbs, isDB
}

func newDB(s *store.Store, places []domain.Place, owner domain.Place, k dbKind, rootLoc string) DB {
	db := DB{Key: owner.Key(), Kind: k.kind, Owner: owner}
	for _, p := range places {
		// A member is a replica node of the database: a Mongo place whose
		// root password references the owner's and that carries
		// REPLICA_SET_NAME and a KEYFILE (literal or reference): replica
		// nodes need the keyfile; init helpers and apps copying the
		// password do not. Postgres and Redis have no members.
		has := func(name string) bool { return s.Variable(domain.LocationID(p.EnvID, p.ServiceID, name)) != nil }
		if k.kind != "mongo" || p.EnvID != owner.EnvID || p.Shared() || p == owner || !has("REPLICA_SET_NAME") || !has("KEYFILE") {
			continue
		}
		v := s.Variable(domain.LocationID(p.EnvID, p.ServiceID, k.rootVar))
		if v == nil || v.Literal {
			continue
		}
		for _, r := range domain.ParseReferences(v.Unrendered) {
			if s.ResolveReference(p, r) == rootLoc {
				db.Members = append(db.Members, p)
				break
			}
		}
	}

	vars := rendered(s, owner)
	switch k.kind {
	case "mongo":
		db.RootUser = vars["MONGO_INITDB_ROOT_USERNAME"]
	case "postgres":
		db.RootUser = firstSet(vars, "POSTGRES_USER", "PGUSER")
	case "redis":
		db.RootUser = firstSet(vars, "REDIS_USER", "REDISUSER")
		if db.RootUser == "" {
			db.RootUser = "default"
		}
	}
	port := k.port
	if k.portVar != "" {
		if n, err := strconv.Atoi(vars[k.portVar]); err == nil && n > 0 {
			port = n
		}
	}
	db.Port = port

	seen := map[Host]bool{}
	add := func(h Host) {
		if !seen[h] {
			seen[h] = true
			db.Hosts = append(db.Hosts, h)
		}
	}
	// Hosts come from Railway's system values: they are never variables.
	for _, m := range append([]domain.Place{owner}, db.Members...) {
		sys := func(name string) string { return s.SystemVar(m.EnvID, m.ServiceID, name) }
		if d, pp := sys("RAILWAY_TCP_PROXY_DOMAIN"), sys("RAILWAY_TCP_PROXY_PORT"); d != "" && pp != "" {
			add(Host{Addr: d + ":" + pp, EnvID: m.EnvID, Proxy: true})
		}
		if d := sys("RAILWAY_PRIVATE_DOMAIN"); d != "" {
			add(Host{Addr: d + ":" + strconv.Itoa(port), EnvID: m.EnvID})
		}
	}
	return db
}

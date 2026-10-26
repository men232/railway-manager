package rotation

import (
	"slices"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	s := newStore(
		// pg is a postgres database reachable at pg.railway.internal:5432.
		rec("spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
		rec("spg", "POSTGRES_USER", "postgres"),
		rec("spg", "RAILWAY_PRIVATE_DOMAIN", "pg.railway.internal"),
		rec("spg", "RAILWAY_TCP_PROXY_DOMAIN", "pg.proxy.rlwy.net"),
		rec("spg", "RAILWAY_TCP_PROXY_PORT", "23456"),

		rec("sapi", "ROOT_URL", "postgres://postgres:rootpw@pg.railway.internal:5432/x"),
		rec("sapi", "USER_URL", "postgres://u:p@pg.railway.internal:5432/x"),
		rec("sapi", "PROXY_URL", "postgres://u:q@pg.proxy.rlwy.net:23456/x"),
		rec("sapi", "JWT_SECRET", "jwt-secret-value-0123456789"),
		rec("sapi", "VITE_X", "vite-public-value-0123456789"),
		rec("sapi", "STRIPE_API_KEY", "sk_live_0123456789abcdefgh"),
		rec("sapi", "CACHE_URL", "redis://default:pw@cache.example.com:6379"),
		rec("sapi", "DB_PASSWORD", "db-password-value-0123456789"),
		rec("sapi", "PORT", "8080"),
		rec("sapi", "MY_TOKENISH", "internal-name-value-0123456789"),
	)
	o := prodOnly()
	o.Internal = []string{"MY_TOKENISH"}
	a := Analyze(s, o)

	cases := []struct {
		name   string
		class  Class
		db     bool
		public bool
		note   string
	}{
		{"POSTGRES_PASSWORD", ClassDB, true, false, "changes the password inside pg (Proj/prod)"},
		{"ROOT_URL", ClassURL, true, false, "root of pg (Proj/prod): rewritten if that password rotates"},
		{"USER_URL", ClassURL, true, false, "user 'u' on pg (Proj/prod): new password set in the DB"},
		{"PROXY_URL", ClassURL, true, false, "user 'u' on pg (Proj/prod): new password set in the DB"},
		{"JWT_SECRET", ClassGen, false, false, ""},
		{"VITE_X", ClassAsk, false, true, ""},
		{"STRIPE_API_KEY", ClassAsk, false, false, ""},
		{"CACHE_URL", ClassAsk, false, false, "credentials for cache.example.com:6379 (not a DB in the scanned projects)"},
		{"DB_PASSWORD", ClassGen, false, false, ""},
		{"PORT", ClassHidden, false, false, ""},
		{"MY_TOKENISH", ClassGen, false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := oneGroup(t, a, c.name)
			if g.Class != c.class {
				t.Errorf("Class = %v, want %v", g.Class, c.class)
			}
			if (g.DB != nil) != c.db {
				t.Errorf("DB set = %v, want %v", g.DB != nil, c.db)
			}
			if g.DB != nil && (g.DB.Kind != "postgres" || g.DB.Owner.ServiceID != "spg") {
				t.Errorf("DB = %s at %s, want postgres at spg", g.DB.Kind, g.DB.Owner.ServiceID)
			}
			if g.Public != c.public {
				t.Errorf("Public = %v, want %v", g.Public, c.public)
			}
			if g.Note != c.note {
				t.Errorf("Note = %q, want %q", g.Note, c.note)
			}
		})
	}
}

func TestWarningsAndOff(t *testing.T) {
	s := newStore(
		rec("sapi", "ENCRYPTION_SECRET_KEY", "encryption-key-value-0123456789"),
		rec("sapi", "KEYFILE", "keyfile-value-0123456789abcdef"),
		rec("sapi", "JWT_SECRET", "jwt-secret-value-0123456789"),
		rec("sapi", "EMAIL_LINK_SECRET", "email-link-value-0123456789"),
		rec("sapi", "OTHER_SECRET", "other-secret-value-0123456789"),
	)
	a := Analyze(s, prodOnly())
	cases := []struct {
		name, warning string
		off           bool
	}{
		{"ENCRYPTION_SECRET_KEY", "data encrypted with the old key becomes unreadable; migrate first", true},
		{"KEYFILE", "restarts every replica-set member at once; apply waits until the database is primary again", true},
		{"JWT_SECRET", "logs every user out", false},
		{"EMAIL_LINK_SECRET", "links in already-sent emails stop working", false},
		{"OTHER_SECRET", "", false},
	}
	for _, c := range cases {
		g := oneGroup(t, a, c.name)
		if g.Warning != c.warning || g.OffByDefault != c.off {
			t.Errorf("%s: Warning=%q OffByDefault=%v, want %q %v", c.name, g.Warning, g.OffByDefault, c.warning, c.off)
		}
		if g.Class != ClassGen && c.name != "OTHER_SECRET" {
			t.Errorf("%s: Class = %v, want GEN", c.name, g.Class)
		}
	}
}

func TestSharedRootProblem(t *testing.T) {
	v := "shared-root-password-0123456789"
	s := newStore(
		rec("smongo", "MONGO_INITDB_ROOT_PASSWORD", v),
		rec("spg", "POSTGRES_PASSWORD", v),
	)
	g := oneGroup(t, Analyze(s, prodOnly()), "POSTGRES_PASSWORD")
	if g.Class != ClassDB || g.DB == nil {
		t.Fatalf("Class = %v DB = %v, want DB", g.Class, g.DB)
	}
	if g.Problem != "2 databases share this password: rotate them one at a time" {
		t.Errorf("Problem = %q", g.Problem)
	}
}

func TestParseCredURL(t *testing.T) {
	u, ok := parseCredURL("mongodb+srv://user:pw@a.example.com:1,b.example.com:2/db?x=1")
	if !ok || u.scheme != "mongodb+srv" || u.user != "user" || strings.Join(u.hosts, " ") != "a.example.com:1 b.example.com:2" {
		t.Errorf("parseCredURL = %+v %v", u, ok)
	}
	for _, v := range []string{"https://example.com/x", "postgres://:pw@h/x", "postgres://u:@h/x", "Postgres://u:p@h/x", "plain"} {
		if _, ok := parseCredURL(v); ok {
			t.Errorf("parseCredURL(%q) ok, want not a credential URL", v)
		}
	}
}

// A password holding "@" runs to the last "@" of the authority: the host never
// carries a piece of it, so no note shows part of a password.
func TestParseCredURLAtInPassword(t *testing.T) {
	for i, v := range []string{
		"redis://default:S3cr@tPart@redis.example.com:6379",
		"redis://default:a@bFrag@cFrag@redis.example.com:6379/0",
	} {
		u, ok := parseCredURL(v)
		if host := strings.Join(u.hosts, ","); !ok || u.user != "default" || host != "redis.example.com:6379" {
			t.Errorf("case %d: ok=%v user=%q, host is not redis.example.com:6379 (len %d)", i, ok, u.user, len(host))
		}
	}

	s := newStore(
		rec("spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
		rec("spg", "POSTGRES_USER", "postgres"),
		rec("spg", "RAILWAY_PRIVATE_DOMAIN", "pg.railway.internal"),
		rec("sapi", "CACHE_URL", "redis://default:S3cr@tPart@redis.example.com:6379"),
		rec("sapi", "TWO_AT_URL", "redis://default:a@bFrag@cFrag@redis.example.com:6379/0"),
		rec("sapi", "PG_AT_URL", "postgres://u:p@ss@pg.railway.internal:5432/x"),
	)
	a := Analyze(s, prodOnly())
	for _, name := range []string{"CACHE_URL", "TWO_AT_URL"} {
		g := oneGroup(t, a, name)
		if want := "credentials for redis.example.com:6379 (not a DB in the scanned projects)"; g.Note != want {
			t.Errorf("%s: Note is not %q", name, want)
		}
		if strings.Contains(g.Note, "Part") || strings.Contains(g.Note, "Frag") || strings.Contains(g.Note, "@") {
			t.Errorf("%s: Note holds a password fragment", name)
		}
	}
	g := oneGroup(t, a, "PG_AT_URL")
	if g.Class != ClassURL || g.DB == nil || g.DB.Owner.ServiceID != "spg" {
		t.Errorf("PG_AT_URL: Class = %v DB = %v, want URL on spg", g.Class, g.DB)
	}
}

// Passwords may hold "?", "#" and "@"; the host is whole and ends the
// authority, so a malformed URL never yields a password piece as a host.
func TestParseCredURLPasswordChars(t *testing.T) {
	for i, v := range []string{
		"postgres://u:pa?ss@pg.railway.internal:5432/x",
		"postgres://u:pa#ss@pg.railway.internal:5432/x",
		"postgres://u:p?a#ss@pg.railway.internal:5432?sslmode=disable",
		"postgres://u:pa@ss@pg.railway.internal:5432#frag",
		"postgres://u:p@a@ss@pg.railway.internal:5432/x",
	} {
		u, ok := parseCredURL(v)
		if host := strings.Join(u.hosts, ","); !ok || u.unclear || u.user != "u" || host != "pg.railway.internal:5432" {
			t.Errorf("case %d: ok=%v unclear=%v user=%q, host is not pg.railway.internal:5432 (len %d)", i, ok, u.unclear, u.user, len(host))
		}
	}
	// The shortest and longest password readings differ: unclear, and the
	// right host is among the candidates. An "@" in the query or fragment,
	// or an "@" then "?" in the password.
	for i, c := range []struct{ v, host string }{
		{"postgres://u:pw@pg.internal:5432?sslmode=disable&email=a@b.com", "pg.internal:5432"},
		{"mongodb+srv://u:pw@c.mongodb.net?appName=x@y", "c.mongodb.net"},
		{"postgres://u:pw@pg.internal:5432#a@evil", "pg.internal:5432"},
		{"postgres://u:p@a?s@s#@pg.railway.internal:5432", "pg.railway.internal:5432"},
	} {
		u, ok := parseCredURL(c.v)
		if !ok || !u.unclear || u.user != "u" || !slices.Contains(u.hosts, c.host) {
			t.Errorf("unclear case %d: ok=%v unclear=%v, %q not among %d candidates", i, ok, u.unclear, c.host, len(u.hosts))
		}
	}
	for i, v := range []string{"redis://u:a@b@/x", "redis://u:a@b@", "redis://u:a@b c"} {
		if u, ok := parseCredURL(v); ok {
			t.Errorf("case %d: parsed with %d hosts, want not a credential URL", i, len(u.hosts))
		}
	}

	const ambiguous = "postgres://u:p@a?s@s#@pg.railway.internal:5432"
	s := newStore(
		rec("spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
		rec("spg", "RAILWAY_PRIVATE_DOMAIN", "pg.railway.internal"),
		rec("sapi", "DB_SECRET_URL", "postgres://u:pa?ss#@pg.railway.internal:5432/x"),
		rec("sapi", "AMBIGUOUS_URL", ambiguous),
	)
	a := Analyze(s, prodOnly())
	for _, name := range []string{"DB_SECRET_URL", "AMBIGUOUS_URL"} {
		g := oneGroup(t, a, name)
		if g.Class != ClassURL || g.DB == nil || g.DB.Owner.ServiceID != "spg" {
			t.Errorf("%s: Class = %v DB = %v, want URL on spg", name, g.Class, g.DB)
		}
	}

	// No database: the note never prints an uncertain host.
	s = newStore(
		rec("sapi", "AMBIGUOUS_URL", ambiguous),
		rec("sapi", "QUERY_AT_URL", "postgres://u:pw@pg.internal:5432#a@evil"),
	)
	a = Analyze(s, prodOnly())
	for _, name := range []string{"AMBIGUOUS_URL", "QUERY_AT_URL"} {
		g := oneGroup(t, a, name)
		if want := "credentials for an unclear host (not a DB in the scanned projects)"; g.Class != ClassAsk || g.Note != want {
			t.Errorf("%s: Class = %v, Note is not %q (len %d)", name, g.Class, want, len(g.Note))
		}
	}
}

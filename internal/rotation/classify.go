package rotation

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/men232/railway-manager/internal/store"
)

// builtinInternal are the internal names, always GEN.
var builtinInternal = []string{
	"ADMIN_TOKEN", "MASTER_PASSWORD", "JWT_SECRET", "APP_SECRET", "EMAIL_LINK_SECRET",
	"ENCRYPTION_SECRET_KEY", "KEYFILE", "QDRANT__SERVICE__API_KEY",
}

// warnings per name (in the app's lower-case style).
var warnings = map[string]string{
	"ENCRYPTION_SECRET_KEY": "data encrypted with the old key becomes unreadable; migrate first",
	"KEYFILE":               "restarts every replica-set member at once; apply waits until the database is primary again",
	"JWT_SECRET":            "logs every user out",
	"APP_SECRET":            "logs every user out",
	"EMAIL_LINK_SECRET":     "links in already-sent emails stop working",
}

var offByDefault = map[string]bool{"ENCRYPTION_SECRET_KEY": true, "KEYFILE": true}

func isPublic(n string) bool {
	return strings.HasPrefix(n, "NUXT_PUBLIC_") || strings.HasPrefix(n, "VITE_") ||
		hasAnySuffix(n, "CLIENT_ID", "_SID", "POSTHOG_KEY", "_DSN")
}

func isExternal(n string) bool {
	return containsAny(n, "WEBHOOK", "SIGNING_SECRET", "API_KEY") ||
		hasAnySuffix(n, "_TOKEN", "_KEY", "_SECRET") ||
		strings.HasPrefix(n, "TOKEN") ||
		containsAny(n, "SERVICE_ACCOUNT", "SA_JSON", "LICENSE")
}

func isInternalPattern(n string) bool {
	return containsAny(n, "SECRET", "PASSWORD") || strings.HasSuffix(n, "_SALT")
}

// looksSecret decides whether a sealed variable is kept.
func looksSecret(n string, internal map[string]bool) bool {
	return internal[n] || isExternal(n) || isInternalPattern(n)
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

func hasAnySuffix(s string, sufs ...string) bool {
	for _, x := range sufs {
		if strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}

// credURL is a parsed credential URL. The password is never kept.
type credURL struct {
	scheme, user string
	// hosts are the URL's hosts; when unclear, the candidates of both
	// readings, used only to match a database and never shown.
	hosts []string
	// unclear: the shortest and the longest password reading give
	// different hosts, so one of them may be a piece of the password.
	unclear bool
	// authDB is the database a mongodb URL authenticates against
	// (mongoAuthDB); "" for any other scheme.
	authDB string
}

// credURLLazy and credURLGreedy:
// scheme://user:password@host[,host…][/?#rest], scheme in lower-case
// letters and "+", user and password non-empty. The password may hold "@",
// "?" and "#"; hosts hold no "@" and end the authority with "/", "?", "#" or
// the end. Lazy takes the shortest password that leaves such a host (an "@"
// in the query or fragment never makes a host), greedy the longest (an "@"
// then "?" in the password stays in it). The password is never captured.
var (
	credURLLazy   = regexp.MustCompile(`^([a-z+]+)://([^:@/\s]+):[^/\s]+?@([^@/?#\s]+)(?:[/?#]|$)`)
	credURLGreedy = regexp.MustCompile(`^([a-z+]+)://([^:@/\s]+):[^/\s]+@([^@/?#\s]+)(?:[/?#]|$)`)
)

// parseCredURL reads value with both patterns: not a URL when both fail,
// as read when they agree, unclear (hosts from both) when they differ.
func parseCredURL(value string) (credURL, bool) {
	l := credURLLazy.FindStringSubmatch(value)
	g := credURLGreedy.FindStringSubmatch(value)
	switch {
	case l == nil && g == nil:
		return credURL{}, false
	case l == nil:
		l = g
	case g == nil:
		g = l
	}
	u := credURL{scheme: l[1], user: l[2], hosts: strings.Split(l[3], ",")}
	if g[3] != l[3] {
		u.unclear = true
		u.hosts = append(u.hosts, strings.Split(g[3], ",")...)
	}
	if strings.HasPrefix(u.scheme, "mongodb") {
		u.authDB = mongoAuthDB(value[len(strings.TrimRight(l[0], "/?#")):])
	}
	return u, true
}

// mongoAuthDB is the database a Mongo URL authenticates against, from what
// follows its hosts: the authSource option (named in any case, as the
// driver reads it), else the database of the path, else admin.
func mongoAuthDB(rest string) string {
	rest, _, _ = strings.Cut(rest, "#")
	path, query, _ := strings.Cut(rest, "?")
	if q, err := url.ParseQuery(query); err == nil {
		for k, v := range q {
			if strings.EqualFold(k, "authSource") && len(v) > 0 && v[0] != "" {
				return v[0]
			}
		}
	}
	if db := strings.TrimPrefix(path, "/"); db != "" {
		if d, err := url.PathUnescape(db); err == nil {
			return d
		}
		return db
	}
	return "admin"
}

type classifier struct {
	s        *store.Store
	dbs      []DB
	internal map[string]bool
}

// dbLabel names a database in notes: "<service> (<project/env>)".
func (c classifier) dbLabel(db DB) string {
	return c.s.ServiceName(db.Owner.ServiceID) + " (" + c.s.EnvLabel(db.Owner.EnvID) + ")"
}

// classify sets g's class, note and warning from its names and value. value
// is the old value ("" when sealed); roots are the indexes of the DBs whose
// root password g holds.
func (c classifier) classify(g *Group, value string, roots []int) {
	anyName := func(f func(string) bool) bool {
		for _, n := range g.Names {
			if f(n) {
				return true
			}
		}
		return false
	}
	url, isURL := parseCredURL(value)
	var urlDB *DB
	if isURL {
		urlDB = c.urlDB(url, g.Locs)
	}

	switch {
	case len(roots) > 0:
		db := c.dbs[roots[0]]
		g.Class, g.DB = ClassDB, &db
		g.Note = "changes the password inside " + c.dbLabel(db)
		if len(roots) > 1 {
			g.Problem = strconv.Itoa(len(roots)) + " databases share this password: rotate them one at a time"
		}
	case urlDB != nil:
		db := *urlDB
		g.Class, g.DB = ClassURL, &db
		if url.user == db.RootUser {
			g.Note = "root of " + c.dbLabel(db) + ": rewritten if that password rotates"
		} else {
			g.Note = "user '" + url.user + "' on " + c.dbLabel(db) + ": new password set in the DB"
		}
	case anyName(func(n string) bool { return c.internal[n] }):
		g.Class = ClassGen
	case anyName(isPublic):
		g.Class, g.Public = ClassAsk, true
	case isURL || anyName(isExternal):
		g.Class = ClassAsk
		if isURL {
			host := strings.Join(url.hosts, ",")
			if url.unclear {
				// One reading's host may be a piece of the password.
				host = "an unclear host"
			}
			g.Note = "credentials for " + host + " (not a DB in the scanned projects)"
		}
	case anyName(isInternalPattern):
		g.Class = ClassGen
	default:
		g.Class = ClassHidden
	}

	for _, n := range g.Names {
		if w, ok := warnings[n]; ok && g.Warning == "" {
			g.Warning = w
		}
		if offByDefault[n] {
			g.OffByDefault = true
		}
	}
}

// urlDB is the first database the URL's hosts name from the environment
// of one of locs (matchDBs), or nil.
func (c classifier) urlDB(u credURL, locs []Loc) *DB {
	for _, l := range locs {
		if ms := matchDBs(c.dbs, u, l.Place.EnvID); len(ms) > 0 {
			return &c.dbs[ms[0].db]
		}
	}
	return nil
}

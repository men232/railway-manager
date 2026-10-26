package rotation

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

// URLMode is what happens to a URL place when its password rotates.
type URLMode int

const (
	// URLRewrite: a literal URL on this database with this row's user; its
	// password is rewritten.
	URLRewrite URLMode = iota
	// URLFollows: a reference resolving to a rewritten URL or to the
	// password variable; it follows by itself.
	URLFollows
	// URLOther: a URL on this database with another user; display only.
	URLOther
	// URLUnrecognised: a place carrying this row's current password that is
	// not known to follow it: a URL whose host names no known database, or
	// a literal copy of the password outside the database's environment or
	// shorter than 20 characters. Never rewritten: it needs a decision (p f
	// c x) or k before the password runs.
	URLUnrecognised
	// URLOtherDB: a literal with this row's password at another database's
	// place or own password variable; display only, never counted.
	URLOtherDB
)

func (m URLMode) String() string {
	switch m {
	case URLRewrite:
		return "rewrite"
	case URLFollows:
		return "follows"
	case URLOther:
		return "other"
	case URLUnrecognised:
		return "unrecognised"
	case URLOtherDB:
		return "otherdb"
	default:
		return "URLMode(" + strconv.Itoa(int(m)) + ")"
	}
}

// URLPlace is one variable that carries a password, in any environment.
type URLPlace struct {
	Loc  Loc
	Mode URLMode
	// ViaProxy: the URL reaches the database through its public TCP proxy.
	ViaProxy bool
	// Unclear: the URL cannot be rewritten safely — its password's end is
	// unclear (RewriteURL refuses it) or its hosts name more than one
	// database. It needs a decision by hand.
	Unclear bool
	// Copy: a literal copy of the password (not a URL), set to the new
	// password itself; in Copies.
	Copy bool
}

// KeyFileUser is the user part of a Mongo KEYFILE row's group key; "@"
// never starts a URL user.
const KeyFileUser = "@keyfile"

// unknownRootUser is the user part of a root row whose root user is unknown.
const unknownRootUser = "@root"

// PasswordRow is one database password — a root, a user seen in URLs, or a
// Mongo KEYFILE — with every place in every environment that carries it.
// It never holds a value.
type PasswordRow struct {
	// Key is the change group: "rotate-db:<db key>:<user>".
	Key string
	DB  DB
	// User is the database user; "" for a root whose user is unknown (the
	// row then has a Problem) and KeyFileUser for a KEYFILE row.
	User string
	Root bool
	// KeyFile: the Mongo KEYFILE of DB.
	KeyFile bool
	// Own is the database's own variable (root password or the owner's
	// KEYFILE); nil for a user row.
	Own *Loc
	// URLs are the places that follow this password: rewritten, by
	// reference, or unrecognised (display only). Others are the same
	// database's URLs of other users, shown only.
	URLs, Others []URLPlace
	// Copies are a KEYFILE row's members' KEYFILE variables: literal copies
	// (URLRewrite, written with the owner) and references (URLFollows); for
	// a password, its literal copies in the owner's environment (URLRewrite,
	// Copy) and the references to them (URLFollows).
	Copies           []URLPlace
	Warning, Problem string
	// AuthDB is a Mongo row's database, the one its user belongs to and its
	// URLs authenticate against ("admin" for the root); "" for other kinds.
	AuthDB string
}

// PasswordRows lists one row per database root password, per user seen in
// URLs on a database, and per Mongo database with a literal KEYFILE. Places
// come from every environment read (o.AllEnvIDs); a row is listed when its
// database or any of its rewritten or following URLs is in o.EnvIDs. Rows
// follow a.DBs; per database: root, users by name, KEYFILE.
func PasswordRows(s *store.Store, a Analysis, o Options) []PasswordRow {
	all := buildPasswordRows(s, a.DBs, selectedPlaces(s, readEnvIDs(o)), o.Since)
	out := make([]PasswordRow, 0, len(all))
	for _, r := range all {
		if r.inEnvs(o.EnvIDs) {
			out = append(out, r)
		}
	}
	return out
}

// readEnvIDs are the environments read for databases and URLs.
func readEnvIDs(o Options) []string {
	if len(o.AllEnvIDs) > 0 {
		return o.AllEnvIDs
	}
	return o.EnvIDs
}

func (r PasswordRow) inEnvs(envIDs []string) bool {
	if slices.Contains(envIDs, r.DB.Owner.EnvID) {
		return true
	}
	for _, u := range r.URLs {
		if u.Mode != URLUnrecognised && slices.Contains(envIDs, u.Loc.Place.EnvID) {
			return true
		}
	}
	return false
}

// dbURL is a literal credential URL on a known database.
type dbURL struct {
	db     int
	user   string
	authDB string // Mongo: the database the URL authenticates against
	place  URLPlace
}

// strayURL is a literal credential URL on no known database, with the
// hashes of its password readings.
type strayURL struct {
	loc    Loc
	hashes []string
}

func buildPasswordRows(s *store.Store, dbs []DB, places []domain.Place, since time.Time) []PasswordRow {
	order := make(map[string]int, len(places))
	for i, p := range places {
		order[p.Key()] = i
	}

	var urls []dbURL
	var strays []strayURL
	for _, p := range places {
		for _, v := range s.VariablesAt(p) {
			if strings.HasPrefix(v.Name, "RAILWAY_") || !v.Literal || v.Sealed {
				continue
			}
			u, ok := parseCredURL(v.Unrendered)
			if !ok {
				continue
			}
			ms := matchDBs(dbs, u, v.Place.EnvID)
			if len(ms) == 0 {
				strays = append(strays, strayURL{loc: locOf(v, since), hashes: passwordHashes(v.Unrendered)})
				continue
			}
			for _, m := range ms {
				urls = append(urls, dbURL{db: m.db, user: u.user, authDB: u.authDB, place: URLPlace{
					Loc: locOf(v, since), Mode: URLRewrite, ViaProxy: m.proxy,
					Unclear: u.unclear || len(ms) > 1 || !allHostsKnown(dbs, u, v.Place.EnvID),
				}})
			}
		}
	}

	rootFP := map[string]int{}
	dbVar := map[string]bool{}    // every database's own password variable
	dbPlaceOf := map[string]int{} // owner and member place key → database
	for i, db := range dbs {
		id := domain.LocationID(db.Owner.EnvID, db.Owner.ServiceID, rootVar(db.Kind))
		dbVar[id] = true
		if v := s.Variable(id); v != nil {
			rootFP[v.Fingerprint]++
		}
		for _, p := range append([]domain.Place{db.Owner}, db.Members...) {
			if _, seen := dbPlaceOf[p.Key()]; !seen {
				dbPlaceOf[p.Key()] = i
			}
		}
	}
	// literals are the readable literal variables that are no credential
	// URL, by value hash, for the copies found by value.
	literals := map[string][]*store.Variable{}
	for _, p := range places {
		for _, v := range s.VariablesAt(p) {
			if strings.HasPrefix(v.Name, "RAILWAY_") || !v.Literal || v.Sealed || v.Unrendered == "" {
				continue
			}
			if _, isURL := parseCredURL(v.Unrendered); !isURL {
				literals[hashOf(v.Unrendered)] = append(literals[hashOf(v.Unrendered)], v)
			}
		}
	}

	follows := func(from []string) []URLPlace {
		seen := map[string]bool{}
		var out []URLPlace
		for _, id := range from {
			for _, d := range s.Dependents(id) {
				v := s.Variable(d)
				if v == nil || seen[d] || slices.Contains(from, d) {
					continue
				}
				if _, read := order[v.Place.Key()]; !read {
					continue
				}
				seen[d] = true
				out = append(out, URLPlace{Loc: locOf(v, since), Mode: URLFollows})
			}
		}
		return out
	}
	// unrecognised lists the stray URLs carrying one of the hashed passwords.
	unrecognised := func(hashes map[string]bool) []URLPlace {
		var out []URLPlace
		for _, st := range strays {
			if slices.ContainsFunc(st.hashes, func(h string) bool { return hashes[h] }) {
				out = append(out, URLPlace{Loc: st.loc, Mode: URLUnrecognised})
			}
		}
		return out
	}

	var rows []PasswordRow
	for i, db := range dbs {
		byUser := map[string][]URLPlace{}
		authDBs := map[string][]string{} // Mongo: by user, the databases its URLs authenticate against
		for _, u := range urls {
			if u.db == i {
				byUser[u.user] = append(byUser[u.user], u.place)
				if u.authDB != "" && !slices.Contains(authDBs[u.user], u.authDB) {
					authDBs[u.user] = append(authDBs[u.user], u.authDB)
				}
			}
		}
		// shared counts, by hash, the current passwords of this database's
		// rows (its root, each user): a value two rows hold is never copied
		// automatically, whose copy it is cannot be told.
		shared := map[string]int{}
		if own := s.Variable(domain.LocationID(db.Owner.EnvID, db.Owner.ServiceID, rootVar(db.Kind))); own != nil && own.Literal && !own.Sealed && own.Unrendered != "" {
			shared[hashOf(own.Unrendered)]++
		}
		for u, ps := range byUser {
			if u == db.RootUser {
				continue
			}
			if pw, ok := rowPassword(s, PasswordRow{URLs: ps}); ok {
				shared[hashOf(pw)]++
			}
		}
		others := func(user string) []URLPlace {
			var out []URLPlace
			for _, u := range urls {
				if u.db == i && u.user != user {
					p := u.place
					p.Mode = URLOther
					out = append(out, p)
				}
			}
			return out
		}
		// build makes the row of user; own is the root password variable
		// (nil for a user row).
		build := func(user string, own *store.Variable) PasswordRow {
			r := PasswordRow{Key: DBGroupKey(db.Key, user), DB: db, User: user}
			hashes := map[string]bool{}
			var from []string
			if own != nil {
				l := locOf(own, since)
				r.Own = &l
				from = append(from, own.ID)
				if own.Literal && !own.Sealed {
					hashes[hashOf(own.Unrendered)] = true
				}
			}
			for _, p := range byUser[user] {
				r.URLs = append(r.URLs, p)
				from = append(from, p.Loc.ID)
				if !p.Unclear {
					for _, h := range passwordHashes(s.Variable(p.Loc.ID).Unrendered) {
						hashes[h] = true
					}
				}
			}
			r.URLs = append(r.URLs, follows(from)...)
			if len(hashes) > 0 {
				r.URLs = append(r.URLs, unrecognised(hashes)...)
			}
			r.Others = others(user)
			// Literal copies of the password, by value (any name).
			if pw, ok := rowPassword(s, r); ok {
				var copyIDs []string
				for _, v := range literals[hashOf(pw)] {
					if own != nil && v.ID == own.ID {
						continue
					}
					l := locOf(v, since)
					switch at, isDB := dbPlaceOf[v.Place.Key()]; {
					case dbVar[v.ID] || isDB && at != i:
						r.Others = append(r.Others, URLPlace{Loc: l, Mode: URLOtherDB})
					case len(pw) >= copyMinLen && v.Place.EnvID == db.Owner.EnvID && shared[hashOf(pw)] < 2:
						r.Copies = append(r.Copies, URLPlace{Loc: l, Mode: URLRewrite, Copy: true})
						copyIDs = append(copyIDs, v.ID)
					default:
						r.URLs = append(r.URLs, URLPlace{Loc: l, Mode: URLUnrecognised})
					}
				}
				if len(copyIDs) > 0 {
					r.Copies = append(r.Copies, follows(copyIDs)...)
				}
			}
			sortPlaces(r.URLs, order)
			sortPlaces(r.Copies, order)
			sortPlaces(r.Others, order)
			return r
		}

		if own := s.Variable(domain.LocationID(db.Owner.EnvID, db.Owner.ServiceID, rootVar(db.Kind))); own != nil {
			r := build(db.RootUser, own)
			r.Root = true
			if db.Kind == "mongo" {
				r.AuthDB = "admin" // MONGO_INITDB_ROOT_USERNAME is made in admin
			}
			if n := rootFP[own.Fingerprint]; n > 1 {
				r.Problem = strconv.Itoa(n) + " databases share this password: rotate them one at a time"
			}
			if db.RootUser == "" {
				r.Key = DBGroupKey(db.Key, unknownRootUser)
				r.Problem = joinNote("root user unknown · set "+rootUserVar(db.Kind), r.Problem)
			}
			rows = append(rows, r)
		}
		users := make([]string, 0, len(byUser))
		for u := range byUser {
			if u != db.RootUser {
				users = append(users, u)
			}
		}
		sort.Strings(users)
		for _, u := range users {
			r := build(u, nil)
			if db.Kind == "mongo" {
				r.AuthDB = "admin"
				switch dbs := authDBs[u]; {
				case len(dbs) == 1:
					r.AuthDB = dbs[0]
				case len(dbs) > 1:
					sort.Strings(dbs)
					r.Problem = joinNote("its URLs log in to different databases ("+strings.Join(dbs, ", ")+"): change it by hand", r.Problem)
				}
			}
			rows = append(rows, r)
		}
		if kf := keyFileOf(s, db); kf != nil {
			rows = append(rows, keyFileRow(s, db, kf, places, follows, order, since))
		}
	}
	return rows
}

// copyMinLen is the shortest password whose literal copies are written
// with it (and only when no other row of the database holds the same
// password): a shorter value (a user name, "postgres") may be equal by chance,
// so its same-value places need a decision instead.
const copyMinLen = 20

// rowPassword is the current password of a root or user row, a secret used
// only to find its copies: the own variable's value, or the one password
// of a user row's clear rewritten URLs. false when it is unreadable or a
// user row's URLs hold different ones.
func rowPassword(s *store.Store, r PasswordRow) (string, bool) {
	if r.Own != nil {
		v := s.Variable(r.Own.ID)
		if v == nil || !v.Literal || v.Sealed || v.Unrendered == "" {
			return "", false
		}
		return v.Unrendered, true
	}
	pw, found := "", false
	for _, u := range r.URLs {
		if u.Mode != URLRewrite || u.Unclear {
			continue
		}
		v := s.Variable(u.Loc.ID)
		if v == nil || !v.Literal || v.Sealed {
			continue
		}
		spans, _ := passwordSpans(v.Unrendered)
		if len(spans) != 1 {
			continue
		}
		p := v.Unrendered[spans[0][0]:spans[0][1]]
		if d, err := url.PathUnescape(p); err == nil {
			p = d
		}
		if found && p != pw {
			return "", false
		}
		pw, found = p, true
	}
	return pw, found && pw != ""
}

// keyFileRow is a Mongo database's KEYFILE row: the owner's KEYFILE, and as
// copies every literal in the owner's environment with the KEYFILE's
// fingerprint (any name) plus every reference to the KEYFILE or a copy. The
// copies come from the value, not the member list: a member the detection
// missed would keep the old KEYFILE and split the replica set after the
// redeploy. A copy at a place that is neither the owner nor a member gives
// the row a Problem.
func keyFileRow(s *store.Store, db DB, kf *store.Variable, places []domain.Place, follows func([]string) []URLPlace, order map[string]int, since time.Time) PasswordRow {
	r := PasswordRow{Key: DBGroupKey(db.Key, KeyFileUser), DB: db, User: KeyFileUser, KeyFile: true, Warning: warnings["KEYFILE"]}
	l := locOf(kf, since)
	r.Own = &l
	ids := []string{kf.ID}
	for _, p := range places {
		if p.EnvID != db.Owner.EnvID {
			continue
		}
		for _, v := range s.VariablesAt(p) {
			if v.ID != kf.ID && v.Literal && !v.Sealed && v.Fingerprint == kf.Fingerprint {
				ids = append(ids, v.ID)
				r.Copies = append(r.Copies, URLPlace{Loc: locOf(v, since), Mode: URLRewrite})
			}
		}
	}
	r.Copies = append(r.Copies, follows(ids)...)
	known := map[string]bool{db.Owner.Key(): true}
	for _, m := range db.Members {
		known[m.Key()] = true
	}
	for _, c := range r.Copies {
		if !known[c.Loc.Place.Key()] {
			r.Problem = "a KEYFILE copy outside the known members"
			break
		}
	}
	sortPlaces(r.Copies, order)
	return r
}

// rootUserVar names the variable that sets a kind's root user.
func rootUserVar(kind string) string {
	switch kind {
	case "mongo":
		return "MONGO_INITDB_ROOT_USERNAME"
	case "postgres":
		return "POSTGRES_USER"
	default:
		return "REDIS_USER"
	}
}

// keyFileOf is the literal KEYFILE of a Mongo database's owner, or nil.
func keyFileOf(s *store.Store, db DB) *store.Variable {
	if db.Kind != "mongo" {
		return nil
	}
	v := s.Variable(domain.LocationID(db.Owner.EnvID, db.Owner.ServiceID, "KEYFILE"))
	if v == nil || !v.Literal {
		return nil
	}
	return v
}

// sortPlaces orders places as the catalog does, then by name.
func sortPlaces(ps []URLPlace, order map[string]int) {
	sort.SliceStable(ps, func(i, j int) bool {
		oi, oj := order[ps[i].Loc.Place.Key()], order[ps[j].Loc.Place.Key()]
		if oi != oj {
			return oi < oj
		}
		return ps[i].Loc.Name < ps[j].Loc.Name
	})
}

func locOf(v *store.Variable, since time.Time) Loc {
	l := Loc{ID: v.ID, Name: v.Name, Place: v.Place, Updated: parseUpdated(v.UpdatedAt), Sealed: v.Sealed}
	l.Fresh = !since.IsZero() && l.Updated.After(since)
	return l
}

// dbMatch is a database a URL names, and whether through its TCP proxy.
type dbMatch struct {
	db    int
	proxy bool
}

// matchDBs lists, in database order, every database one of the URL's hosts
// names from a variable in envID: exact "host:port" (host case-insensitive);
// a private address only from its own environment. A host without a port
// takes its scheme's default port (defaultPorts) against a private address,
// and never matches a proxy one.
func matchDBs(dbs []DB, u credURL, envID string) []dbMatch {
	var out []dbMatch
	for i := range dbs {
		m, ok := dbMatch{db: i}, false
		for _, h := range u.hosts {
			for _, dh := range dbs[i].Hosts {
				addr := h
				if !dh.Proxy {
					addr = withDefaultPort(h, u.scheme)
				}
				if dh.matches(addr, envID) {
					ok = true
					m.proxy = m.proxy || dh.Proxy
				}
			}
		}
		if ok {
			out = append(out, m)
		}
	}
	return out
}

// allHostsKnown reports whether every host of the URL names a database
// (matchDBs): one that names none would get the new password too, so the
// URL needs a decision by hand.
func allHostsKnown(dbs []DB, u credURL, envID string) bool {
	for _, h := range u.hosts {
		if len(matchDBs(dbs, credURL{scheme: u.scheme, hosts: []string{h}}, envID)) == 0 {
			return false
		}
	}
	return true
}

// defaultPorts are the ports a URL without one reaches, by scheme.
var defaultPorts = map[string]string{
	"mongodb": "27017", "postgres": "5432", "postgresql": "5432", "redis": "6379", "rediss": "6379",
}

// withDefaultPort is host with its scheme's default port when it has none
// (and the scheme has one); host as it is otherwise.
func withDefaultPort(host, scheme string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 && i > strings.LastIndex(host, "]") {
		return host
	}
	if p, ok := defaultPorts[scheme]; ok && host != "" {
		return host + ":" + p
	}
	return host
}

// hashOf is a value's SHA-256 digest, hex-encoded so it survives JSON
// (fingerprints.json); passwords are compared only this way and never kept.
func hashOf(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// Fingerprint is a password's digest as HoldsPassword compares it (hex
// SHA-256): kept to tell whether a place holds that password, never shown.
func Fingerprint(password string) string { return hashOf(password) }

// CurrentPassword is the current password of a password row, as hashes of
// its readings (never the value): its own variable's value (a root or a
// KEYFILE), or, for a user row, the password its clear rewritten URLs carry.
// mixed: a user row's URLs carry different passwords, so there is no single
// current one; hashes then hold every one of them.
func CurrentPassword(s *store.Store, row PasswordRow) (hashes map[string]bool, mixed bool) {
	hashes = map[string]bool{}
	if row.Own != nil {
		if v := s.Variable(row.Own.ID); v != nil && v.Literal && !v.Sealed {
			hashes[hashOf(v.Unrendered)] = true
		}
		return hashes, false
	}
	seen := map[string]bool{}
	for _, u := range row.URLs {
		if u.Mode != URLRewrite || u.Unclear {
			continue
		}
		v := s.Variable(u.Loc.ID)
		if v == nil || !v.Literal || v.Sealed {
			continue
		}
		spans, _ := passwordSpans(v.Unrendered)
		if len(spans) != 1 {
			continue
		}
		pw := v.Unrendered[spans[0][0]:spans[0][1]]
		if d, err := url.PathUnescape(pw); err == nil {
			pw = d
		}
		seen[hashOf(pw)] = true
		for _, h := range passwordHashes(v.Unrendered) {
			hashes[h] = true
		}
	}
	return hashes, len(seen) > 1
}

// HoldsPassword reports whether value — a password, or a credential URL —
// is or carries a password among hashes (CurrentPassword).
func HoldsPassword(value string, hashes map[string]bool) bool {
	if len(hashes) == 0 {
		return false
	}
	if hashes[hashOf(value)] {
		return true
	}
	for _, h := range passwordHashes(value) {
		if hashes[h] {
			return true
		}
	}
	return false
}

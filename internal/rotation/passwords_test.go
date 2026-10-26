package rotation

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
)

const rootPW = "mongo-root-pw-0123456789abc"

// pwCatalog: production holds Mongo, api and worker; staging holds api and
// sync (the cross-environment case: sync reads production's Mongo through its
// proxy).
func pwCatalog() railway.Catalog {
	return railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "Proj", Workspace: "w",
		Environments: []railway.Environment{
			{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"smongo", "smongo2", "smongo3", "spg", "sapi", "sworker"}},
			{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"smongo", "sapi", "ssync"}},
		},
		Services: []railway.Service{
			{ID: "smongo", Name: "mongo"}, {ID: "sapi", Name: "api"},
			{ID: "sworker", Name: "worker"}, {ID: "ssync", Name: "sync"},
			{ID: "smongo2", Name: "mongo-2"}, {ID: "smongo3", Name: "mongo-3"}, {ID: "spg", Name: "pg"},
		},
	}}}
}

func envRec(env, service, name, value string) railway.VarRecord {
	return railway.VarRecord{EnvID: env, ServiceID: service, Name: name, Unrendered: value, Rendered: value}
}

func pwStore(extra ...railway.VarRecord) *store.Store {
	recs := []railway.VarRecord{
		envRec("e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", rootPW),
		envRec("e1", "smongo", "MONGO_INITDB_ROOT_USERNAME", "mongo"),
		envRec("e1", "smongo", "KEYFILE", "keyfile-value-0123456789abcdef"),
		envRec("e1", "smongo", "MONGO_URL", "mongodb://${{MONGO_INITDB_ROOT_USERNAME}}:${{MONGO_INITDB_ROOT_PASSWORD}}@${{RAILWAY_PRIVATE_DOMAIN}}:27017"),
		envRec("e1", "smongo", "RAILWAY_PRIVATE_DOMAIN", "mongo.railway.internal"),
		envRec("e1", "smongo", "RAILWAY_TCP_PROXY_DOMAIN", "a.proxy.rlwy.net"),
		envRec("e1", "smongo", "RAILWAY_TCP_PROXY_PORT", "11111"),
		envRec("e1", "sapi", "MONGO_URL", "${{mongo.MONGO_URL}}"),
		envRec("e1", "sworker", "MONGO_URL", "mongodb://mongo:"+rootPW+"@mongo.railway.internal:27017/db?authSource=admin"),
		envRec("e1", "sworker", "APP_DB", "mongodb://app:app-pw-0123@mongo.railway.internal:27017/app"),
		envRec("e1", "sworker", "REPORT_DB", "mongodb://ro:ro-pw-0123@mongo.railway.internal:27017/app"),
		envRec("e2", "ssync", "MONGO_SOURCE", "mongodb://mongo:"+rootPW+"@a.proxy.rlwy.net:11111/?replicaSet=rs0"),
		envRec("e2", "sapi", "SRC", "${{sync.MONGO_SOURCE}}"),
	}
	s := store.New()
	s.ReplaceCatalog(pwCatalog())
	vars, sys := splitSystem(append(recs, extra...))
	s.ReplaceVariables("p1", vars, sys...)
	return s
}

func pwOptions(envIDs ...string) Options {
	return Options{EnvIDs: envIDs, AllEnvIDs: []string{"e1", "e2"}}
}

func passwordRows(s *store.Store, o Options) []PasswordRow {
	return PasswordRows(s, Analyze(s, o), o)
}

func rowByKey(t *testing.T, rows []PasswordRow, key string) PasswordRow {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no row %q in %v", key, rowKeys(rows))
	return PasswordRow{}
}

func rowKeys(rows []PasswordRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Key
	}
	return out
}

// placeLines renders URL places as "<place key> <NAME> <mode>[ proxy]".
func placeLines(ps []URLPlace) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Loc.Place.Key() + " " + p.Loc.Name + " " + p.Mode.String()
		if p.ViaProxy {
			out[i] += " proxy"
		}
		if p.Unclear {
			out[i] += " unclear"
		}
		if p.Copy {
			out[i] += " copy"
		}
	}
	return out
}

// The cross-environment case: only staging is selected, and MONGO_SOURCE
// there holds production Mongo's root through the TCP proxy.
func TestPasswordRowsOwnerCase(t *testing.T) {
	rows := passwordRows(pwStore(), pwOptions("e2"))
	if got, want := rowKeys(rows), []string{"rotate-db:e1/smongo:mongo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	r := rows[0]
	if !r.Root || r.KeyFile || r.User != "mongo" || r.DB.Key != "e1/smongo" || r.DB.Kind != "mongo" {
		t.Errorf("row = %+v", r)
	}
	if r.Own == nil || r.Own.Name != "MONGO_INITDB_ROOT_PASSWORD" || r.Own.Place.Key() != "e1/smongo" {
		t.Errorf("Own = %+v", r.Own)
	}
	want := []string{
		"e1/sapi MONGO_URL follows",
		"e1/smongo MONGO_URL follows",
		"e1/sworker MONGO_URL rewrite",
		"e2/sapi SRC follows",
		"e2/ssync MONGO_SOURCE rewrite proxy",
	}
	if got := placeLines(r.URLs); !reflect.DeepEqual(got, want) {
		t.Errorf("URLs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantOthers := []string{"e1/sworker APP_DB other", "e1/sworker REPORT_DB other"}
	if got := placeLines(r.Others); !reflect.DeepEqual(got, wantOthers) {
		t.Errorf("Others = %v, want %v", got, wantOthers)
	}
	if r.Warning != "" || r.Problem != "" {
		t.Errorf("Warning %q Problem %q", r.Warning, r.Problem)
	}
}

func TestPasswordRowsAllRows(t *testing.T) {
	rows := passwordRows(pwStore(), pwOptions("e1"))
	want := []string{
		"rotate-db:e1/smongo:mongo",
		"rotate-db:e1/smongo:app",
		"rotate-db:e1/smongo:ro",
		"rotate-db:e1/smongo:@keyfile",
	}
	if got := rowKeys(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}

	app := rowByKey(t, rows, "rotate-db:e1/smongo:app")
	if app.Root || app.KeyFile || app.User != "app" || app.Own != nil || app.DB.Key != "e1/smongo" {
		t.Errorf("app row = %+v", app)
	}
	if got, want := placeLines(app.URLs), []string{"e1/sworker APP_DB rewrite"}; !reflect.DeepEqual(got, want) {
		t.Errorf("app URLs = %v, want %v", got, want)
	}
	wantOthers := []string{
		"e1/sworker MONGO_URL other",
		"e1/sworker REPORT_DB other",
		"e2/ssync MONGO_SOURCE other proxy",
	}
	if got := placeLines(app.Others); !reflect.DeepEqual(got, wantOthers) {
		t.Errorf("app Others = %v, want %v", got, wantOthers)
	}

	kf := rowByKey(t, rows, "rotate-db:e1/smongo:@keyfile")
	if !kf.KeyFile || kf.Root || kf.Own == nil || kf.Own.Name != "KEYFILE" || len(kf.URLs) != 0 || len(kf.Others) != 0 {
		t.Errorf("keyfile row = %+v", kf)
	}
	if kf.Warning != warnings["KEYFILE"] {
		t.Errorf("keyfile Warning = %q", kf.Warning)
	}
}

// A row appears when its database or any URL place is selected: user rows
// whose places all live in production are absent when only staging is.
func TestPasswordRowsSelection(t *testing.T) {
	s := pwStore(envRec("e2", "ssync", "APP_SOURCE", "mongodb://app:app-pw-0123@a.proxy.rlwy.net:11111/app"))
	rows := passwordRows(s, pwOptions("e2"))
	want := []string{"rotate-db:e1/smongo:mongo", "rotate-db:e1/smongo:app"}
	if got := rowKeys(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	app := rows[1]
	// Places in unselected environments stay on the row.
	if got, want := placeLines(app.URLs), []string{"e1/sworker APP_DB rewrite", "e2/ssync APP_SOURCE rewrite proxy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("app URLs = %v, want %v", got, want)
	}
}

func TestPasswordRowsSharedRoot(t *testing.T) {
	s := pwStore(
		envRec("e1", "sworker", "MONGO_INITDB_ROOT_PASSWORD", rootPW),
		envRec("e1", "sworker", "MONGO_INITDB_ROOT_USERNAME", "mongo"),
	)
	rows := passwordRows(s, pwOptions("e1"))
	r := rowByKey(t, rows, "rotate-db:e1/smongo:mongo")
	if r.Problem != "2 databases share this password: rotate them one at a time" {
		t.Errorf("Problem = %q", r.Problem)
	}
}

func TestRowsLeaveKeyfileToPasswordRow(t *testing.T) {
	s := pwStore(envRec("e1", "sapi", "KEYFILE", "another-keyfile-0123456789abcdef"))
	var labels []string
	for _, r := range Rows(Analyze(s, pwOptions("e1"))) {
		if r.Label == "KEYFILE" {
			for _, g := range r.Groups {
				for _, l := range g.Locs {
					labels = append(labels, l.Place.Key())
				}
			}
		}
	}
	// The Mongo KEYFILE is its password row; a KEYFILE elsewhere stays GEN.
	if want := []string{"e1/sapi"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("KEYFILE row places = %v, want %v", labels, want)
	}
}

func TestDBGroupKey(t *testing.T) {
	g := DBGroupKey("e1/smongo", "app")
	if g != "rotate-db:e1/smongo:app" {
		t.Fatalf("DBGroupKey = %q", g)
	}
	if db, user, ok := ParseDBGroupKey(g); !ok || db != "e1/smongo" || user != "app" {
		t.Errorf("ParseDBGroupKey = %q %q %v", db, user, ok)
	}
	if _, ok := ParseGroupKey(g); ok {
		t.Errorf("ParseGroupKey accepted %q", g)
	}
	for _, bad := range []string{"rotate:abc", "rotate:add:X", "rotate-db:", "rotate-db:e1/smongo", "other"} {
		if _, _, ok := ParseDBGroupKey(bad); ok {
			t.Errorf("ParseDBGroupKey accepted %q", bad)
		}
	}
	if db, user, ok := ParseDBGroupKey(DBGroupKey("e1/smongo", KeyFileUser)); !ok || db != "e1/smongo" || user != KeyFileUser {
		t.Errorf("keyfile key = %q %q %v", db, user, ok)
	}
}

func TestDBCoverageOf(t *testing.T) {
	s := pwStore()
	g := DBGroupKey("e1/smongo", "mongo")
	ch := func(env, svc, target, group, kind string) changeset.Change {
		return changeset.Change{
			Kind: kind, Group: group, Target: target,
			Place: domain.Place{ProjectID: "p1", EnvID: env, ServiceID: svc},
			New:   json.RawMessage(`{}`),
		}
	}
	changes := []changeset.Change{
		ch("e2", "ssync", "MONGO_SOURCE", g, "variable.set"),
		ch("e1", "sworker", "MONGO_URL", "", "variable.keep"),
		ch("e1", "smongo", "MONGO_INITDB_ROOT_PASSWORD", "rotate:other", "variable.set"),
		ch("e1", "sapi", "MONGO_URL", g, "variable.set"), // a reference place: not counted
	}
	for _, tc := range []struct {
		envs           []string
		changed, total int
	}{
		{[]string{"e1", "e2"}, 2, 3},
		{[]string{"e2"}, 1, 1},
		{[]string{"e1"}, 1, 2},
	} {
		cov, ok := DBCoverageOf(s, changes, g, pwOptions(tc.envs...))
		if !ok || cov.Group != g || cov.Changed != tc.changed || cov.Total != tc.total {
			t.Errorf("envs %v: cov = %+v ok=%v, want %d/%d", tc.envs, cov, ok, tc.changed, tc.total)
		}
	}
	if _, ok := DBCoverageOf(s, changes, GroupKey("abc"), pwOptions("e1")); ok {
		t.Errorf("DBCoverageOf took a value rotation group")
	}
	if _, ok := CoverageOf(s, changes, g, []string{"e1"}); ok {
		t.Errorf("CoverageOf took a rotate-db group")
	}
	if _, ok := DBCoverageOf(s, changes, DBGroupKey("e9/nope", "mongo"), pwOptions("e1")); ok {
		t.Errorf("DBCoverageOf took an unknown database")
	}
	kf, ok := DBCoverageOf(s, []changeset.Change{ch("e1", "smongo", "KEYFILE", DBGroupKey("e1/smongo", KeyFileUser), "variable.set")},
		DBGroupKey("e1/smongo", KeyFileUser), pwOptions("e1"))
	if !ok || kf.Changed != 1 || kf.Total != 1 {
		t.Errorf("keyfile cov = %+v ok=%v", kf, ok)
	}
}

func TestRewriteURL(t *testing.T) {
	for i, tc := range []struct{ url, user, pw, want string }{
		{
			"mongodb://mongo:old-pw@a.proxy.rlwy.net:11111/?replicaSet=rs0", "mongo", "p@ss:w/rd?#%",
			"mongodb://mongo:p%40ss%3Aw%2Frd%3F%23%25@a.proxy.rlwy.net:11111/?replicaSet=rs0",
		},
		{
			"mongodb://mongo:old@h1:27017,h2:27017,h3:27017/db?authSource=admin&replicaSet=rs0", "mongo", "Ab9-._~",
			"mongodb://mongo:Ab9-._~@h1:27017,h2:27017,h3:27017/db?authSource=admin&replicaSet=rs0",
		},
		{"redis://default:a@b@redis.railway.internal:6379", "default", "new pw+&=$!'()*,;", "redis://default:new%20pw%2B%26%3D%24%21%27%28%29%2A%2C%3B@redis.railway.internal:6379"},
		{"postgresql://app:o%40ld@pg.railway.internal:5432/x?sslmode=require#frag", "app", "pässword", "postgresql://app:p%C3%A4ssword@pg.railway.internal:5432/x?sslmode=require#frag"},
		{"postgres+ssl://app:x@pg:5432", "app", "y", "postgres+ssl://app:y@pg:5432"},
	} {
		got, err := RewriteURL(tc.url, tc.user, tc.pw)
		if err != nil || got != tc.want {
			t.Errorf("case %d: RewriteURL = %q, %v\nwant %q", i, got, err, tc.want)
		}
	}
}

func TestRewriteURLRefuses(t *testing.T) {
	const old = "s3cr3t-old-0123"
	for i, tc := range []struct{ url, user, pw string }{
		{"mongodb://mongo:" + old + "@h:1/db", "app", "new"},             // another user
		{"not a url " + old, "mongo", "new"},                             // not a credential URL
		{"mongodb://mongo:" + old + "@h:1/db", "mongo", ""},              // empty password
		{"mongodb://mongo:" + old + "@x?y@host:1/db", "mongo", "new"},    // unclear: "?" may be the password's
		{"mongodb://mongo:" + old + "@x#frag@host:1/db", "mongo", "new"}, // unclear through "#"
	} {
		got, err := RewriteURL(tc.url, tc.user, tc.pw)
		if err == nil {
			t.Errorf("case %d: RewriteURL = %q, want an error", i, got)
			continue
		}
		if got != "" || strings.Contains(err.Error(), old) || strings.Contains(err.Error(), "x?y") || strings.Contains(err.Error(), "host") {
			t.Errorf("case %d: got %q, error %q leaks", i, got, err)
		}
	}
}

// RebaseURL follows a URL changed in Railway since its rewrite was made
// when it still has the user and the hosts of the rewrite.
func TestRebaseURL(t *testing.T) {
	const rewritten = "mongodb://root:NEW@mongo.railway.internal:27017/db?authSource=admin"
	for i, tc := range []struct {
		current string
		want    string
		ok      bool
	}{
		{"mongodb://root:old-pw@mongo.railway.internal:27017/other?replicaSet=rs0", "mongodb://root:N%40W@mongo.railway.internal:27017/other?replicaSet=rs0", true},
		{"mongodb://root:old-pw@MONGO.railway.internal:27017/db", "mongodb://root:N%40W@MONGO.railway.internal:27017/db", true},
		{"mongodb://root:N%40W@mongo.railway.internal:27017/db", "mongodb://root:N%40W@mongo.railway.internal:27017/db", true},
		{"mongodb://app:old-pw@mongo.railway.internal:27017/db", "", false},      // another user
		{"mongodb://root:old-pw@elsewhere.railway.internal:27017/db", "", false}, // another host
		{"mongodb://root:old-pw@mongo.railway.internal:27018/db", "", false},     // another port
		{"mongodb://root:old@x?y@mongo.railway.internal:27017/db", "", false},    // unclear
		{"${{mongo.MONGO_URL}}", "", false}, // a reference
		{"", "", false},                     // gone
	} {
		got, ok := RebaseURL(tc.current, rewritten, "root", "N@W")
		if ok != tc.ok || got != tc.want {
			t.Errorf("case %d: RebaseURL = %q, %v; want %q, %v", i, got, ok, tc.want, tc.ok)
		}
	}
}

func TestURLModeString(t *testing.T) {
	for m, want := range map[URLMode]string{URLRewrite: "rewrite", URLFollows: "follows", URLOther: "other", URLOtherDB: "otherdb", URLMode(9): "URLMode(" + strconv.Itoa(9) + ")"} {
		if got := m.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", m, got, want)
		}
	}
}

// stagingMongo is staging's own Mongo: the same private domain as
// production's, its own TCP proxy.
func stagingMongo(proxyPort string) []railway.VarRecord {
	return []railway.VarRecord{
		envRec("e2", "smongo", "MONGO_INITDB_ROOT_PASSWORD", "staging-root-pw-0123456789"),
		envRec("e2", "smongo", "MONGO_INITDB_ROOT_USERNAME", "mongo"),
		envRec("e2", "smongo", "RAILWAY_PRIVATE_DOMAIN", "mongo.railway.internal"),
		envRec("e2", "smongo", "RAILWAY_TCP_PROXY_DOMAIN", "a.proxy.rlwy.net"),
		envRec("e2", "smongo", "RAILWAY_TCP_PROXY_PORT", proxyPort),
	}
}

// A private address repeats across environments: it names only the
// database of the URL's own environment; a TCP proxy names its database
// from anywhere.
func TestPasswordRowsPrivateHostStaysInEnv(t *testing.T) {
	s := pwStore(append(stagingMongo("22222"),
		envRec("e2", "sapi", "MONGO_URL", "mongodb://mongo:staging-root-pw-0123456789@mongo.railway.internal:27017/db"))...)
	rows := passwordRows(s, pwOptions("e1", "e2"))
	prod := rowByKey(t, rows, "rotate-db:e1/smongo:mongo")
	stg := rowByKey(t, rows, "rotate-db:e2/smongo:mongo")
	wantProd := []string{
		"e1/sapi MONGO_URL follows",
		"e1/smongo MONGO_URL follows",
		"e1/sworker MONGO_URL rewrite",
		"e2/sapi SRC follows",
		"e2/ssync MONGO_SOURCE rewrite proxy",
	}
	if got := placeLines(prod.URLs); !reflect.DeepEqual(got, wantProd) {
		t.Errorf("prod URLs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantProd, "\n"))
	}
	if got, want := placeLines(stg.URLs), []string{"e2/sapi MONGO_URL rewrite"}; !reflect.DeepEqual(got, want) {
		t.Errorf("staging URLs = %v, want %v", got, want)
	}
	// Value rotation classification follows the same rule.
	g := oneGroup(t, Analyze(s, pwOptions("e1", "e2")), "MONGO_SOURCE")
	if g.DB == nil || g.DB.Key != "e1/smongo" {
		t.Errorf("MONGO_SOURCE DB = %v, want e1/smongo", g.DB)
	}
	for _, g := range groupsWith(Analyze(s, pwOptions("e1", "e2")), "MONGO_URL") {
		for _, l := range g.Locs {
			if l.ID == "e2/sapi/MONGO_URL" && (g.DB == nil || g.DB.Key != "e2/smongo") {
				t.Errorf("staging MONGO_URL DB = %v, want e2/smongo", g.DB)
			}
		}
	}
}

// A proxy is host and port: the shared proxy host on another port, or with
// no port, names no database.
func TestPasswordRowsProxyNeedsPort(t *testing.T) {
	s := pwStore(
		envRec("e2", "ssync", "OTHER_PORT", "mongodb://mongo:x-0123@a.proxy.rlwy.net:33333/"),
		envRec("e2", "ssync", "NO_PORT", "mongodb://mongo:x-0123@a.proxy.rlwy.net/"),
	)
	r := rowByKey(t, passwordRows(s, pwOptions("e2")), "rotate-db:e1/smongo:mongo")
	for _, p := range append(r.URLs, r.Others...) {
		if p.Loc.Name == "OTHER_PORT" || p.Loc.Name == "NO_PORT" {
			t.Errorf("%s listed: %+v", p.Loc.Name, p)
		}
	}
	if a := Analyze(s, pwOptions("e2")); oneGroup(t, a, "OTHER_PORT").DB != nil || oneGroup(t, a, "NO_PORT").DB != nil {
		t.Errorf("classifier matched a proxy host without its port")
	}
}

// Stale data: two databases claim one proxy address. The URL is listed on
// both, unclear, never guessed.
func TestPasswordRowsAmbiguousURLIsUnclear(t *testing.T) {
	s := pwStore(stagingMongo("11111")...)
	rows := passwordRows(s, pwOptions("e1", "e2"))
	for _, key := range []string{"rotate-db:e1/smongo:mongo", "rotate-db:e2/smongo:mongo"} {
		r := rowByKey(t, rows, key)
		found := false
		for _, line := range placeLines(r.URLs) {
			if line == "e2/ssync MONGO_SOURCE rewrite proxy unclear" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s URLs = %v, want MONGO_SOURCE unclear", key, placeLines(r.URLs))
		}
	}
}

func TestPasswordRowsUnclearBoundary(t *testing.T) {
	s := pwStore(envRec("e1", "sworker", "ODD", "mongodb://mongo:pw-0123@x?y@mongo.railway.internal:27017/db"))
	r := rowByKey(t, passwordRows(s, pwOptions("e1")), "rotate-db:e1/smongo:mongo")
	if got := placeLines(r.URLs); !slices.Contains(got, "e1/sworker ODD rewrite unclear") {
		t.Errorf("URLs = %v, want ODD unclear", got)
	}
}

// A URL to an unknown host that carries the row's current password is
// listed, never rewritten; it takes a decision, so coverage counts it.
func TestPasswordRowsUnrecognised(t *testing.T) {
	s := pwStore(
		envRec("e2", "ssync", "OLD_SRC", "mongodb://mongo:"+rootPW+"@old-host.example.com:27017/db"),
		envRec("e2", "ssync", "APP_ELSEWHERE", "mongodb://app:app%2Dpw-0123@elsewhere.example.com:1/db"),
		envRec("e2", "ssync", "UNRELATED", "mongodb://mongo:different-0123@old-host.example.com:27017/db"),
	)
	rows := passwordRows(s, pwOptions("e1", "e2"))
	root := rowByKey(t, rows, "rotate-db:e1/smongo:mongo")
	if got := placeLines(root.URLs); !slices.Contains(got, "e2/ssync OLD_SRC unrecognised") || slices.ContainsFunc(got, func(l string) bool {
		return strings.Contains(l, "UNRELATED") || strings.Contains(l, "APP_ELSEWHERE")
	}) {
		t.Errorf("root URLs = %v", got)
	}
	app := rowByKey(t, rows, "rotate-db:e1/smongo:app")
	if got, want := placeLines(app.URLs), []string{"e1/sworker APP_DB rewrite", "e2/ssync APP_ELSEWHERE unrecognised"}; !reflect.DeepEqual(got, want) {
		t.Errorf("app URLs = %v, want %v", got, want)
	}
	// An unrecognised place alone does not list a row.
	if keys := rowKeys(passwordRows(s, pwOptions("e2"))); slices.Contains(keys, "rotate-db:e1/smongo:app") {
		t.Errorf("app row listed by an unrecognised place: %v", keys)
	}
	cov, _ := DBCoverageOf(s, nil, "rotate-db:e1/smongo:mongo", pwOptions("e2"))
	if cov.Total != 2 {
		t.Errorf("coverage total = %d, want 2 (MONGO_SOURCE and OLD_SRC)", cov.Total)
	}
}

// A private host without a port takes its scheme's default port.
func TestPasswordRowsDefaultPort(t *testing.T) {
	const pgPW = "pg-root-password-0123456789"
	s := pwStore(
		envRec("e1", "sapi", "NOPORT", "mongodb://mongo:"+rootPW+"@mongo.railway.internal/db"),
		envRec("e1", "sapi", "WRONGPORT", "mongodb://mongo:"+rootPW+"@mongo.railway.internal:27018/db"),
		envRec("e1", "spg", "POSTGRES_PASSWORD", pgPW),
		envRec("e1", "spg", "POSTGRES_USER", "postgres"),
		envRec("e1", "spg", "RAILWAY_PRIVATE_DOMAIN", "pg.railway.internal"),
		envRec("e1", "sapi", "PG_URL", "postgresql://postgres:"+pgPW+"@pg.railway.internal/db"),
	)
	rows := passwordRows(s, pwOptions("e1"))
	if got := placeLines(rowByKey(t, rows, "rotate-db:e1/smongo:mongo").URLs); !slices.Contains(got, "e1/sapi NOPORT rewrite") || slices.Contains(got, "e1/sapi WRONGPORT rewrite") {
		t.Errorf("mongo URLs = %v, want NOPORT rewritten, WRONGPORT not", got)
	}
	if got := placeLines(rowByKey(t, rows, "rotate-db:e1/spg:postgres").URLs); !slices.Contains(got, "e1/sapi PG_URL rewrite") {
		t.Errorf("postgres URLs = %v, want PG_URL rewritten", got)
	}
}

// Literal copies of a password, found by value:
// in the owner's environment and 20+ characters long, a copy set to the
// new password; elsewhere, or shorter, an unrecognised place to decide; at
// another database's place, shown only.
func TestPasswordRowsLiteralCopies(t *testing.T) {
	s := pwStore(
		envRec("e1", "sapi", "DB_PASS", rootPW),
		envRec("e2", "sapi", "DB_PASS", rootPW),
		envRec("e1", "sapi", "APP_PASS", "app-pw-0123"),
		envRec("e1", "spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"),
		envRec("e1", "spg", "POSTGRES_USER", "postgres"),
		envRec("e1", "spg", "MIRROR", rootPW),
	)
	rows := passwordRows(s, pwOptions("e1", "e2"))
	root := rowByKey(t, rows, "rotate-db:e1/smongo:mongo")
	if got, want := placeLines(root.Copies), []string{"e1/sapi DB_PASS rewrite copy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root copies = %v, want %v", got, want)
	}
	if got := placeLines(root.URLs); !slices.Contains(got, "e2/sapi DB_PASS unrecognised") {
		t.Errorf("root URLs = %v, want staging's copy unrecognised", got)
	}
	if got := placeLines(root.Others); !slices.Contains(got, "e1/spg MIRROR otherdb") {
		t.Errorf("root others = %v, want the other database's copy shown", got)
	}
	app := rowByKey(t, rows, "rotate-db:e1/smongo:app")
	if got := placeLines(app.URLs); !slices.Contains(got, "e1/sapi APP_PASS unrecognised") || len(app.Copies) != 0 {
		t.Errorf("app URLs = %v copies %v, want the short copy unrecognised", got, placeLines(app.Copies))
	}
	cov, _ := DBCoverageOf(s, nil, "rotate-db:e1/smongo:mongo", pwOptions("e1"))
	if cov.Total != 3 {
		t.Errorf("coverage total = %d, want 3 (the own variable, worker's MONGO_URL, the DB_PASS copy)", cov.Total)
	}
}

// A value two rows of one database hold (a user whose password is the
// root's) is never an automatic copy of either: it is unrecognised; the
// root's own variable on a user row is shown as the database's root.
func TestPasswordRowsSharedPasswordNotCopied(t *testing.T) {
	s := pwStore(
		envRec("e1", "sworker", "TWIN_DB", "mongodb://twin:"+rootPW+"@mongo.railway.internal:27017/app"),
		envRec("e1", "sapi", "DB_PASS", rootPW),
	)
	rows := passwordRows(s, pwOptions("e1"))
	for _, key := range []string{"rotate-db:e1/smongo:mongo", "rotate-db:e1/smongo:twin"} {
		r := rowByKey(t, rows, key)
		if len(r.Copies) != 0 || !slices.Contains(placeLines(r.URLs), "e1/sapi DB_PASS unrecognised") {
			t.Errorf("%s: copies %v URLs %v, want DB_PASS unrecognised", key, placeLines(r.Copies), placeLines(r.URLs))
		}
	}
	twin := rowByKey(t, rows, "rotate-db:e1/smongo:twin")
	if !slices.Contains(placeLines(twin.Others), "e1/smongo MONGO_INITDB_ROOT_PASSWORD otherdb") {
		t.Errorf("twin others = %v, want the root's variable shown", placeLines(twin.Others))
	}
}

// A Mongo user row knows the database its URLs authenticate against
// (authSource, else the URL's database, else admin), and refuses to guess
// when they name different ones: a user of the same name in another
// database is another user.
func TestPasswordRowsMongoAuthDB(t *testing.T) {
	key := func(user string) string {
		return DBGroupKey(domain.Place{EnvID: "e1", ServiceID: "smongo"}.Key(), user)
	}
	rows := passwordRows(pwStore(
		envRec("e1", "sapi", "RO_URL", "mongodb://ro:ro-pw-0123@mongo.railway.internal:27017/app?authSource=reports"),
	), pwOptions("e1"))
	if r := rowByKey(t, rows, key("app")); r.AuthDB != "app" || r.Problem != "" {
		t.Errorf("app row: auth db %q, problem %q; want app", r.AuthDB, r.Problem)
	}
	if r := rowByKey(t, rows, key("mongo")); r.AuthDB != "admin" {
		t.Errorf("root row: auth db %q, want admin", r.AuthDB)
	}
	// ro's URLs disagree: /app (no authSource) and authSource=reports
	if r := rowByKey(t, rows, key("ro")); r.Problem == "" || !strings.Contains(r.Problem, "app") || !strings.Contains(r.Problem, "reports") {
		t.Errorf("ro row: auth db %q, problem %q; want a problem naming both", r.AuthDB, r.Problem)
	}
}

// A URL naming a database and another host as well is no automatic
// rewrite: the other host would get the new password too.
func TestPasswordRowsExtraHostUnclear(t *testing.T) {
	rows := passwordRows(pwStore(
		envRec("e1", "sapi", "MIXED", "mongodb://app:app-pw-0123@mongo.railway.internal:27017,elsewhere.example:27017/app"),
	), pwOptions("e1"))
	r := rowByKey(t, rows, DBGroupKey(domain.Place{EnvID: "e1", ServiceID: "smongo"}.Key(), "app"))
	for _, u := range r.URLs {
		switch u.Loc.ID {
		case domain.LocationID("e1", "sapi", "MIXED"):
			if !u.Unclear {
				t.Error("a URL with a host of no database is rewritten automatically")
			}
		case domain.LocationID("e1", "sworker", "APP_DB"):
			if u.Unclear {
				t.Error("a URL on the database alone needs a decision")
			}
		}
	}
}

func TestPasswordRowsUnknownRootUser(t *testing.T) {
	s := pwStore(envRec("e1", "spg", "POSTGRES_PASSWORD", "pg-root-password-0123456789"))
	r := rowByKey(t, passwordRows(s, pwOptions("e1")), "rotate-db:e1/spg:@root")
	if !r.Root || r.User != "" || r.Problem != "root user unknown · set POSTGRES_USER" {
		t.Errorf("row = %+v", r)
	}
	if db, user, ok := ParseDBGroupKey(r.Key); !ok || db != "e1/spg" || user != "@root" {
		t.Errorf("ParseDBGroupKey(%q) = %q %q %v", r.Key, db, user, ok)
	}
}

func TestPasswordRowsKeyFileCopies(t *testing.T) {
	s := pwStore(
		envRec("e1", "smongo2", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"),
		envRec("e1", "smongo2", "KEYFILE", "keyfile-value-0123456789abcdef"),
		envRec("e1", "smongo2", "REPLICA_SET_NAME", "rs0"),
		envRec("e1", "smongo3", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"),
		envRec("e1", "smongo3", "REPLICA_SET_NAME", "rs0"),
		envRec("e1", "smongo3", "KEYFILE", "${{mongo.KEYFILE}}"),
	)
	kf := rowByKey(t, passwordRows(s, pwOptions("e1")), "rotate-db:e1/smongo:@keyfile")
	if len(kf.URLs) != 0 || len(kf.Others) != 0 {
		t.Errorf("keyfile URLs %v Others %v, want none", kf.URLs, kf.Others)
	}
	if got, want := placeLines(kf.Copies), []string{"e1/smongo2 KEYFILE rewrite", "e1/smongo3 KEYFILE follows"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Copies = %v, want %v", got, want)
	}
	cov, ok := DBCoverageOf(s, nil, kf.Key, pwOptions("e1"))
	if !ok || cov.Total != 2 {
		t.Errorf("keyfile coverage = %+v, want total 2", cov)
	}
}

// KEYFILE copies come from the value: a member's copy under another name
// is found; a copy at a place that is no member is a Problem.
func TestPasswordRowsKeyFileCopiesByValue(t *testing.T) {
	member := []railway.VarRecord{
		envRec("e1", "smongo2", "MONGO_INITDB_ROOT_PASSWORD", "${{mongo.MONGO_INITDB_ROOT_PASSWORD}}"),
		envRec("e1", "smongo2", "MONGO_KEYFILE", "keyfile-value-0123456789abcdef"),
		envRec("e1", "smongo2", "KEYFILE", "${{MONGO_KEYFILE}}"),
		envRec("e1", "smongo2", "REPLICA_SET_NAME", "rs0"),
	}
	kf := rowByKey(t, passwordRows(pwStore(member...), pwOptions("e1")), "rotate-db:e1/smongo:@keyfile")
	if got, want := placeLines(kf.Copies), []string{"e1/smongo2 KEYFILE follows", "e1/smongo2 MONGO_KEYFILE rewrite"}; !reflect.DeepEqual(got, want) || kf.Problem != "" {
		t.Errorf("Copies = %v (problem %q), want %v", got, kf.Problem, want)
	}

	// A node the member rule misses (no REPLICA_SET_NAME) holding a copy.
	stray := append(member, envRec("e1", "smongo3", "KEYFILE", "keyfile-value-0123456789abcdef"))
	kf = rowByKey(t, passwordRows(pwStore(stray...), pwOptions("e1")), "rotate-db:e1/smongo:@keyfile")
	if kf.Problem != "a KEYFILE copy outside the known members" {
		t.Errorf("Problem = %q", kf.Problem)
	}
}

func TestCurrentPassword(t *testing.T) {
	s := pwStore(envRec("e1", "sapi", "APP_DB", "mongodb://app:app%2Dpw-0123@mongo.railway.internal:27017/app"))
	rows := passwordRows(s, pwOptions("e1", "e2"))
	root := rowByKey(t, rows, "rotate-db:e1/smongo:mongo")
	cur, mixed := CurrentPassword(s, root)
	if mixed || !HoldsPassword(rootPW, cur) || HoldsPassword("other-pw", cur) {
		t.Errorf("root: mixed %v, holds %v", mixed, HoldsPassword(rootPW, cur))
	}
	if !HoldsPassword("mongodb://x:"+rootPW+"@h:1/db", cur) || HoldsPassword("mongodb://x:nope@h:1/db", cur) {
		t.Error("a URL carrying the root password is not recognised")
	}
	// app's two URLs carry one password, one of them percent-encoded.
	app := rowByKey(t, rows, "rotate-db:e1/smongo:app")
	cur, mixed = CurrentPassword(s, app)
	if mixed || !HoldsPassword("app-pw-0123", cur) {
		t.Errorf("app: mixed %v", mixed)
	}

	s = pwStore(envRec("e1", "sapi", "APP_DB", "mongodb://app:another-pw-9@mongo.railway.internal:27017/app"))
	app = rowByKey(t, passwordRows(s, pwOptions("e1", "e2")), "rotate-db:e1/smongo:app")
	if cur, mixed = CurrentPassword(s, app); !mixed || !HoldsPassword("app-pw-0123", cur) || !HoldsPassword("another-pw-9", cur) {
		t.Errorf("different passwords: mixed %v, %d hashes", mixed, len(cur))
	}
}

// A Fingerprint survives a JSON round trip (a scrubbed run keeps it in
// fingerprints.json) and still matches the password it was made from.
func TestFingerprintSurvivesJSON(t *testing.T) {
	fp := Fingerprint(rootPW)
	data, err := json.Marshal(map[string]string{"c1": fp})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !HoldsPassword(rootPW, map[string]bool{back["c1"]: true}) {
		t.Errorf("a fingerprint read back from JSON no longer matches its password")
	}
}

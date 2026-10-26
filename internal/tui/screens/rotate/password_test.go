package rotate_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/rotate"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

// Password fixture values: none may ever reach a frame.
const (
	dbRootV   = "mongo-root-pw-0123456789abc"
	dbAppV    = "app-user-pw-0123456789"
	dbPgV     = "postgres-root-pw-0123456789"
	dbKeyV    = "keyfileValue0123456789abcdefABCDEF"
	dbPasted  = "pasted-db-password-0123"
	dbURLV    = "mongodb://mongo:my-own-url-pw-777@mongo.railway.internal:27017/x"
	dbHandV   = "hand-set-password-0123456"
	dbRootGrp = "rotate-db:e1/mongo:mongo"
)

var dbSecrets = []string{dbRootV, dbAppV, dbPgV, dbKeyV, dbPasted, "my-own-url-pw-777", dbHandV}

// The password fixture: production holds Mongo (with a KEYFILE), Postgres,
// api (a reference to Mongo's URL) and worker (a root URL and the user
// app's URL); staging's sync reads production's Mongo through its TCP proxy
// (the cross-environment MONGO_SOURCE case); dev's etl too, and dev is read only.
var dbCatalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{
		{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"mongo", "pg", "api", "worker"}},
		{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"sync"}},
		{ID: "e3", Name: "dev", CanAccess: true, ServiceIDs: []string{"etl"}},
	},
	Services: []railway.Service{
		{ID: "mongo", Name: "mongo"}, {ID: "pg", Name: "pg"}, {ID: "api", Name: "api"},
		{ID: "worker", Name: "worker"}, {ID: "sync", Name: "sync-worker"}, {ID: "etl", Name: "etl"},
	},
}}}

func dbVars() []railway.VarRecord {
	const at = "2026-10-01T09:30:00"
	proxyURL := "mongodb://mongo:" + dbRootV + "@a.proxy.rlwy.net:11111/?replicaSet=rs0"
	return []railway.VarRecord{
		rec("e1", "mongo", "MONGO_INITDB_ROOT_PASSWORD", dbRootV, at),
		rec("e1", "mongo", "MONGO_INITDB_ROOT_USERNAME", "mongo", at),
		rec("e1", "mongo", "KEYFILE", dbKeyV, at),
		rec("e1", "mongo", "MONGO_URL", "mongodb://${{MONGO_INITDB_ROOT_USERNAME}}:${{MONGO_INITDB_ROOT_PASSWORD}}@${{RAILWAY_PRIVATE_DOMAIN}}:27017", at),
		rec("e1", "mongo", "RAILWAY_PRIVATE_DOMAIN", "mongo.railway.internal", at),
		rec("e1", "mongo", "RAILWAY_TCP_PROXY_DOMAIN", "a.proxy.rlwy.net", at),
		rec("e1", "mongo", "RAILWAY_TCP_PROXY_PORT", "11111", at),
		rec("e1", "pg", "POSTGRES_PASSWORD", dbPgV, at),
		rec("e1", "pg", "POSTGRES_USER", "postgres", at),
		rec("e1", "pg", "RAILWAY_PRIVATE_DOMAIN", "pg.railway.internal", at),
		rec("e1", "api", "MONGO_URL", "${{mongo.MONGO_URL}}", at),
		rec("e1", "worker", "MONGO_URL", "mongodb://mongo:"+dbRootV+"@mongo.railway.internal:27017/db?authSource=admin", at),
		rec("e1", "worker", "APP_DB", "mongodb://app:"+dbAppV+"@mongo.railway.internal:27017/app", at),
		rec("e2", "sync", "MONGO_SOURCE", proxyURL, at),
		rec("e3", "etl", "MONGO_SOURCE", proxyURL, at),
	}
}

// legacyVars add to dbVars an old host no database answers to, with the
// root password, and a literal copy of it in staging: unrecognised places.
func legacyVars() []railway.VarRecord {
	const at = "2026-10-01T09:30:00"
	return append(dbVars(),
		rec("e1", "api", "LEGACY_URL", "mongodb://mongo:"+dbRootV+"@old-mongo.example.com:27017/db", at),
		rec("e2", "sync", "ROOT_PASS", dbRootV, at),
	)
}

func dbCtx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(dbCatalog)
	replaceVars(st, dbVars())
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views: &view.Views{Store: st, Reg: kinds.NewRegistry()},
		Settings: &settings.Settings{
			Projects: []string{"p1"}, WriteEnvs: []string{"e1", "e2"},
			Since: "2026-10-20T14:00:00Z",
		},
		Theme: tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Filters: map[string]string{},
		Now:     func() time.Time { return now },
	}
}

// dbCheck fails when a frame of s shows any fixture secret or any value
// the change set carries (generated, pasted or rewritten).
func dbCheck(t *testing.T, c *tui.Ctx, s tui.Screen) string {
	t.Helper()
	got := frame(c, s)
	tuitest.NoSecrets(t, got, append(slices.Clone(dbSecrets), generated(c)...)...)
	return got
}

// dbPress presses keys one by one, checking every frame for secrets, and
// applies the intents to the change set as the root does.
func dbPress(t *testing.T, c *tui.Ctx, s tui.Screen, keys ...tea.KeyMsg) (tui.Screen, []tea.Msg) {
	t.Helper()
	var all []tea.Msg
	for _, k := range keys {
		var ms []tea.Msg
		s, ms = press(c, s, k)
		secrets := append(slices.Clone(dbSecrets), generated(c)...)
		for _, ch := range puts(ms) {
			if v, ok := kinds.SetValue(ch); ok {
				secrets = append(secrets, v)
			}
			if _, v, ok := kinds.PasswordValue(ch); ok {
				secrets = append(secrets, v)
			}
		}
		for _, f := range flashes(ms) {
			tuitest.NoSecrets(t, f.Text, secrets...)
		}
		root(t, c, ms)
		dbCheck(t, c, s)
		all = append(all, ms...)
	}
	return s, all
}

// dbPlaces opens the places screen of the row labelled label.
func dbPlaces(t *testing.T, c *tui.Ctx, label string) tui.Screen {
	t.Helper()
	s := openRow(t, c, label)
	dbCheck(t, c, s)
	return s
}

func passwordChange(t *testing.T, c *tui.Ctx) changeset.Change {
	t.Helper()
	for _, ch := range c.CS.Changes {
		if ch.Kind == "db.password" && ch.Group == dbRootGrp {
			return ch
		}
	}
	t.Fatal("no db.password change of mongo root")
	return changeset.Change{}
}

func changeAt(c *tui.Ctx, loc string) (changeset.Change, bool) {
	for _, ch := range c.CS.Changes {
		if locOf(ch) == loc {
			return ch, true
		}
	}
	return changeset.Change{}, false
}

func TestPasswordListGolden(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := dbCtx(w, 40)
		s := to(t, c, open(t, c), "mongo root")
		s, _ = dbPress(t, c, s, space)
		got := dbCheck(t, c, s)
		for _, want := range []string{"── database (", "↻ DB   mongo root", "⇄ USR  mongo user app", "↻ KEY  mongo KEYFILE"} {
			if !strings.Contains(got, want) {
				t.Errorf("width %d: list misses %q:\n%s", w, want, got)
			}
		}
		tuitest.Golden(t, "password_list_"+itoa(w)+".golden", got)
	}
}

func TestPasswordPlacesGolden(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := dbCtx(w, 24)
		s := dbPlaces(t, c, "mongo root")
		s, _ = dbPress(t, c, s, r("g"))
		s = at(t, c, s, "e1/worker/MONGO_URL")
		s, _ = dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed(dbURLV)...)...)
		s = at(t, c, s, "e3/etl/MONGO_SOURCE")
		s, _ = dbPress(t, c, s, r("k"))
		s = at(t, c, s, "e2/sync/MONGO_SOURCE")
		got := dbCheck(t, c, s)
		for _, want := range []string{
			"⇄ rewritten (root, via proxy)", "${{…}} follows", "user 'app' · untouched",
			"old password stops working", "✎ your value (", "→ new password (32 chars)",
			"1 password · 3 URLs rewritten · 1 your value · 1 kept",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("width %d: places miss %q:\n%s", w, want, got)
			}
		}
		tuitest.Golden(t, "password_places_"+itoa(w)+".golden", got)
	}
}

// The cross-environment case in the list panel: staging's MONGO_SOURCE reads
// production's Mongo through its proxy and is rewritten.
func TestPasswordPanelMongoSource(t *testing.T) {
	c := dbCtx(100, 50)
	s := to(t, c, open(t, c), "mongo root")
	got := dbCheck(t, c, s)
	for _, want := range []string{
		"↻ DB   mongo root · new password 32 letters+digits",
		"password  production · mongo · Mongo   MONGO_INITDB_ROOT_PASSWORD",
		"URLs 5:  4 rewritten · 0 your value · 1 left",
		"! apps reconnect after redeploy · new connections fail ~1–2 min",
		"MONGO_SOURCE  ⇄ rewritten (root, via proxy)",
		"dev · etl is read only · Settings w or k keeps it",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("panel misses %q:\n%s", want, got)
		}
	}
	if !hasPlace(got, "staging", "sync-worker") {
		t.Errorf("panel misses place staging sync-worker:\n%s", got)
	}
}

// space decides a password like a GEN row: 32 letters and digits; on a
// decided row it removes the password change alone (the root removes its
// automatic rewrites).
func TestPasswordSpace(t *testing.T) {
	c := dbCtx(80, 30)
	s := to(t, c, open(t, c), "mongo root")
	s, ms := dbPress(t, c, s, space)
	if f := flashText(ms); f != "✓ mongo root: new password" {
		t.Errorf("flash = %q", f)
	}
	pw := passwordChange(t, c)
	src, v, ok := kinds.PasswordValue(pw)
	if !ok || src != kinds.SourceGenerate || len(v) != 32 || strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		t.Errorf("password: source %q, len %d, ok %v", src, len(v), ok)
	}
	for _, loc := range []string{"e1/mongo/MONGO_INITDB_ROOT_PASSWORD", "e1/worker/MONGO_URL", "e2/sync/MONGO_SOURCE"} {
		ch, ok := changeAt(c, loc)
		if !ok || ch.Group != dbRootGrp || !slices.Equal(ch.Requires, []string{pw.ID}) {
			t.Errorf("%s: change %+v", loc, ch)
		}
	}
	// dev · etl is read only: decided, but it cannot run.
	if !strings.Contains(dbCheck(t, c, s), "✗ blocked") {
		t.Errorf("state:\n%s", frame(c, s))
	}
	_, ms = dbPress(t, c, s, space)
	if got := removed(ms); len(puts(ms)) != 0 || !slices.Equal(got, []string{pw.ID}) {
		t.Errorf("undo: removes %v, puts %d", got, len(puts(ms)))
	}
}

func TestPasswordPlacesPasswordLine(t *testing.T) {
	c := dbCtx(80, 30)
	s := dbPlaces(t, c, "mongo root")
	if got := rotate.PlaceCursor(c, s); got != "e1/mongo/MONGO_INITDB_ROOT_PASSWORD" {
		t.Fatalf("first line %q", got)
	}
	for key, want := range map[string]string{
		"x": "✗ the password's own variable is never deleted",
		"k": "! the password line takes g, p or o",
		"f": "! the password line takes g, p or o",
		"u": "! no password decided yet",
	} {
		_, ms := dbPress(t, c, s, r(key))
		if f := flashText(ms); f != want || len(puts(ms)) != 0 {
			t.Errorf("%s: flash %q, puts %d", key, f, len(puts(ms)))
		}
	}
	// p: masked twice; "${{" and the current password are refused.
	for v, want := range map[string]string{
		"abc${{x}}def-0123": "a password cannot hold ${{",
		dbRootV:             "that is the current, leaked password",
	} {
		_, ms := dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed(v)...)...)
		if f := flashText(ms); !strings.Contains(f, want) || len(puts(ms)) != 0 {
			t.Errorf("paste: flash %q, puts %d", f, len(puts(ms)))
		}
	}
	s, ms := dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed(dbPasted)...)...)
	if src, v, _ := kinds.PasswordValue(passwordChange(t, c)); src != kinds.SourcePaste || v != dbPasted {
		t.Errorf("paste: source %q (value matches %v), flash %q", src, v == dbPasted, flashText(ms))
	}
	// o: the red warning shows with the prompt and stays.
	s, _ = dbPress(t, c, s, r("o"))
	if got := dbCheck(t, c, s); !strings.Contains(got, "database not touched: Railway is written as if the password already") {
		t.Errorf("o prompt misses the warning:\n%s", got)
	}
	s, ms = dbPress(t, c, s, typed(dbHandV)...)
	if src, _, _ := kinds.PasswordValue(passwordChange(t, c)); src != kinds.SourceHand {
		t.Errorf("o: source %q, flash %q", src, flashText(ms))
	}
	if f := flashes(ms); len(f) != 1 || f[0].Kind != '!' || f[0].Text != "mongo root: changed by hand · database not touched" {
		t.Errorf("o flash = %+v", f)
	}
	if got := dbCheck(t, c, s); !strings.Contains(got, "changed by hand") || !strings.Contains(got, "database not touched") {
		t.Errorf("hand decision not shown:\n%s", got)
	}
	// u on the password line removes the password change alone.
	pw := passwordChange(t, c)
	_, ms = dbPress(t, c, s, r("u"))
	if got := removed(ms); !slices.Equal(got, []string{pw.ID}) || len(puts(ms)) != 0 {
		t.Errorf("u: removes %v, puts %d", got, len(puts(ms)))
	}
}

func TestPasswordPostgresNonASCII(t *testing.T) {
	c := dbCtx(80, 30)
	s := dbPlaces(t, c, "pg root")
	_, ms := dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed("pässwörd-0123456789")...)...)
	if f := flashText(ms); !strings.Contains(f, "Postgres password must be ASCII") || len(puts(ms)) != 0 {
		t.Errorf("flash %q, puts %d", f, len(puts(ms)))
	}
}

func TestPasswordPlacesURLLines(t *testing.T) {
	c := dbCtx(80, 30)
	s := dbPlaces(t, c, "mongo root")

	// Before the password is decided a URL decision waits for nothing.
	s = at(t, c, s, "e1/worker/MONGO_URL")
	s, _ = dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed(dbURLV)...)...)
	ch, ok := changeAt(c, "e1/worker/MONGO_URL")
	if !ok || ch.Group != dbRootGrp || len(ch.Requires) != 0 || ch.Origin == nil || ch.Origin.Letter != "" {
		t.Fatalf("early decision = %+v", ch)
	}
	// Deciding the password ties it.
	s = at(t, c, s, "e1/mongo/MONGO_INITDB_ROOT_PASSWORD")
	s, _ = dbPress(t, c, s, r("g"))
	pw := passwordChange(t, c)
	if ch, _ := changeAt(c, "e1/worker/MONGO_URL"); !slices.Equal(ch.Requires, []string{pw.ID}) || value(t, ch) != dbURLV {
		t.Errorf("decided place after g = %+v", ch)
	}
	// A URL decided with p after the password requires it.
	s = at(t, c, s, "e2/sync/MONGO_SOURCE")
	s, ms := dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed(dbURLV)...)...)
	if ch, _ := changeAt(c, "e2/sync/MONGO_SOURCE"); !slices.Equal(ch.Requires, []string{pw.ID}) || ch.Group != dbRootGrp {
		t.Errorf("p after the password = %+v (flash %q)", ch, flashText(ms))
	}
	// g and o belong to the password line.
	for _, key := range []string{"g", "o"} {
		_, ms := dbPress(t, c, s, r(key))
		if f := flashText(ms); !strings.Contains(f, "is for the password line") {
			t.Errorf("%s on a URL: %q", key, f)
		}
	}
	// u on the place's own decision: removed, and rewritten again with the
	// password.
	s, ms = dbPress(t, c, s, r("u"))
	if ch, ok := changeAt(c, "e2/sync/MONGO_SOURCE"); !ok || !rotationAuto(ch) || !slices.Equal(ch.Requires, []string{pw.ID}) {
		t.Errorf("u: change %+v, msgs %#v", ch, flashText(ms))
	}
	// u on an automatic rewrite is refused.
	_, ms = dbPress(t, c, s, r("u"))
	if f := flashText(ms); f != "! rewritten with the password · u on the password line undoes it" {
		t.Errorf("u on a rewrite: %q", f)
	}
	// x deletes a URL place.
	s = at(t, c, s, "e1/worker/MONGO_URL")
	s, _ = dbPress(t, c, s, r("x"))
	if ch, _ := changeAt(c, "e1/worker/MONGO_URL"); ch.Kind != "variable.delete" || ch.Group != dbRootGrp {
		t.Errorf("x = %+v", ch)
	}
	// A reference follows by itself.
	s = at(t, c, s, "e1/api/MONGO_URL")
	_, ms = dbPress(t, c, s, r("p"))
	if f := flashText(ms); f != "! production · api MONGO_URL follows by reference · decide it at its source" {
		t.Errorf("p on a reference: %q", f)
	}
}

func rotationAuto(c changeset.Change) bool {
	return c.Origin != nil && c.Origin.Letter == changeset.LetterRewrite
}

// k is the only decision a read-only environment takes.
func TestPasswordReadOnlyKeep(t *testing.T) {
	c := dbCtx(80, 30)
	s := at(t, c, dbPlaces(t, c, "mongo root"), "e3/etl/MONGO_SOURCE")
	for _, key := range []string{"p", "f", "c", "x"} {
		_, ms := dbPress(t, c, s, r(key))
		if f := flashText(ms); f != "✗ dev · etl is read only · k keeps it" || len(puts(ms)) != 0 {
			t.Errorf("%s: flash %q", key, f)
		}
	}
	s, ms := dbPress(t, c, s, r("k"))
	ch, ok := changeAt(c, "e3/etl/MONGO_SOURCE")
	if !ok || ch.Kind != "variable.keep" || ch.Group != dbRootGrp {
		t.Fatalf("k = %+v (flash %q)", ch, flashText(ms))
	}
	if got := dbCheck(t, c, s); !strings.Contains(got, "old password stops working") {
		t.Errorf("kept line:\n%s", got)
	}
	_, ms = dbPress(t, c, s, r("u"))
	if _, ok := changeAt(c, "e3/etl/MONGO_SOURCE"); ok || !strings.Contains(flashText(ms), "decision undone") {
		t.Errorf("u on a keep: %q", flashText(ms))
	}
}

// The cursor never stops on another user's URL.
func TestPasswordCursorSkipsOthers(t *testing.T) {
	c := dbCtx(80, 30)
	s := dbPlaces(t, c, "mongo root")
	var seen []string
	for range 12 {
		seen = append(seen, rotate.PlaceCursor(c, s))
		s, _ = dbPress(t, c, s, down)
	}
	for range 12 {
		s, _ = dbPress(t, c, s, up)
		seen = append(seen, rotate.PlaceCursor(c, s))
	}
	if slices.Contains(seen, "e1/worker/APP_DB") {
		t.Errorf("cursor stopped on another user's line: %v", seen)
	}
	if !slices.Contains(seen, "e3/etl/MONGO_SOURCE") || seen[len(seen)-1] != "e1/mongo/MONGO_INITDB_ROOT_PASSWORD" {
		t.Errorf("cursor path %v", seen)
	}
	if got := dbCheck(t, c, s); !strings.Contains(got, "user 'app' · untouched") {
		t.Errorf("other user not shown:\n%s", got)
	}
}

// A user row's first line is its password, inside the database.
func TestPasswordUserRow(t *testing.T) {
	c := dbCtx(80, 30)
	s := dbPlaces(t, c, "mongo user app")
	if got := dbCheck(t, c, s); !strings.Contains(got, "user app") {
		t.Errorf("user password line:\n%s", got)
	}
	_, ms := dbPress(t, c, s, r("g"))
	var pw changeset.Change
	for _, ch := range puts(ms) {
		if ch.Kind == "db.password" {
			pw = ch
		}
	}
	if pw.Target != "app" || pw.Group != "rotate-db:e1/mongo:app" {
		t.Errorf("user password change = %+v (flash %q)", pw, flashText(ms))
	}
}

// A KEYFILE asks first; its copies take no decision.
func TestPasswordKeyFile(t *testing.T) {
	c := dbCtx(80, 30)
	s := to(t, c, open(t, c), "mongo KEYFILE")
	s, ms := dbPress(t, c, s, space)
	if len(puts(ms)) != 0 || !strings.Contains(dbCheck(t, c, s), "new KEYFILE?") {
		t.Fatalf("space did not ask:\n%s", frame(c, s))
	}
	_, ms = dbPress(t, c, s, r("y"))
	var kinds_ []string
	for _, ch := range puts(ms) {
		kinds_ = append(kinds_, ch.Kind)
	}
	if !slices.Contains(kinds_, "db.keyfile") || !strings.Contains(flashText(ms), "new KEYFILE") {
		t.Errorf("KEYFILE decision: kinds %v, flash %q", kinds_, flashText(ms))
	}
	p := dbPlaces(t, c, "mongo KEYFILE")
	_, ms = dbPress(t, c, p, r("o"))
	if f := flashText(ms); f != "! a KEYFILE is not changed by hand · g or p" {
		t.Errorf("o on a KEYFILE: %q", f)
	}
}

// A stalled row takes only continue: c in the list asks, then resumes its
// run; enter opens its places read only.
func TestPasswordStalled(t *testing.T) {
	c := dbCtx(80, 30)
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: []apply.RunInfo{{
		ID: "r1", State: apply.RunFailed, Created: now,
		Steps: map[string]apply.StepRecord{
			"dbpw:e1/mongo:mongo": {Status: apply.StatusDone, Acted: true},
			"redeploy:e1/worker":  {Status: apply.StatusFailed},
			"redeploy:e2/sync":    {Status: apply.StatusDone},
		},
	}}}
	s := to(t, c, open(t, c), "mongo root")
	got := dbCheck(t, c, s)
	for _, want := range []string{"✗ stalled", "1 of 2 apps not redeployed · c continue", "c continue"} {
		if !strings.Contains(got, want) {
			t.Errorf("stalled row misses %q:\n%s", want, got)
		}
	}
	_, ms := dbPress(t, c, s, space)
	if f := flashText(ms); f != "✗ continue or revert the stalled run first" {
		t.Errorf("space: %q", f)
	}
	s, ms = dbPress(t, c, s, r("c"))
	if len(ms) != 0 || !strings.Contains(frame(c, s), "continue the stalled rotation of mongo root? (y/n)") {
		t.Fatalf("c did not ask: %#v\n%s", ms, frame(c, s))
	}
	s, ms = dbPress(t, c, s, r("y"))
	if len(ms) != 1 || ms[0] != (tui.ResumeMsg{RunID: "r1"}) {
		t.Errorf("c y: %#v", ms)
	}
	s, ms = dbPress(t, c, s, r("c"), r("n"))
	if len(ms) != 0 {
		t.Errorf("c n: %#v", ms)
	}
	_, ms = dbPress(t, c, s, enter)
	if o, ok := ms[0].(tui.OpenMsg); len(ms) != 1 || !ok || o.Name != "rotate.places" {
		t.Errorf("enter: %#v", ms)
	}
	p := dbPlaces(t, c, "mongo root")
	if got := dbCheck(t, c, p); !strings.Contains(got, "read only · c in the list continues") {
		t.Errorf("places of a stalled row:\n%s", got)
	}
	for _, key := range []string{"g", "p", "o", "k", "u", "x", "f", " "} {
		_, ms := dbPress(t, c, p, r(key))
		if f := flashText(ms); f != "✗ continue or revert the stalled run first" {
			t.Errorf("places %q: %q", key, f)
		}
	}
	_, ms = dbPress(t, c, p, r("c"))
	if f := flashText(ms); f != "! read-only · c in the list continues" {
		t.Errorf("places c: %q", f)
	}
}

func TestPasswordRunStates(t *testing.T) {
	run := func(state string, redeploy string) []apply.RunInfo {
		return []apply.RunInfo{{
			ID: "r1", State: state, Created: now,
			Steps: map[string]apply.StepRecord{
				"dbpw:e1/mongo:mongo": {Status: apply.StatusDone, Acted: true},
				"redeploy:e1/worker":  {Status: redeploy},
				"redeploy:e2/sync":    {Status: apply.StatusDone},
			},
		}}
	}
	for _, tc := range []struct {
		name  string
		runs  []apply.RunInfo
		live  map[string]tui.RunProgress
		state string
	}{
		{"applying", run(apply.RunRunning, apply.StatusPending), map[string]tui.RunProgress{"r1": {State: apply.RunRunning}}, "⟳ applying"},
		{"not redeployed", run(apply.RunFinished, apply.StatusFailed), nil, "◐ changed · 1 not redeployed"},
		{"changed", run(apply.RunFinished, apply.StatusDone), nil, "✓ changed"},
	} {
		c := dbCtx(100, 30)
		c.RunIndex = tui.RunIndex{Loaded: true, Runs: tc.runs}
		c.Running = tc.live
		s := to(t, c, open(t, c), "mongo root")
		if got := dbCheck(t, c, s); !strings.Contains(got, tc.state+"  ↻ DB   mongo root") {
			t.Errorf("%s: state missing:\n%s", tc.name, got)
		}
	}
}

// A password change still waiting in the change set when applied: A selects
// the password groups too.
func TestPasswordApplyPreset(t *testing.T) {
	c := dbCtx(80, 30)
	s := to(t, c, open(t, c), "mongo root")
	s, _ = dbPress(t, c, s, space)
	_, ms := dbPress(t, c, s, r("A"))
	o, ok := ms[0].(tui.OpenMsg)
	if !ok {
		t.Fatalf("A: %#v", ms)
	}
	ids, _ := o.Args[1].([]string)
	if !slices.Contains(ids, passwordChange(t, c).ID) {
		t.Errorf("A misses the password change: %v", ids)
	}
}

// Places carrying the row's password that are not known to follow it (a
// URL to an unknown host, a literal copy outside the database's
// environment) need a decision or k before the password is decided (the
// row reads blocked, space is refused, the line
// takes the cursor and k.
func TestPasswordUnrecognisedLine(t *testing.T) {
	c := dbCtx(80, 30)
	replaceVars(c.Store, legacyVars())
	s := to(t, c, open(t, c), "mongo root")
	const why = "production · api LEGACY_URL carries this password · p f c x or k"
	if got := dbCheck(t, c, s); !strings.Contains(got, "✗ "+why) {
		t.Errorf("row not blocked:\n%s", got)
	}
	_, ms := dbPress(t, c, s, space)
	if f := flashText(ms); f != "✗ "+why {
		t.Errorf("space on a row with an undecided unrecognised place: %q", f)
	}
	if len(c.CS.Changes) != 0 {
		t.Fatalf("decided anyway: %+v", c.CS.Changes)
	}
	s = dbPlaces(t, c, "mongo root")
	if got := dbCheck(t, c, s); !strings.Contains(got, "LEGACY_URL") || !strings.Contains(got, "carries this password · p f c x or k") || !strings.Contains(got, "ROOT_PASS") {
		t.Errorf("unrecognised lines:\n%s", got)
	}
	for _, loc := range []string{"e1/api/LEGACY_URL", "e2/sync/ROOT_PASS"} {
		s = at(t, c, s, loc)
		s, _ = dbPress(t, c, s, r("k"))
		if ch, ok := changeAt(c, loc); !ok || ch.Kind != "variable.keep" || ch.Group != dbRootGrp {
			t.Errorf("k on %s: %+v", loc, ch)
		}
	}
	s = at(t, c, s, "e1/mongo/MONGO_INITDB_ROOT_PASSWORD")
	s, _ = dbPress(t, c, s, r("g"))
	passwordChange(t, c)
	if got := dbCheck(t, c, s); strings.Count(got, "old password stops working") != 2 {
		t.Errorf("kept lines:\n%s", got)
	}
}

// The places of a row with unrecognised places, a literal copy and another
// database's same value, before any decision.
func TestPasswordUnrecognisedGolden(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := dbCtx(w, 24)
		const at = "2026-10-01T09:30:00"
		replaceVars(c.Store, append(legacyVars(),
			rec("e1", "worker", "MONGO_PASS", dbRootV, at),
			rec("e1", "pg", "MIRROR", dbRootV, at)))
		s := dbPlaces(t, c, "mongo root")
		got := dbCheck(t, c, s)
		for _, want := range []string{"carries this password", "⇄ same password", "same value · other database"} {
			if !strings.Contains(got, want) {
				t.Errorf("width %d: places miss %q:\n%s", w, want, got)
			}
		}
		tuitest.Golden(t, "password_unrecognised_"+itoa(w)+".golden", got)
	}
}

// A user whose password is the root's: the root's variable is shown as
// this database's root, and a literal copy of the value is unrecognised.
func TestPasswordSharedWithRoot(t *testing.T) {
	c := dbCtx(100, 30)
	const at = "2026-10-01T09:30:00"
	replaceVars(c.Store, append(dbVars(),
		rec("e1", "worker", "TWIN_DB", "mongodb://twin:"+dbRootV+"@mongo.railway.internal:27017/app", at),
		rec("e1", "api", "DB_PASS", dbRootV, at)))
	s := dbPlaces(t, c, "mongo user twin")
	got := dbCheck(t, c, s)
	for _, want := range []string{"same value · root of this database · untouched", "carries this password · p f c x or k"} {
		if !strings.Contains(got, want) {
			t.Errorf("places miss %q:\n%s", want, got)
		}
	}
}

// Letter selection never reaches another user's line: no letter is on a
// password row, so a key acts on the cursor's line alone.
func TestPasswordLetterSelectionSkipsOthers(t *testing.T) {
	c := dbCtx(80, 30)
	s := dbPlaces(t, c, "mongo root")
	s, _ = dbPress(t, c, s, r("A"))
	_, ms := dbPress(t, c, s, r("k"))
	if f := flashText(ms); f != "! the password line takes g, p or o" {
		t.Errorf("k after A: %q", f)
	}
	if _, ok := changeAt(c, "e1/worker/APP_DB"); ok || len(c.CS.Changes) != 0 {
		t.Errorf("changes = %+v", c.CS.Changes)
	}
}

// With staging alone selected, production Mongo's row still lists its
// production places.
func TestPasswordStagingOnly(t *testing.T) {
	c := dbCtx(80, 30)
	c.Settings.Envs = []string{"e2"}
	s := dbPlaces(t, c, "mongo root")
	got := dbCheck(t, c, s)
	for _, pl := range [][2]string{{"production", "worker"}, {"production", "mongo"}, {"staging", "sync-worker"}} {
		if !hasPlace(got, pl[0], pl[1]) {
			t.Errorf("staging only misses %s %s:\n%s", pl[0], pl[1], got)
		}
	}
}

// A value kept for a row is dropped once the row has a run or Railway holds
// it: deciding again gives a new password.
func TestPasswordCachedValueDropped(t *testing.T) {
	c := dbCtx(80, 30)
	s := to(t, c, open(t, c), "mongo root")
	s, _ = dbPress(t, c, s, space)
	_, first, _ := kinds.PasswordValue(passwordChange(t, c))
	// Applied elsewhere: the change is gone and Railway holds the value.
	vs := dbVars()
	vs[0].Unrendered, vs[0].Rendered = first, first
	replaceVars(c.Store, vs)
	c.CS = &changeset.ChangeSet{}
	dbSecrets = append(dbSecrets, first)
	defer func() { dbSecrets = dbSecrets[:len(dbSecrets)-1] }()
	s, _ = dbPress(t, c, s, space)
	if _, second, _ := kinds.PasswordValue(passwordChange(t, c)); second == first {
		t.Error("the applied password was decided again")
	}
	// A decision seen put, then removed elsewhere: the next one is new.
	_, third, _ := kinds.PasswordValue(passwordChange(t, c))
	dbCheck(t, c, s) // the screen sees the change
	c.CS = &changeset.ChangeSet{}
	_, _ = dbPress(t, c, s, space)
	if _, fourth, _ := kinds.PasswordValue(passwordChange(t, c)); fourth == third {
		t.Error("a decision removed elsewhere was decided again")
	}
}

// Two presses before the root puts the first decide one password.
func TestPasswordDoublePress(t *testing.T) {
	c := dbCtx(80, 30)
	s := to(t, c, open(t, c), "mongo root")
	s, ms1 := press(c, s, space)
	_, ms2 := press(c, s, space)
	var vals []string
	for _, ch := range append(puts(ms1), puts(ms2)...) {
		if _, v, ok := kinds.PasswordValue(ch); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) != 2 || vals[0] != vals[1] {
		t.Errorf("two presses gave %d passwords, equal %v", len(vals), len(vals) == 2 && vals[0] == vals[1])
	}
}

// u at an Unclear place removes its decision without a rewrite: the row
// then needs it set by hand.
func TestPasswordUndoUnclear(t *testing.T) {
	c := dbCtx(80, 30)
	odd := rec("e1", "worker", "ODD_URL", "mongodb://mongo:"+dbRootV+"@x?y@mongo.railway.internal:27017/db", "2026-10-01T09:30:00")
	replaceVars(c.Store, append(dbVars(), odd))
	s := at(t, c, dbPlaces(t, c, "mongo root"), "e1/worker/ODD_URL")
	s, _ = dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed(dbURLV)...)...)
	s = at(t, c, s, "e1/mongo/MONGO_INITDB_ROOT_PASSWORD")
	s, _ = dbPress(t, c, s, r("g"))
	s = at(t, c, s, "e1/worker/ODD_URL")
	_, ms := dbPress(t, c, s, r("u"))
	f := flashes(ms)
	if len(f) != 1 || f[0].Kind != '!' || f[0].Text != "1 place: decision undone · set this place by hand before running" {
		t.Errorf("flash = %+v", f)
	}
	if _, ok := changeAt(c, "e1/worker/ODD_URL"); ok {
		t.Error("the unclear place still has a change")
	}
	if _, ok := changeAt(c, "e1/worker/MONGO_URL"); !ok {
		t.Error("the other places lost their rewrites")
	}
}

// A user row whose URLs hold different passwords refuses either of them.
func TestPasswordUserMixedRefusesEither(t *testing.T) {
	c := dbCtx(80, 30)
	const other = "other-app-pw-0001"
	dbSecrets = append(dbSecrets, other)
	defer func() { dbSecrets = dbSecrets[:len(dbSecrets)-1] }()
	replaceVars(c.Store, append(dbVars(), rec("e1", "api", "APP_DB", "mongodb://app:"+other+"@mongo.railway.internal:27017/app", "2026-10-01T09:30:00")))
	u := dbPlaces(t, c, "mongo user app")
	if got := dbCheck(t, c, u); !strings.Contains(got, "different passwords") {
		t.Errorf("mixed row not blocked:\n%s", got)
	}
	for _, v := range []string{dbAppV, other} {
		_, ms := dbPress(t, c, u, append([]tea.KeyMsg{r("p")}, typed(v)...)...)
		if f := flashText(ms); f != "✗ that is the current, leaked password" {
			t.Errorf("paste of an old password: %q", f)
		}
	}
}

// URLs carrying the current password are refused, as is the password line
// as a copy source; a user row's current password comes from its URLs.
func TestPasswordLeakedURLRefused(t *testing.T) {
	c := dbCtx(80, 30)
	s := at(t, c, dbPlaces(t, c, "mongo root"), "e1/worker/MONGO_URL")
	leakedURL := "mongodb://mongo:" + dbRootV + "@mongo.railway.internal:27017/other"
	_, ms := dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed(leakedURL)...)...)
	if f := flashText(ms); f != "✗ that is the current, leaked value" || len(puts(ms)) != 0 {
		t.Errorf("leaked URL: %q", f)
	}
	s, _ = dbPress(t, c, s, r("c"), up, up, up, up, up, up)
	_, ms = dbPress(t, c, s, enter)
	if f := flashText(ms); f != "✗ the password line is not a URL · choose a URL place" {
		t.Errorf("copy from the password line: %q", f)
	}

	u := dbPlaces(t, c, "mongo user app")
	_, ms = dbPress(t, c, u, append([]tea.KeyMsg{r("p")}, typed(dbAppV)...)...)
	if f := flashText(ms); f != "✗ that is the current, leaked password" || len(puts(ms)) != 0 {
		t.Errorf("user's current password: %q", f)
	}
}

// An undo whose password decision cannot be made again is refused: it
// would leave the place undecided.
func TestPasswordUndoRebuildRefused(t *testing.T) {
	c := dbCtx(80, 30)
	s := dbPlaces(t, c, "mongo root")
	s, _ = dbPress(t, c, s, r("g"))
	s = at(t, c, s, "e2/sync/MONGO_SOURCE")
	s, _ = dbPress(t, c, s, append([]tea.KeyMsg{r("p")}, typed(dbURLV)...)...)
	// An edit made elsewhere at another place of the row.
	edit, err := kinds.SetVariable(c.Store, c.CS.Changes, pl("e1", "worker"), "MONGO_URL", dbURLV, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CS.Put(edit, nil); err != nil {
		t.Fatal(err)
	}
	c.CS.Changes[len(c.CS.Changes)-1].Group, c.CS.Changes[len(c.CS.Changes)-1].Origin = "", nil
	_, ms := dbPress(t, c, s, r("u"))
	if f := flashText(ms); !strings.HasPrefix(f, "✗ cannot undo: ") || len(removed(ms)) != 0 {
		t.Errorf("undo: %q, removes %v", f, removed(ms))
	}
}

// The screen refuses a typed password equal to the current one; a revert,
// which sets the current value back by design, still validates.
func TestPasswordRevertStillValidates(t *testing.T) {
	c := dbCtx(80, 30)
	row, ok := rotation.RowOf(c.Store, dbRootGrp, rotation.Options{EnvIDs: []string{"e1", "e2", "e3"}, AllEnvIDs: []string{"e1", "e2", "e3"}})
	if !ok {
		t.Fatal("no row")
	}
	rv, err := kinds.DBPasswordRevert(c.Store, row, dbHandV, now)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if err := kinds.NewRegistry()[rv.Kind].Validate(c.Store, rv); err != nil {
		t.Errorf("revert does not validate: %v", err)
	}
}

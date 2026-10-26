package rotate_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	rootmodel "github.com/men232/railway-manager/internal/tui/root"
	"github.com/men232/railway-manager/internal/tui/screens/keys"
	"github.com/men232/railway-manager/internal/tui/screens/rotate"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	time.Local = time.UTC
	os.Exit(m.Run())
}

// Fixture values: none may ever reach a frame.
const (
	jwtV    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	appA    = "app-secret-value-alpha-0001"
	appB    = "app-secret-value-bravo-0002"
	encV    = "encryption-secret-value-0003"
	stripeV = "sk_live_fixture_value_0004"
	publicV = "public-site-key-value-0005"
	rootV   = "mongo-root-password-0006"
	sessV   = "session-secret-value-0007"
	adminV  = "admin-token-value-0008"
)

var secrets = []string{jwtV, appA, appB, encV, stripeV, publicV, rootV, sessV, adminV}

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func pl(env, svc string) domain.Place {
	return domain.Place{ProjectID: "p1", EnvID: env, ServiceID: svc}
}

var catalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{
		{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"api", "web", "mongo"}},
		{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"api"}},
	},
	Services: []railway.Service{{ID: "api", Name: "api"}, {ID: "web", Name: "web"}, {ID: "mongo", Name: "mongo"}},
}}}

func rec(env, svc, name, value, updated string) railway.VarRecord {
	return railway.VarRecord{EnvID: env, ServiceID: svc, Name: name, Unrendered: value, Rendered: value, UpdatedAt: updated}
}

func vars() []railway.VarRecord {
	return []railway.VarRecord{
		rec("e1", "api", "JWT_SECRET", jwtV, "2026-10-01T09:30:00"),
		rec("e1", "web", "JWT_SECRET", jwtV, "2026-10-01T09:30:00"),
		rec("e2", "api", "JWT_SECRET", jwtV, "2026-10-01T09:30:00"),
		rec("e1", "api", "APP_SECRET", appA, "2026-10-02T10:00:00"),
		rec("e2", "api", "APP_SECRET", appB, "2026-10-21T08:00:00"),
		rec("e1", "api", "ENCRYPTION_SECRET_KEY", encV, "2026-10-03T10:00:00"),
		rec("e1", "api", "STRIPE_API_KEY", stripeV, "2026-10-04T10:00:00"),
		rec("e1", "web", "STRIPE_API_KEY", stripeV, "2026-10-04T10:00:00"),
		rec("e1", "web", "NUXT_PUBLIC_SITE_KEY", publicV, "2026-10-05T10:00:00"),
		rec("e2", "api", "SESSION_PASSWORD", sessV, "2026-10-06T10:00:00"),
		rec("e1", "mongo", "ADMIN_TOKEN", adminV, "2026-10-06T10:00:00"),
		rec("e1", "mongo", "MONGO_INITDB_ROOT_PASSWORD", rootV, "2026-10-07T10:00:00"),
		rec("e1", "mongo", "MONGO_INITDB_ROOT_USERNAME", "mongo", "2026-10-07T10:00:00"),
		rec("e1", "mongo", "RAILWAY_PRIVATE_DOMAIN", "mongo.railway.internal", "2026-10-07T10:00:00"),
		rec("e1", "api", "MONGO_URL", "mongodb://mongo:"+rootV+"@mongo.railway.internal:27017/app", "2026-10-07T10:00:00"),
		rec("e1", "web", "USES_JWT", "${{api.JWT_SECRET}}", "2026-10-07T10:00:00"),
	}
}

// replaceVars holds recs in p1. Records named like a Railway system value
// (railway.SystemVarNames) become system values, as discovery delivers them.
func replaceVars(st *store.Store, recs []railway.VarRecord) {
	var vs []railway.VarRecord
	var sys []railway.SystemRecord
	for _, r := range recs {
		if slices.Contains(railway.SystemVarNames, r.Name) {
			sys = append(sys, railway.SystemRecord{EnvID: r.EnvID, ServiceID: r.ServiceID, Name: r.Name, Value: r.Rendered})
			continue
		}
		vs = append(vs, r)
	}
	st.ReplaceVariables("p1", vs, sys...)
}

func ctx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(catalog)
	replaceVars(st, vars())
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views: &view.Views{Store: st, Reg: kinds.NewRegistry()},
		Settings: &settings.Settings{
			Projects: []string{"p1"}, WriteEnvs: []string{"e1"},
			Since: "2026-10-20T14:00:00Z",
		},
		Theme: tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Filters: map[string]string{},
		Now:     func() time.Time { return now },
	}
}

func fp(c *tui.Ctx, loc string) string { return c.Store.Variable(loc).Fingerprint }

// put adds a change to the change set as the root would.
func put(t *testing.T, c *tui.Ctx, ch changeset.Change) {
	t.Helper()
	if err := c.CS.Put(ch, nil); err != nil {
		t.Fatal(err)
	}
}

// rotateSet puts a Rotate-made set of value at a place.
func rotateSet(t *testing.T, c *tui.Ctx, p domain.Place, name, value, rowKey, letter string) changeset.Change {
	t.Helper()
	ch, err := kinds.SetVariable(c.Store, c.CS.Changes, p, name, value, now)
	if err != nil {
		t.Fatal(err)
	}
	ch.Group = rotation.GroupKey(fp(c, domain.LocationID(p.EnvID, p.ServiceID, name)))
	ch.Origin = &changeset.Origin{Producer: "rotate", Row: rowKey, Letter: letter}
	put(t, c, ch)
	return ch
}

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	space = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	enter = tea.KeyMsg{Type: tea.KeyEnter}
	down  = tea.KeyMsg{Type: tea.KeyDown}
)

// settle runs cmd and feeds component messages back into the screen,
// returning the intents for the root.
func settle(c *tui.Ctx, s tui.Screen, cmd tea.Cmd) (tui.Screen, []tea.Msg) {
	var out []tea.Msg
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch m := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range m {
				run(c)
			}
		case nil:
		case components.SubmitMsg, components.CancelMsg, components.ConfirmedMsg, components.FilePickedMsg:
			var next tea.Cmd
			s, next = s.Update(c, m)
			run(next)
		default:
			if !rotate.IsInternal(m) {
				out = append(out, m)
				return
			}
			var next tea.Cmd
			s, next = s.Update(c, m)
			run(next)
		}
	}
	run(cmd)
	return s, out
}

func press(c *tui.Ctx, s tui.Screen, keys ...tea.KeyMsg) (tui.Screen, []tea.Msg) {
	var all []tea.Msg
	for _, k := range keys {
		var cmd tea.Cmd
		var ms []tea.Msg
		s, cmd = s.Update(c, k)
		s, ms = settle(c, s, cmd)
		all = append(all, ms...)
	}
	return s, all
}

// open builds the screen and runs its Init.
func open(t *testing.T, c *tui.Ctx, args ...any) tui.Screen {
	t.Helper()
	s := rotate.Build(args...)
	if s == nil {
		t.Fatalf("Build(%v) = nil", args)
	}
	s, _ = settle(c, s, s.Init(c))
	return s
}

func frame(c *tui.Ctx, s tui.Screen) string {
	note := ""
	if n, ok := s.(tui.Noter); ok {
		note = n.Note(c)
	}
	return tuitest.Plain(tui.Frame(c, s.Title(), note, s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
}

// to moves the cursor to the row labelled label.
func to(t *testing.T, c *tui.Ctx, s tui.Screen, label string) tui.Screen {
	t.Helper()
	for range 40 {
		if rotate.Cursor(c, s) == label {
			return s
		}
		s, _ = press(c, s, down)
	}
	t.Fatalf("no row %q reached; cursor on %q", label, rotate.Cursor(c, s))
	return s
}

func puts(ms []tea.Msg) []changeset.Change {
	var out []changeset.Change
	for _, m := range ms {
		switch x := m.(type) {
		case tui.PutChangeMsg:
			out = append(out, x.Change)
		case tui.ChangesMsg:
			out = append(out, x.Put...)
		}
	}
	return out
}

// flashes are the flashes ms show; a tui.ChangesMsg shows its Done, as the
// root does when every intent succeeds.
func flashes(ms []tea.Msg) []tui.FlashMsg {
	var out []tui.FlashMsg
	for _, m := range ms {
		switch x := m.(type) {
		case tui.FlashMsg:
			out = append(out, x)
		case tui.ChangesMsg:
			kind := x.Kind
			if kind == 0 {
				kind = '✓'
			}
			out = append(out, tui.FlashMsg{Kind: kind, Text: x.Done})
		}
	}
	return out
}

func value(t *testing.T, ch changeset.Change) string {
	t.Helper()
	v, ok := kinds.SetValue(ch)
	if !ok {
		t.Fatalf("change %s is not a set", ch.Kind)
	}
	return v
}

func TestRotateGolden(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 30)
		rowKey := fp(c, "e1/api/JWT_SECRET")
		rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", "new-value-for-jwt-000000", rowKey, "a")
		rotateSet(t, c, pl("e1", "web"), "JWT_SECRET", "new-value-for-jwt-000000", rowKey, "a")
		stripeKey := fp(c, "e1/api/STRIPE_API_KEY")
		rotateSet(t, c, pl("e1", "api"), "STRIPE_API_KEY", "new-stripe-value-0000000", stripeKey, "a")
		s := open(t, c)
		s, _ = press(c, s, r("h")) // the public row shows
		s = to(t, c, s, "JWT_SECRET")
		got := frame(c, s)
		tuitest.NoSecrets(t, got, append(secrets, "new-value-for-jwt-000000", "new-stripe-value-0000000")...)
		tuitest.Golden(t, "rotate_"+itoa(w)+".golden", got)
	}
}

func itoa(n int) string {
	if n == 80 {
		return "80"
	}
	return "60"
}

func TestRotateNoLeakTime(t *testing.T) {
	c := ctx(80, 30)
	c.Settings.Since = ""
	s := open(t, c)
	if n := s.(tui.Noter).Note(c); !strings.HasPrefix(n, "no leak time · o settings · ") || !strings.HasSuffix(n, " rows") {
		t.Errorf("note = %q", n)
	}
	c.Settings.Since = "2026-10-20T14:00:00Z"
	if n := s.(tui.Noter).Note(c); !strings.HasPrefix(n, "leak 10-20 14:00 · ") {
		t.Errorf("note = %q", n)
	}
}

// A read that failed for some places says so above the rows: a secret
// there is missing from them and from every count.
func TestRotatePartialReadWarns(t *testing.T) {
	c := ctx(80, 30)
	s := open(t, c)
	const warning = "the last read missed some places: a secret there is not counted"
	if strings.Contains(frame(c, s), warning) {
		t.Fatal("a complete read warns")
	}
	c.Store.FinishLoadWarn(store.InventoryKey("p1"), now, "proj/production/web: rate limited")
	if !strings.Contains(frame(c, s), warning) {
		t.Errorf("a partial read does not warn:\n%s", frame(c, s))
	}
}

func TestRotateSpaceGenerates(t *testing.T) {
	c := ctx(80, 30)
	s := to(t, c, open(t, c), "JWT_SECRET")
	s, ms := press(c, s, space)
	ch := puts(ms)
	if len(ch) != 2 { // the staging place is read only
		t.Fatalf("puts = %d, want 2: %#v", len(ch), ms)
	}
	rowKey := fp(c, "e1/api/JWT_SECRET")
	v := value(t, ch[0])
	if value(t, ch[1]) != v {
		t.Error("one group got two values")
	}
	if len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" {
		t.Errorf("generated value is not 64 hex (len %d)", len(v))
	}
	for _, x := range ch {
		if x.Group != rotation.GroupKey(rowKey) {
			t.Errorf("group = %q", x.Group)
		}
		if x.Origin == nil || *x.Origin != (changeset.Origin{Producer: "rotate", Row: rowKey, Letter: "a", Source: changeset.SourceGenerate}) {
			t.Errorf("origin = %+v", x.Origin)
		}
		if x.Place.EnvID != "e1" {
			t.Errorf("change in %s", x.Place.EnvID)
		}
	}
	if f := flashes(ms); len(f) != 1 || f[0].Text != "2 places: new value" {
		t.Errorf("flashes = %+v", f)
	}
	for _, x := range ch {
		put(t, c, x)
	}
	tuitest.NoSecrets(t, frame(c, s), append(secrets, v)...)
	if !strings.Contains(frame(c, s), "✓ 2/2") {
		t.Errorf("decided row state missing:\n%s", frame(c, s))
	}
}

// Two values: one new value per old value, each with its own group and
// letter; the sealed places of a name share one value.
func TestRotateSpaceGeneratesPerGroup(t *testing.T) {
	c := ctx(80, 30)
	c.Settings.WriteEnvs = []string{"e1", "e2"}
	vs := vars()
	for i := range vs {
		// staging's APP_SECRET too keeps a leaked value (a group rotated
		// after the leak is left alone by space).
		if vs[i].EnvID == "e2" && vs[i].Name == "APP_SECRET" {
			vs[i].UpdatedAt = "2026-10-02T10:00:00"
		}
	}
	replaceVars(c.Store, append(vs,
		railway.VarRecord{EnvID: "e1", ServiceID: "api", Name: "COOKIE_PASSWORD", Sealed: true},
		railway.VarRecord{EnvID: "e1", ServiceID: "web", Name: "COOKIE_PASSWORD", Sealed: true},
	))
	s := to(t, c, open(t, c), "APP_SECRET")
	_, ms := press(c, s, space)
	ch := puts(ms)
	if len(ch) != 2 {
		t.Fatalf("puts = %d", len(ch))
	}
	if value(t, ch[0]) == value(t, ch[1]) {
		t.Error("two groups share a value")
	}
	letters := ch[0].Origin.Letter + ch[1].Origin.Letter
	if letters != "ab" || ch[0].Group == ch[1].Group {
		t.Errorf("letters %q groups %q %q", letters, ch[0].Group, ch[1].Group)
	}

	s = to(t, c, open(t, c), "COOKIE_PASSWORD")
	_, ms = press(c, s, space)
	ch = puts(ms)
	if len(ch) != 2 {
		t.Fatalf("sealed puts = %d", len(ch))
	}
	if value(t, ch[0]) != value(t, ch[1]) || len(value(t, ch[0])) != 24 {
		t.Error("sealed places of one name do not share a 24-char value")
	}
	if ch[0].Origin.Letter != "" || ch[0].Group == ch[1].Group {
		t.Errorf("sealed origin %+v groups %q %q", ch[0].Origin, ch[0].Group, ch[1].Group)
	}
}

func TestRotateSpaceReusesValue(t *testing.T) {
	c := ctx(80, 30)
	rowKey := fp(c, "e1/api/JWT_SECRET")
	const planned = "planned-value-0123456789"
	rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", planned, rowKey, "a")
	s := to(t, c, open(t, c), "JWT_SECRET")
	s, ms := press(c, s, space)
	ch := puts(ms)
	if len(ch) != 1 || ch[0].Place != pl("e1", "web") {
		t.Fatalf("puts = %+v", ch)
	}
	if value(t, ch[0]) != planned {
		t.Error("the planned value of the group was not reused")
	}
	tuitest.NoSecrets(t, frame(c, s), append(secrets, planned)...)
}

func TestRotateSpaceUndo(t *testing.T) {
	c := ctx(80, 30)
	rowKey := fp(c, "e1/api/JWT_SECRET")
	a := rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", "planned-value-0123456789", rowKey, "a")
	b := rotateSet(t, c, pl("e1", "web"), "JWT_SECRET", "planned-value-0123456789", rowKey, "a")
	s := to(t, c, open(t, c), "JWT_SECRET")
	_, ms := press(c, s, space)
	gone := removed(ms)
	if len(puts(ms)) != 0 || len(gone) != 2 || !(gone[0] == a.ID && gone[1] == b.ID || gone[0] == b.ID && gone[1] == a.ID) {
		t.Errorf("msgs = %#v", ms)
	}
}

func TestRotateRefusesDB(t *testing.T) {
	c := ctx(80, 30)
	s := open(t, c)
	for label, want := range map[string]string{
		"ADMIN_TOKEN":      "mongo is a database · its password is in the database section",
		"SESSION_PASSWORD": "shop/staging is not in write environments · Settings w",
	} {
		s = to(t, c, open(t, c), label)
		_, ms := press(c, s, space)
		f := flashes(ms)
		if len(puts(ms)) != 0 || len(f) != 1 || f[0].Text != want {
			t.Errorf("%s: msgs = %#v", label, ms)
		}
	}
}

func TestRotateAskOpensPlaces(t *testing.T) {
	c := ctx(80, 30)
	for _, k := range []tea.KeyMsg{space, enter} {
		s := to(t, c, open(t, c), "STRIPE_API_KEY")
		_, ms := press(c, s, k)
		want := fp(c, "e1/api/STRIPE_API_KEY")
		if len(ms) != 1 {
			t.Fatalf("msgs = %#v", ms)
		}
		o, ok := ms[0].(tui.OpenMsg)
		if !ok || o.Name != "rotate.places" || len(o.Args) != 1 || o.Args[0] != want {
			t.Errorf("%s: msg = %#v", k, ms[0])
		}
	}
}

func TestRotateOffByDefaultAsks(t *testing.T) {
	c := ctx(80, 30)
	s := to(t, c, open(t, c), "ENCRYPTION_SECRET_KEY")
	s, ms := press(c, s, space)
	if len(ms) != 0 || !s.(tui.Capturer).Capturing() {
		t.Fatalf("no question: %#v", ms)
	}
	if !strings.Contains(frame(c, s), "data encrypted with the old key becomes unreadable") {
		t.Errorf("warning not asked:\n%s", frame(c, s))
	}
	s, ms = press(c, s, r("n"))
	if len(puts(ms)) != 0 || s.(tui.Capturer).Capturing() {
		t.Errorf("n wrote: %#v", ms)
	}
	s, _ = press(c, s, space)
	_, ms = press(c, s, r("y"))
	if len(puts(ms)) != 1 {
		t.Errorf("y wrote %d changes", len(puts(ms)))
	}
}

func TestRotateHidden(t *testing.T) {
	c := ctx(80, 30)
	s := open(t, c)
	if strings.Contains(frame(c, s), "NUXT_PUBLIC_SITE_KEY") {
		t.Error("a public row shows before h")
	}
	s, _ = press(c, s, r("h"))
	if !strings.Contains(frame(c, s), "NUXT_PUBLIC_SITE_KEY") {
		t.Error("h does not show the public row")
	}
	s, _ = press(c, s, r("h"))
	if strings.Contains(frame(c, s), "NUXT_PUBLIC_SITE_KEY") {
		t.Error("h again does not hide it")
	}
}

func TestRotateFilter(t *testing.T) {
	c := ctx(80, 30)
	s := open(t, c)
	s, _ = press(c, s, r("/"), r("s"), r("t"), r("r"), r("i"))
	got := frame(c, s)
	if !strings.Contains(got, "STRIPE_API_KEY") || strings.Contains(got, "JWT_SECRET") {
		t.Errorf("filter:\n%s", got)
	}
	if rotate.Cursor(c, s) != "STRIPE_API_KEY" {
		t.Errorf("cursor on %q", rotate.Cursor(c, s))
	}
}

func TestRotateApplyPreset(t *testing.T) {
	c := ctx(80, 30)
	s := open(t, c)
	_, ms := press(c, s, r("A"))
	if f := flashes(ms); len(f) != 1 || f[0].Kind != '!' {
		t.Errorf("A with nothing to apply: %#v", ms)
	}
	rowKey := fp(c, "e1/api/JWT_SECRET")
	a := rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", "planned-value-0123456789", rowKey, "a")
	other, err := kinds.SetVariable(c.Store, c.CS.Changes, pl("e1", "web"), "STRIPE_API_KEY", "edited-in-variables-0000", now)
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, other)
	added, err := kinds.SetVariable(c.Store, c.CS.Changes, pl("e1", "web"), "NEW_SECRET", "added-value-000000000000", now)
	if err != nil {
		t.Fatal(err)
	}
	added.Group = rotation.AddGroupKey("NEW_SECRET")
	put(t, c, added)
	_, ms = press(c, s, r("A"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	o, ok := ms[0].(tui.OpenMsg)
	if !ok || o.Name != "changeset" || len(o.Args) != 2 || o.Args[0] != "select" {
		t.Fatalf("msg = %#v", ms[0])
	}
	ids, _ := o.Args[1].([]string)
	if len(ids) != 2 || !(ids[0] == a.ID && ids[1] == added.ID || ids[1] == a.ID && ids[0] == added.ID) {
		t.Errorf("ids = %v", ids)
	}
}

func TestRotateOpensAtFingerprint(t *testing.T) {
	c := ctx(80, 30)
	s := open(t, c, fp(c, "e1/api/STRIPE_API_KEY"))
	if rotate.Cursor(c, s) != "STRIPE_API_KEY" {
		t.Errorf("cursor on %q", rotate.Cursor(c, s))
	}
	if strings.Contains(frame(c, s), "NUXT_PUBLIC_SITE_KEY") {
		t.Error("hidden rows shown for a visible row")
	}

	s = open(t, c, fp(c, "e1/web/NUXT_PUBLIC_SITE_KEY"))
	if rotate.Cursor(c, s) != "NUXT_PUBLIC_SITE_KEY" || !strings.Contains(frame(c, s), "NUXT_PUBLIC_SITE_KEY") {
		t.Errorf("hidden row: cursor on %q", rotate.Cursor(c, s))
	}

	// a value in no row: hidden rows on, cursor at the top, and a flash
	s = rotate.Build(fp(c, "e1/web/USES_JWT"))
	s, ms := settle(c, s, s.Init(c))
	if len(ms) != 1 || ms[0] != (tui.FlashMsg{Kind: '!', Text: "this value is not in a rotation row"}) {
		t.Errorf("no row: msgs = %#v", ms)
	}
	if rotate.Cursor(c, s) != "mongo root" { // the database section comes first
		t.Errorf("no row: cursor on %q, want the top", rotate.Cursor(c, s))
	}
	if !strings.Contains(frame(c, s), "NUXT_PUBLIC_SITE_KEY") {
		t.Error("no row: hidden rows not shown")
	}
	if rotate.Build(42) != nil {
		t.Error("Build(42) should be nil")
	}
}

func TestRotateRefresh(t *testing.T) {
	c := ctx(80, 30)
	_, ms := press(c, open(t, c), r("r"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if _, ok := ms[0].(tui.RefreshMsg); !ok {
		t.Errorf("msg = %#v", ms[0])
	}
}

// The detail panel: action, stats, warning, coverage, places, references.
func TestRotatePanel(t *testing.T) {
	c := ctx(100, 40)
	rowKey := fp(c, "e1/api/JWT_SECRET")
	rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", "planned-value-0123456789", rowKey, "a")
	s := to(t, c, open(t, c), "JWT_SECRET")
	got := frame(c, s)
	for _, want := range []string{
		"◆ GEN  JWT_SECRET · 64 hex, like now",
		"places 3  values 1  rotated 0/3 (after 10-20 14:00)",
		"! logs every user out",
		"JWT_SECRET · 1 of 3 places · 2 keep the old value",
		"→ 24 chars",
		"read only",
		"↳ through references: 1 · redeployed with the new value",
		"USES_JWT ← api.JWT_SECRET",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("panel misses %q:\n%s", want, got)
		}
	}
	for _, pl := range [][2]string{{"production", "api"}, {"staging", "api"}} {
		if !hasPlace(got, pl[0], pl[1]) {
			t.Errorf("panel misses place %s %s:\n%s", pl[0], pl[1], got)
		}
	}
	tuitest.NoSecrets(t, got, append(secrets, "planned-value-0123456789")...)
}

// hasPlace reports whether frame has a line with env and service as
// neighbouring columns.
func hasPlace(frame, env, service string) bool {
	return regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(env) + `\s+` + regexp.QuoteMeta(service) + `(\s|$)`).MatchString(frame)
}

// The keys modal lists every key of the screen.
func TestRotateKeysModal(t *testing.T) {
	c := ctx(80, 30)
	s := open(t, c)
	k := keys.New(s.Title(), s.Keys())
	got := ansi.Strip(k.View(c, 76, 24))
	for _, want := range []string{"enter", "space", "h", "/", "A", "r", "j/k", "pgup/pgdn", "esc"} {
		if !strings.Contains(got, "  "+want+" ") {
			t.Errorf("keys modal misses %q:\n%s", want, got)
		}
	}
}

// Scrolling keeps the cursor's row and its section header on screen.
func TestRotateSectionHeaderPinned(t *testing.T) {
	c := ctx(80, 15)
	s := open(t, c)
	s, _ = press(c, s, r("h"))
	section := map[string]string{
		"ADMIN_TOKEN": "generated", "APP_SECRET": "generated", "ENCRYPTION_SECRET_KEY": "generated",
		"JWT_SECRET": "generated", "SESSION_PASSWORD": "generated",
		"NUXT_PUBLIC_SITE_KEY": "values you paste", "STRIPE_API_KEY": "values you paste",
		"MONGO_INITDB_ROOT_USERNAME": "values you paste",
		"mongo root":                 "database",
	}
	check := func() {
		t.Helper()
		label := rotate.Cursor(c, s)
		body := ansi.Strip(s.View(c, 76, 9))
		list, _, _ := strings.Cut(body, "\n"+strings.Repeat("─", 76))
		if !strings.Contains(list, "── "+section[label]+" (") {
			t.Errorf("cursor on %s: header %q not on screen:\n%s", label, section[label], body)
		}
		short := label[:min(len(label), 10)]
		if !strings.Contains(list, short) {
			t.Errorf("cursor row %s not on screen:\n%s", label, body)
		}
	}
	for range 10 {
		check()
		s, _ = press(c, s, down)
	}
	for range 10 {
		check()
		s, _ = press(c, s, tea.KeyMsg{Type: tea.KeyUp})
	}
}

func TestRotateProblemRefused(t *testing.T) {
	c := ctx(80, 30)
	// two databases share one root password
	replaceVars(c.Store, append(vars(),
		rec("e1", "web", "MONGO_INITDB_ROOT_PASSWORD", rootV, "2026-10-07T10:00:00"),
	))
	s := to(t, c, open(t, c), "mongo root")
	if !strings.Contains(frame(c, s), "✗ cannot rotate") {
		t.Errorf("state:\n%s", frame(c, s))
	}
	_, ms := press(c, s, space)
	if f := flashes(ms); len(f) != 1 || len(puts(ms)) != 0 || f[0].Text != "2 databases share this password: rotate them one at a time" {
		t.Errorf("msgs = %#v", ms)
	}
}

// A second space before the root puts the first batch gives the same
// value: every put of the group carries one value.
func TestRotateSpaceTwiceInFlight(t *testing.T) {
	c := ctx(80, 30)
	s := to(t, c, open(t, c), "JWT_SECRET")
	s, first := press(c, s, space)
	_, second := press(c, s, space)
	all := append(puts(first), puts(second)...)
	if len(all) != 4 {
		t.Fatalf("puts = %d, want 4", len(all))
	}
	v := value(t, all[0])
	for _, ch := range all[1:] {
		if value(t, ch) != v {
			t.Error("one group got two values")
		}
	}
}

// After an undo the row gets a new value, not the forgotten one.
func TestRotateUndoForgetsPlanned(t *testing.T) {
	c := ctx(80, 30)
	s := to(t, c, open(t, c), "JWT_SECRET")
	s, ms := press(c, s, space)
	old := value(t, puts(ms)[0])
	for _, ch := range puts(ms) {
		put(t, c, ch)
	}
	s, ms = press(c, s, space) // undo
	root(t, c, ms)
	_, ms = press(c, s, space)
	if ch := puts(ms); len(ch) != 2 || value(t, ch[0]) == old {
		t.Error("undo kept the planned value")
	}
}

// A refresh that reorders the rows keeps the cursor on its row.
func TestRotateRefreshKeepsCursor(t *testing.T) {
	c := ctx(80, 30)
	s := to(t, c, open(t, c), "JWT_SECRET")
	// AAA_SECRET sorts first and ADMIN_TOKEN goes away
	var vs []railway.VarRecord
	for _, v := range vars() {
		if v.Name != "ADMIN_TOKEN" {
			vs = append(vs, v)
		}
	}
	vs = append(vs, rec("e1", "api", "AAA_PASSWORD", "aaa-password-value-0009", "2026-10-01T09:30:00"),
		rec("e1", "web", "ABC_PASSWORD", "abc-password-value-0010", "2026-10-01T09:30:00"))
	replaceVars(c.Store, vs)
	s.View(c, 76, 24)
	if got := rotate.Cursor(c, s); got != "JWT_SECRET" {
		t.Errorf("cursor on %q after refresh", got)
	}
}

func TestRotateNoteNarrow(t *testing.T) {
	c := ctx(60, 30)
	// one more row, so the hidden ones make the count too long at 60
	replaceVars(c.Store, append(vars(), rec("e1", "api", "EXTRA_SECRET", "extra-secret-value-0123456789", "2026-10-01T09:30:00")))
	s := open(t, c)
	if n := s.(tui.Noter).Note(c); n != "leak 10-20 14:00 · 8 rows" {
		t.Errorf("note at 60 = %q", n)
	}
	s, _ = press(c, s, r("h")) // 10 rows: the count no longer fits
	if n := s.(tui.Noter).Note(c); n != "leak 10-20 14:00" {
		t.Errorf("note at 60 = %q", n)
	}
	c.Settings.Since = ""
	if n := s.(tui.Noter).Note(c); n != "no leak time · 10 rows" {
		t.Errorf("note at 60 without leak = %q", n)
	}
}

// The password rows read the runs index and, for the runs executing here,
// their live state.
func TestRotateRunsWiring(t *testing.T) {
	c := ctx(80, 30)
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: []apply.RunInfo{{ID: "r1", State: apply.RunRunning}, {ID: "r2", State: apply.RunFailed}}}
	c.Running = map[string]tui.RunProgress{"r1": {State: apply.RunRunning}, "r3": {State: apply.RunFinished}, "r4": {}}
	got := rotate.RotateRuns(c)
	if len(got.Runs) != 2 || got.Runs[0].ID != "r1" {
		t.Errorf("runs = %+v", got.Runs)
	}
	if want := map[string]string{"r1": apply.RunRunning, "r3": apply.RunFinished}; !maps.Equal(got.Live, want) {
		t.Errorf("live = %v, want %v", got.Live, want)
	}
}

// A pending set of the group that holds a reference (edited in Variables)
// is no generated value: generating for the group's other places makes a
// new literal one.
func TestRotateSpaceSkipsReferenceValue(t *testing.T) {
	c := ctx(80, 30)
	rowKey := fp(c, "e1/api/JWT_SECRET")
	rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", "${{web.STRIPE_API_KEY}}", rowKey, "a")
	s := to(t, c, open(t, c), "JWT_SECRET")
	_, ms := press(c, s, space)
	ch := puts(ms)
	if len(ch) != 1 || ch[0].Place != pl("e1", "web") {
		t.Fatalf("puts at %v, want e1/web alone", placesOf(ch))
	}
	if v := value(t, ch[0]); len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" {
		t.Errorf("generated value is not 64 hex (len %d)", len(v))
	}
}

// Half applied: api and web of production got a new value after the leak
// (their own group, rotated everywhere), worker keeps the leaked one. Space
// generates for worker alone and points at c; a second space undoes it.
func TestRotateSpaceLeavesRotatedGroup(t *testing.T) {
	const fresh = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	c := ctx(80, 30)
	replaceVars(c.Store, []railway.VarRecord{
		rec("e1", "api", "JWT_SECRET", fresh, "2026-10-21T09:30:00"),
		rec("e1", "web", "JWT_SECRET", fresh, "2026-10-21T09:30:00"),
		rec("e1", "mongo", "JWT_SECRET", jwtV, "2026-10-01T09:30:00"),
	})
	// mongo has no database variables here: it is an ordinary place.
	s := to(t, c, open(t, c), "JWT_SECRET")
	s, ms := press(c, s, space)
	ch := puts(ms)
	if len(ch) != 1 || ch[0].Place != pl("e1", "mongo") {
		t.Fatalf("puts at %v, want e1/mongo alone", placesOf(ch))
	}
	if got := flashText(ms); got != "! some places are already rotated · c in the row copies from them" {
		t.Errorf("flash %q", got)
	}
	tuitest.NoSecrets(t, frame(c, s), append(secrets, fresh, value(t, ch[0]))...)
	root(t, c, ms)
	// The rotated group's lines need nothing: the row is done, not half.
	if f := frame(c, s); !strings.Contains(f, "✓ 3/3") || strings.Contains(f, "◐") {
		t.Errorf("row state is not ✓ 3/3:\n%s", f)
	}
	_, ms = press(c, s, space)
	if len(puts(ms)) != 0 || len(removed(ms)) != 1 || removed(ms)[0] != ch[0].ID {
		t.Errorf("second space: %q puts %d removed %v", flashText(ms), len(puts(ms)), removed(ms))
	}
}

// The row is rotated everywhere after the leak: space leaves it alone.
func TestRotateSpaceRotatedRow(t *testing.T) {
	const fresh = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	c := ctx(80, 30)
	replaceVars(c.Store, []railway.VarRecord{
		rec("e1", "api", "JWT_SECRET", fresh, "2026-10-21T09:30:00"),
		rec("e1", "web", "JWT_SECRET", fresh, "2026-10-21T09:30:00"),
	})
	s := to(t, c, open(t, c), "JWT_SECRET")
	_, ms := press(c, s, space)
	if len(puts(ms)) != 0 || len(removed(ms)) != 0 || flashText(ms) != "! already rotated after the leak · enter shows the places" {
		t.Errorf("space: %q puts %d removed %d", flashText(ms), len(puts(ms)), len(removed(ms)))
	}
}

// placesOf is where changes go, for messages that must not print values.
func placesOf(ch []changeset.Change) []string {
	out := make([]string, len(ch))
	for i, c := range ch {
		out[i] = c.Place.Key()
	}
	return out
}

// doneFlash checks a decision's command is one tui.ChangesMsg whose Done
// is want: the root flashes it only when every intent succeeds, so a
// refusal is never hidden behind it.
func doneFlash(t *testing.T, cmd tea.Cmd, want string) {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	m, ok := cmd().(tui.ChangesMsg)
	if !ok {
		t.Fatal("not one tui.ChangesMsg")
	}
	if len(m.Put)+len(m.Remove) == 0 {
		t.Error("no intents")
	}
	if got := flashText([]tea.Msg{m}); got != want {
		t.Errorf("Done = %q, want %q", got, want)
	}
}

func TestDecisionDoneFlash(t *testing.T) {
	t.Run("space", func(t *testing.T) {
		c := ctx(80, 30)
		s := to(t, c, open(t, c), "JWT_SECRET")
		_, cmd := s.Update(c, space)
		doneFlash(t, cmd, "✓ 2 places: new value")
	})
	t.Run("undo", func(t *testing.T) {
		c := ctx(80, 30)
		rowKey := fp(c, "e1/api/JWT_SECRET")
		rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", "planned-value-0123456789", rowKey, "a")
		rotateSet(t, c, pl("e1", "web"), "JWT_SECRET", "planned-value-0123456789", rowKey, "a")
		s := to(t, c, open(t, c), "JWT_SECRET")
		_, cmd := s.Update(c, space)
		doneFlash(t, cmd, "✓ 2 places: decision undone")
	})
	t.Run("places g", func(t *testing.T) {
		c := pctx(80, 30)
		s := openRow(t, c, "JWT_SECRET")
		s = at(t, c, s, "e1/api/JWT_SECRET")
		_, cmd := s.Update(c, r("g"))
		doneFlash(t, cmd, "✓ 1 place: new value")
	})
	t.Run("add", func(t *testing.T) {
		c := pctx(80, 30)
		s := addTo(t, c, "NEW_SECRET", 1)
		_, cmd := s.Update(c, r("g"))
		doneFlash(t, cmd, "✓ 1 place: NEW_SECRET added")
	})
}

// A decision's intents travel as one tui.ChangesMsg: the root flashes Done
// only when all succeed, so its refusal is never hidden by the screen's ✓.
func TestRotateUndoReferencedShowsRefusal(t *testing.T) {
	c := ctx(80, 30)
	rowKey := fp(c, "e1/api/JWT_SECRET")
	rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", "planned-value-0123456789", rowKey, "a")
	rotateSet(t, c, pl("e1", "web"), "JWT_SECRET", "planned-value-0123456789", rowKey, "a")
	// A change from elsewhere reads the api place's new value.
	ref, err := kinds.SetVariable(c.Store, c.CS.Changes, pl("e1", "web"), "USES_NEW", "${{api.JWT_SECRET}}", now)
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, ref)

	s := to(t, c, open(t, c), "JWT_SECRET")
	_, ms := press(c, s, space)
	if len(ms) != 1 {
		t.Fatalf("want one tui.ChangesMsg and no flash of the screen's own, got %d messages", len(ms))
	}
	if _, ok := ms[0].(tui.ChangesMsg); !ok {
		t.Fatalf("message is %T, want tui.ChangesMsg", ms[0])
	}

	m := rootmodel.New(rootmodel.Deps{
		Store: c.Store, Views: c.Views, Reg: kinds.NewRegistry(), CS: c.CS, Settings: c.Settings,
		Theme: c.Theme, Now: c.Now, ChangeSetPath: filepath.Join(t.TempDir(), "changeset.json"),
	})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.Update(ms[0])
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "✗ JWT_SECRET is referenced by a pending change: remove that first") || strings.Contains(v, "decision undone") {
		t.Errorf("the root's refusal is not shown:\n%s", v)
	}
	tuitest.NoSecrets(t, v, append(secrets, "planned-value-0123456789")...)
}

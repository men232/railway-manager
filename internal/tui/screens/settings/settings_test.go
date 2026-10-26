package settings_test

import (
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/picker"
	ss "github.com/men232/railway-manager/internal/tui/screens/settings"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

const secret = "fixture-value-alpha"

var catalog = railway.Catalog{Projects: []railway.Project{
	{
		ID: "p1", Name: "shop", Workspace: "ws",
		Environments: []railway.Environment{
			{ID: "e1", Name: "production", CanAccess: true},
			{ID: "e2", Name: "staging", CanAccess: true},
		},
	},
	{ID: "p2", Name: "blog", Workspace: "ws", Environments: []railway.Environment{{ID: "e3", Name: "production", CanAccess: true}}},
}}

func ctx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(catalog)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	st.FinishLoad("catalog", now)
	st.FinishLoad("inventory:p1", now)
	st.FailLoad("inventory:p2", "Discover: connection refused")
	c := &tui.Ctx{
		Store: st, Theme: tui.NewTheme(tuitest.Profile()), Account: "fake@example.test", Token: "config · expires 14:05",
		Settings: &settings.Settings{Projects: []string{"p1", "p2"}, WriteEnvs: []string{"e1"}, Icons: "auto"},
		Width:    w, Height: h,
		Now: func() time.Time { return now },
	}
	return c
}

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func msgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	m := cmd()
	if b, ok := m.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			out = append(out, msgs(c)...)
		}
		return out
	}
	return []tea.Msg{m}
}

// open runs the OpenMsg a key press produced and builds the picker like Ctx.Open.
type opened struct {
	title    string
	items    []picker.Item
	selected map[string]bool
	done     func([]string) tea.Msg
}

func openOf(t *testing.T, cmd tea.Cmd) opened {
	t.Helper()
	ms := msgs(cmd)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	om, ok := ms[0].(tui.OpenMsg)
	if !ok || om.Name != "picker" || len(om.Args) != 4 {
		t.Fatalf("msg = %#v", ms[0])
	}
	return opened{om.Args[0].(string), om.Args[1].([]picker.Item), om.Args[2].(map[string]bool), om.Args[3].(func([]string) tea.Msg)}
}

func TestSettingsWriteEnvsStartsFromSaved(t *testing.T) {
	c := ctx(80, 20)
	c.Settings.WriteEnvs = []string{"e1"}
	_, cmd := ss.New().Update(c, r("w"))
	o := openOf(t, cmd)
	if len(o.items) != 3 {
		t.Errorf("items = %v", o.items)
	}
	if !reflect.DeepEqual(o.selected, map[string]bool{"e1": true}) {
		t.Errorf("selected = %v", o.selected)
	}
	// the picker built from those args shows only e1 checked
	p := picker.Build(o.title, o.items, o.selected, o.done)
	view := p.View(c, 76, 10)
	if got := ansi.Strip(view); !strings.Contains(got, "[x] shop/production") || strings.Contains(got, "[x] shop/staging") {
		t.Errorf("view:\n%s", got)
	}
}

func TestSettingsIconsCycle(t *testing.T) {
	c := ctx(80, 20)
	s := ss.New()
	c.NerdDetected = true
	for _, want := range []string{"nerd", "plain", "auto"} {
		var cmd tea.Cmd
		s, cmd = s.Update(c, r("g"))
		if c.Settings.Icons != want {
			t.Errorf("icons = %q, want %q", c.Settings.Icons, want)
		}
		if wantNerd := want != "plain"; c.Icons.Nerd != wantNerd {
			t.Errorf("%s: Nerd = %v", want, c.Icons.Nerd)
		}
		ms := msgs(cmd)
		if len(ms) != 1 {
			t.Fatalf("msgs = %#v", ms)
		}
		if _, ok := ms[0].(tui.SaveSettingsMsg); !ok {
			t.Errorf("msg = %#v", ms[0])
		}
	}
	c.NerdDetected = false
	_, _ = s.Update(c, r("g")) // nerd
	_, _ = s.Update(c, r("g")) // plain
	_, _ = s.Update(c, r("g")) // auto
	if c.Icons.Nerd {
		t.Error("auto should follow NerdDetected=false")
	}
}

func TestSettingsEmptyCatalogRefusesPickers(t *testing.T) {
	c := ctx(80, 20)
	c.Store = store.New()
	c.Settings.Envs = []string{"e2"}
	before := *c.Settings
	for _, k := range []string{"p", "e", "w"} {
		_, cmd := ss.New().Update(c, r(k))
		ms := msgs(cmd)
		f, ok := ms[0].(tui.FlashMsg)
		if len(ms) != 1 || !ok || f.Kind != '!' || f.Text != "nothing to pick yet · r refresh" {
			t.Errorf("%s: msgs = %#v", k, ms)
		}
	}
	if !reflect.DeepEqual(*c.Settings, before) {
		t.Errorf("settings changed: %+v", c.Settings)
	}
}

func TestSettingsStaleWriteEnvSurvivesConfirm(t *testing.T) {
	c := ctx(80, 20)
	c.Settings.WriteEnvs = []string{"e1", "gone"}
	s := ss.New()
	_, cmd := s.Update(c, r("w"))
	o := openOf(t, cmd)
	var stale *picker.Item
	for i := range o.items {
		if o.items[i].ID == "gone" {
			stale = &o.items[i]
		}
	}
	if stale == nil || stale.Label != "gone (not in catalog)" || !o.selected["gone"] {
		t.Fatalf("stale item = %+v selected=%v", stale, o.selected)
	}
	// confirm without changes: the picker hands back every checked id
	_, cmd = picker.Build(o.title, o.items, o.selected, o.done).Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	seq := reflect.ValueOf(cmd()) // Sequence(Pop, onDone)
	done := seq.Index(1).Interface().(tea.Cmd)()
	s.Update(c, done)
	if !reflect.DeepEqual(c.Settings.WriteEnvs, []string{"e1", "gone"}) {
		t.Errorf("write = %v", c.Settings.WriteEnvs)
	}
}

// Account is the last entry: its panel holds the account, the token and
// the last read, whole; enter says how to sign in again.
func TestSettingsAccount(t *testing.T) {
	c := ctx(80, 20)
	c.Account = "someone-with-a-long-address@example.test"
	s, cmd := ss.New().Update(c, r("a"))
	if f, ok := flashOf(msgs(cmd)); !ok || !strings.Contains(f.Text, "railway login") {
		t.Errorf("a: %#v", msgs(cmd))
	}
	v := ansi.Strip(s.View(c, 76, 24))
	for _, want := range []string{"someone-with-a-long-address@example.test", "config · expires 14:05", "read   2026-10-24 12:00"} {
		if !strings.Contains(v, want) {
			t.Errorf("%q missing:\n%s", want, v)
		}
	}
	c.Token, c.Account = "", ""
	if v := ansi.Strip(s.View(c, 76, 24)); !strings.Contains(v, "unknown") || !strings.Contains(v, "not signed in") {
		t.Errorf("unset:\n%s", v)
	}
}

func TestSettingsProjectsDoneSavesAndRefreshes(t *testing.T) {
	c := ctx(80, 20)
	s := ss.New()
	_, cmd := s.Update(c, r("p"))
	o := openOf(t, cmd)
	if len(o.items) != 2 || !o.selected["p1"] || !o.selected["p2"] {
		t.Fatalf("opened = %+v", o)
	}
	_, cmd = s.Update(c, o.done([]string{"p2"}))
	if !reflect.DeepEqual(c.Settings.Projects, []string{"p2"}) {
		t.Errorf("projects = %v", c.Settings.Projects)
	}
	var save, refresh bool
	for _, m := range msgs(cmd) {
		switch m.(type) {
		case tui.SaveSettingsMsg:
			save = true
		case tui.RefreshMsg:
			refresh = true
		}
	}
	if !save || !refresh {
		t.Errorf("save=%v refresh=%v", save, refresh)
	}

	// write envs: save only, no refresh
	_, cmd = s.Update(c, r("w"))
	o = openOf(t, cmd)
	_, cmd = s.Update(c, o.done([]string{"e1", "e2"}))
	ms := msgs(cmd)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if _, ok := ms[0].(tui.SaveSettingsMsg); !ok {
		t.Errorf("msg = %#v", ms[0])
	}
	if !reflect.DeepEqual(c.Settings.WriteEnvs, []string{"e1", "e2"}) {
		t.Errorf("write = %v", c.Settings.WriteEnvs)
	}
}

func TestSettingsGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 24)
		s := ss.New()
		out := tuitest.Plain(tui.Frame(c, s.Title(), "", s.View(c, w-tui.ChromeCols, 24-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
		tuitest.NoSecrets(t, out, secret)
		tuitest.Golden(t, "settings_"+strconv.Itoa(w)+".golden", out)
	}
}

// fixedZone sets time.Local to UTC+2 for the test.
func fixedZone(t *testing.T) {
	t.Helper()
	old := time.Local
	time.Local = time.FixedZone("test", 2*3600)
	t.Cleanup(func() { time.Local = old })
}

// typeIn types v into the open input and submits it, returning the messages.
func typeIn(t *testing.T, c *tui.Ctx, s tui.Screen, v string) (tui.Screen, []tea.Msg) {
	t.Helper()
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyCtrlU})
	if v != "" {
		s, _ = s.Update(c, r(v))
	}
	var cmd tea.Cmd
	s, cmd = s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	var out []tea.Msg
	for _, m := range msgs(cmd) {
		var c2 tea.Cmd
		s, c2 = s.Update(c, m)
		out = append(out, m)
		out = append(out, msgs(c2)...)
	}
	return s, out
}

func flashOf(ms []tea.Msg) (tui.FlashMsg, bool) {
	for _, m := range ms {
		if f, ok := m.(tui.FlashMsg); ok {
			return f, true
		}
	}
	return tui.FlashMsg{}, false
}

func saved(ms []tea.Msg) bool {
	for _, m := range ms {
		if _, ok := m.(tui.SaveSettingsMsg); ok {
			return true
		}
	}
	return false
}

func TestSettingsLeakTime(t *testing.T) {
	fixedZone(t)
	c := ctx(80, 20)
	var s tui.Screen = ss.New()
	if v := ansi.Strip(s.View(c, 76, 24)); !strings.Contains(v, "Leak time") || !strings.Contains(v, "—") {
		t.Errorf("unset view:\n%s", v)
	}
	s, _ = s.Update(c, r("l"))
	if !s.(tui.Capturer).Capturing() {
		t.Error("input should capture keys")
	}
	s, ms := typeIn(t, c, s, "2026-10-20 14:30")
	if !saved(ms) || c.Settings.Since != "2026-10-20T12:30:00Z" {
		t.Errorf("since = %q msgs=%#v", c.Settings.Since, ms)
	}
	if v := ansi.Strip(s.View(c, 76, 24)); !strings.Contains(v, "2026-10-20 14:30") || !strings.Contains(v, "10-20 14:30") {
		t.Errorf("set view:\n%s", v)
	}
	// reopening prefills the local value
	s, _ = s.Update(c, r("l"))
	if v := ansi.Strip(s.View(c, 76, 24)); !strings.Contains(v, "2026-10-20 14:30█") {
		t.Errorf("prefill:\n%s", v)
	}
	// bad input flashes and keeps the value
	s, ms = typeIn(t, c, s, "yesterday")
	f, ok := flashOf(ms)
	if !ok || f.Kind != '✗' || f.Text != "leak time is YYYY-MM-DD HH:MM" || saved(ms) {
		t.Errorf("msgs = %#v", ms)
	}
	if c.Settings.Since != "2026-10-20T12:30:00Z" {
		t.Errorf("since changed: %q", c.Settings.Since)
	}
	// empty clears
	s, ms = typeIn(t, c, s, "")
	if !saved(ms) || c.Settings.Since != "" {
		t.Errorf("since = %q msgs=%#v", c.Settings.Since, ms)
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("input should be closed")
	}
	// esc cancels without saving
	s, _ = s.Update(c, r("l"))
	_, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEsc})
	s2, _ := s.Update(c, msgs(cmd)[0])
	if s2.(tui.Capturer).Capturing() || c.Settings.Since != "" {
		t.Error("esc should cancel")
	}
}

func TestSettingsInternalNames(t *testing.T) {
	c := ctx(80, 20)
	var s tui.Screen = ss.New()
	s, _ = s.Update(c, r("i"))
	s, ms := typeIn(t, c, s, " redis_url, db_host ,DB_HOST,,api_key")
	if !saved(ms) || !reflect.DeepEqual(c.Settings.Internal, []string{"API_KEY", "DB_HOST", "REDIS_URL"}) {
		t.Errorf("internal = %v msgs=%#v", c.Settings.Internal, ms)
	}
	if v := ansi.Strip(s.View(c, 76, 24)); !strings.Contains(v, "# API_KEY  # DB_HOST  # REDIS_URL") {
		t.Errorf("view:\n%s", v)
	}
	s, _ = s.Update(c, r("i"))
	s, ms = typeIn(t, c, s, "OK_ONE, 1bad")
	f, ok := flashOf(ms)
	if !ok || f.Kind != '✗' || f.Text != "1BAD is not a variable name" || saved(ms) {
		t.Errorf("msgs = %#v", ms)
	}
	if !reflect.DeepEqual(c.Settings.Internal, []string{"API_KEY", "DB_HOST", "REDIS_URL"}) {
		t.Errorf("internal changed: %v", c.Settings.Internal)
	}
	_, ms = typeIn(t, c, s, "")
	if !saved(ms) || len(c.Settings.Internal) != 0 {
		t.Errorf("internal = %v", c.Settings.Internal)
	}
}

// manyEnvs is a catalog with a project of many environments.
func manyEnvs(c *tui.Ctx) []string {
	var envs []railway.Environment
	var ids []string
	for _, n := range []string{"dev", "preview-1", "preview-2", "preview-3", "qa", "staging-eu", "staging-us", "load-test"} {
		envs = append(envs, railway.Environment{ID: "x-" + n, Name: n, CanAccess: true})
		ids = append(ids, "x-"+n)
	}
	cat := railway.Catalog{Projects: append(slices.Clone(catalog.Projects), railway.Project{ID: "p3", Name: "acme", Workspace: "ws", Environments: envs})}
	c.Store.ReplaceCatalog(cat)
	c.Settings.Projects = append(c.Settings.Projects, "p3")
	return ids
}

// Many write environments list by project in the panel, wrapped under the
// first, and none is cut off.
func TestSettingsWriteEnvsWrap(t *testing.T) {
	c := ctx(80, 30)
	c.Settings.WriteEnvs = append([]string{"e1", "e2"}, manyEnvs(c)...)
	s := ss.New()
	s, _ = s.Update(c, r("w"))
	out := ansi.Strip(s.View(c, 76, 26))
	lines := strings.Split(out, "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "shop  ● production  ● staging") })
	if at < 0 {
		t.Fatalf("no shop line:\n%s", out)
	}
	acme := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "acme  ● dev") })
	if acme < 0 || !strings.Contains(lines[acme+1], "      ● ") {
		t.Errorf("acme does not wrap under its first environment:\n%s", out)
	}
	for _, n := range []string{"preview-3", "staging-us", "load-test", "10 environments in 2 projects"} {
		if !strings.Contains(out, n) {
			t.Errorf("%s missing:\n%s", n, out)
		}
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > 76 {
			t.Errorf("line %q is %d wide", l, ansi.StringWidth(l))
		}
	}
}

// The arrows move the cursor; enter changes the selected setting.
func TestSettingsCursor(t *testing.T) {
	c := ctx(80, 30)
	s := ss.New()
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyDown})
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyDown})
	if l := lineWith(ansi.Strip(s.View(c, 76, 26)), "▌"); !strings.Contains(l, "Write envs") {
		t.Errorf("cursor line %q, want write envs", l)
	}
	_, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	if ms := msgs(cmd); len(ms) != 1 || ms[0].(tui.OpenMsg).Args[0] != "Write environments" {
		t.Errorf("enter on write envs: %#v", ms)
	}
	// a letter moves the cursor too
	s, _ = s.Update(c, r("l"))
	if l := lineWith(ansi.Strip(s.View(c, 76, 26)), "▌"); !strings.Contains(l, "Leak time") {
		t.Errorf("after l: %q", l)
	}
}

// On a short screen the list scrolls to keep the selected setting shown.
func TestSettingsScrolls(t *testing.T) {
	c := ctx(80, 12)
	c.Settings.WriteEnvs = manyEnvs(c)
	s := ss.New()
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyEnd})
	out := ansi.Strip(s.View(c, 76, 8))
	if n := len(strings.Split(out, "\n")); n > 8 {
		t.Errorf("%d lines in 8", n)
	}
	if l := lineWith(out, "▌"); !strings.Contains(l, "Account") {
		t.Errorf("selected icons not shown:\n%s", out)
	}
}

func lineWith(out, sub string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

// A full Settings frame: many environments, internal names and a leak
// time, as the README shows it.
func TestSettingsFullGolden(t *testing.T) {
	c := ctx(100, 26)
	c.Settings.WriteEnvs = append([]string{"e1", "e2", "e3"}, manyEnvs(c)...)
	c.Settings.Internal = []string{"API_KEY", "DB_HOST", "REDIS_URL", "INTERNAL_TOKEN", "SIGNING_SALT", "WEBHOOK_SECRET"}
	c.Settings.Since = "2026-10-20T12:30:00Z"
	s := ss.New()
	s, _ = s.Update(c, r("w"))
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyEsc}) // the picker opened by w is the root's; the cursor stays
	out := tuitest.Plain(tui.Frame(c, s.Title(), "", s.View(c, 100-tui.ChromeCols, 26-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
	tuitest.Golden(t, "settings_full_100.golden", out)
}

// An environment several projects have is in the same column in each of
// them; a project's own follow.
func TestSettingsEnvsAligned(t *testing.T) {
	c := ctx(100, 30)
	env := func(id, name string) railway.Environment {
		return railway.Environment{ID: id, Name: name, CanAccess: true}
	}
	c.Store.ReplaceCatalog(railway.Catalog{Projects: []railway.Project{
		{ID: "p1", Name: "shop", Workspace: "ws", Environments: []railway.Environment{env("a1", "production"), env("a2", "staging")}},
		{ID: "p2", Name: "blog", Workspace: "ws", Environments: []railway.Environment{env("b1", "staging")}},
		{ID: "p3", Name: "acme", Workspace: "ws", Environments: []railway.Environment{env("c1", "dev"), env("c2", "production")}},
	}})
	c.Settings.Projects = []string{"p1", "p2", "p3"}
	c.Settings.WriteEnvs = []string{"a1", "a2", "b1", "c1", "c2"}
	s, _ := ss.New().Update(c, r("w"))
	out := ansi.Strip(s.View(c, 96, 26))
	col := func(project, env string) int {
		l := lineWith(out, project+"  ")
		i := strings.Index(l, "● "+env)
		if i < 0 {
			return -1
		}
		return ansi.StringWidth(l[:i]) // the screen column, not the byte

	}
	if p := col("shop", "production"); p < 0 || col("acme", "production") != p {
		t.Errorf("production not aligned:\n%s", out)
	}
	if st := col("shop", "staging"); st < 0 || col("blog", "staging") != st {
		t.Errorf("staging not aligned:\n%s", out)
	}
	if col("acme", "dev") <= col("acme", "staging") && col("acme", "dev") <= col("acme", "production") {
		t.Errorf("acme's own dev should follow the shared columns:\n%s", out)
	}
}

// Projects are listed under their workspace, each workspace named once.
func TestSettingsProjectsByWorkspace(t *testing.T) {
	c := ctx(100, 30)
	cat := slices.Clone(catalog.Projects)
	cat = append(cat, railway.Project{ID: "p3", Name: "acme", Workspace: "other"})
	c.Store.ReplaceCatalog(railway.Catalog{Projects: cat})
	c.Settings.Projects = []string{"p1", "p3", "p2"}
	out := ansi.Strip(ss.New().View(c, 96, 26))
	ends := func(suffix string) int {
		n := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.HasSuffix(strings.TrimRight(l, " "), suffix) {
				n++
			}
		}
		return n
	}
	if ends("│ ws") != 1 || ends("│ other") != 1 {
		t.Errorf("workspaces not named once:\n%s", out)
	}
	ws, shop, blog := strings.Index(out, "│ ws"), strings.Index(out, "▪ shop"), strings.Index(out, "▪ blog")
	if !(ws < shop && ws < blog) || strings.Index(out, "│ other") > strings.Index(out, "▪ acme") {
		t.Errorf("projects not under their workspace:\n%s", out)
	}
}

// A leak time that is not RFC 3339 (settings.json edited by hand) is shown
// as written, short or not, and never cut mid-character.
func TestSettingsHandWrittenLeakTime(t *testing.T) {
	for _, since := range []string{"oops", "léak-time-by-hand"} {
		c := ctx(80, 20)
		c.Settings.Since = since
		v := ansi.Strip(ss.New().View(c, 76, 24))
		if !strings.Contains(v, since[:4]) || !utf8.ValidString(v) {
			t.Errorf("since %q not shown whole-character:\n%s", since, v)
		}
	}
}

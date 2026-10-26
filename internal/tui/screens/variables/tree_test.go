package variables_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/variables"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

// Long values (20+ characters) group across names by value.
const (
	valA = "fixture-tree-value-alpha"
	valB = "fixture-tree-value-bravo"
	valC = "fixture-tree-value-charlie"
)

var (
	up    = tea.KeyMsg{Type: tea.KeyUp}
	left  = tea.KeyMsg{Type: tea.KeyLeft}
	right = tea.KeyMsg{Type: tea.KeyRight}
	esc   = tea.KeyMsg{Type: tea.KeyEsc}
)

// treeCatalog: project acme, environments production (e1) and staging (e2),
// both with api (s1) and web (s2).
var treeCatalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "acme", Workspace: "ws",
	Environments: []railway.Environment{
		{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"s1", "s2"}},
		{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2"}},
	},
	Services: []railway.Service{{ID: "s1", Name: "api"}, {ID: "s2", Name: "web"}},
}}}

func pl(env, svc string) domain.Place {
	return domain.Place{ProjectID: "p1", EnvID: env, ServiceID: svc}
}

// treeVars by name: API_KEY ×3 (2 values), DEBUG, SIGNING (sealed), TOKEN.
// By value: valA holds production·api API_KEY and TOKEN, production·web
// API_KEY.
func treeVars() []railway.VarRecord {
	return []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "API_KEY", Unrendered: valA, Rendered: valA, UpdatedAt: "2026-10-01T09:30:00"},
		{EnvID: "e1", ServiceID: "s2", Name: "API_KEY", Unrendered: valA, Rendered: valA, UpdatedAt: "2026-10-02T10:15:00"},
		{EnvID: "e2", ServiceID: "s1", Name: "API_KEY", Unrendered: valB, Rendered: valB, UpdatedAt: "2026-10-03T11:45:00"},
		{EnvID: "e1", ServiceID: "s1", Name: "TOKEN", Unrendered: valA, Rendered: valA, UpdatedAt: "2026-10-04T08:00:00"},
		{EnvID: "e2", ServiceID: "s2", Name: "DEBUG", Unrendered: valC, Rendered: valC, UpdatedAt: "2026-10-05T08:00:00"},
		{EnvID: "e1", ServiceID: "s2", Name: "SIGNING", Sealed: true, UpdatedAt: "2026-10-06T08:00:00"},
	}
}

func treeCtx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(treeCatalog)
	st.ReplaceVariables("p1", treeVars())
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views:    &view.Views{Store: st, Reg: kinds.NewRegistry()},
		Settings: &settings.Settings{Projects: []string{"p1"}, Envs: []string{"e1", "e2"}, WriteEnvs: []string{"e1", "e2"}},
		Theme:    tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Now: func() time.Time { return now },
	}
}

func treeFrame(c *tui.Ctx, s tui.Screen) string {
	note := ""
	if n, ok := s.(tui.Noter); ok {
		note = n.Note(c)
	}
	return tuitest.Plain(tui.Frame(c, s.Title(), note, s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
}

func noteOf(c *tui.Ctx, s tui.Screen) string { return s.(tui.Noter).Note(c) }

// cursorPlace identifies the cursor's place by the delete x would put.
func cursorPlace(t *testing.T, c *tui.Ctx, s tui.Screen) (domain.Place, string) {
	t.Helper()
	_, ms := press(c, s, r("x"))
	if len(ms) != 1 {
		t.Fatalf("x msgs = %#v", ms)
	}
	pm, ok := ms[0].(tui.PutChangeMsg)
	if !ok || pm.Change.Kind != "variable.delete" {
		t.Fatalf("x msg = %#v", ms[0])
	}
	return pm.Change.Place, pm.Change.Target
}

func puts(t *testing.T, ms []tea.Msg) []changeset.Change {
	t.Helper()
	var out []changeset.Change
	for _, m := range ms {
		pm, ok := m.(tui.PutChangeMsg)
		if !ok {
			t.Fatalf("msg = %#v", m)
		}
		out = append(out, pm.Change)
	}
	return out
}

func TestTreeScreenByNameAndValue(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	if n := noteOf(c, s); n != "by name · 4 groups · 1 diverged" {
		t.Errorf("note = %q", n)
	}
	out := body(c, s)
	if !strings.Contains(out, "▸ API_KEY") || !strings.Contains(out, "×3") || !strings.Contains(out, "2 values") {
		t.Errorf("by name:\n%s", out)
	}
	// unfold API_KEY, go to its second place (production · web)
	s, _ = press(c, s, enter, down, down)
	if out := body(c, s); !strings.Contains(out, "▾ API_KEY") || !strings.Contains(out, "production · web") {
		t.Fatalf("unfolded:\n%s", out)
	}
	if p, n := cursorPlace(t, c, s); p != pl("e1", "s2") || n != "API_KEY" {
		t.Fatalf("cursor = %v %s", p, n)
	}
	s, _ = press(c, s, r("g"))
	if n := noteOf(c, s); !strings.HasPrefix(n, "by value · ") {
		t.Errorf("note = %q", n)
	}
	if out := body(c, s); !strings.Contains(out, "API_KEY = TOKEN") {
		t.Errorf("by value:\n%s", out)
	}
	if p, n := cursorPlace(t, c, s); p != pl("e1", "s2") || n != "API_KEY" {
		t.Errorf("after g the cursor = %v %s", p, n)
	}
	// and back: still on its place
	s, _ = press(c, s, r("g"))
	if p, n := cursorPlace(t, c, s); p != pl("e1", "s2") || n != "API_KEY" {
		t.Errorf("after g g the cursor = %v %s", p, n)
	}
}

func TestTreeFoldKeptAcrossRefreshAndFilter(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter, down, down, down) // staging · api
	if p, _ := cursorPlace(t, c, s); p != pl("e2", "s1") {
		t.Fatalf("cursor = %v", p)
	}
	// a refresh: one variable changed elsewhere, one new
	vars := treeVars()
	vars[4].Unrendered, vars[4].Rendered = valB, valB
	vars = append(vars, railway.VarRecord{EnvID: "e1", ServiceID: "s1", Name: "AAA_FIRST", Unrendered: "x", Rendered: "x"})
	c.Store.ReplaceVariables("p1", vars)
	s, _ = s.Update(c, loaders.InventoryMsg{ProjectID: "p1"})
	if out := body(c, s); !strings.Contains(out, "▾ API_KEY") {
		t.Errorf("fold lost on refresh:\n%s", out)
	}
	if p, _ := cursorPlace(t, c, s); p != pl("e2", "s1") {
		t.Errorf("cursor after refresh = %v", p)
	}
	s, _ = press(c, s, r("/"), r("staging"), enter)
	if out := body(c, s); !strings.Contains(out, "▾ API_KEY") || strings.Contains(out, "production · web") {
		t.Errorf("filtered:\n%s", out)
	}
	if p, _ := cursorPlace(t, c, s); p != pl("e2", "s1") {
		t.Errorf("cursor after filter = %v", p)
	}
	s, _ = press(c, s, r("/"), esc)
	if out := body(c, s); !strings.Contains(out, "▾ API_KEY") || !strings.Contains(out, "production · web") {
		t.Errorf("filter cleared:\n%s", out)
	}
	if p, _ := cursorPlace(t, c, s); p != pl("e2", "s1") {
		t.Errorf("cursor after clearing = %v", p)
	}
	// ← folds, the cursor goes to the group; the place's own place is gone
	s, _ = press(c, s, left)
	if out := body(c, s); !strings.Contains(out, "▸ API_KEY") || strings.Contains(out, "staging · api") {
		t.Errorf("folded:\n%s", out)
	}
	_, ms := press(c, s, r("x"))
	if len(ms) != 0 || !s.(tui.Capturer).Capturing() {
		t.Errorf("x on the folded group should ask: %#v", ms)
	}
}

func TestTreeRevealEndsOnMoveFilterRefreshSuspend(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	// v on a group line does nothing
	s, ms := press(c, s, r("v"))
	if len(ms) != 0 || strings.Contains(body(c, s), valA) {
		t.Fatalf("v on a group: %#v", ms)
	}
	s, _ = press(c, s, right, down) // production · api
	reveal := func(why string) {
		t.Helper()
		var ms []tea.Msg
		s, ms = press(c, s, r("v"))
		out := body(c, s)
		if len(ms) != 0 || !strings.Contains(out, valA) {
			t.Fatalf("%s: reveal missing (msgs %#v)", why, ms)
		}
		if strings.Count(out, "fixture-tree-value") != 1 {
			t.Fatalf("%s: more than the cursor's place is revealed", why)
		}
	}
	hidden := func(why string) {
		t.Helper()
		if out := body(c, s); strings.Contains(out, "fixture-tree-value") {
			t.Errorf("%s: value still shown", why)
		}
	}
	reveal("first")
	s, _ = press(c, s, down, up)
	hidden("move")
	reveal("filter")
	s, _ = press(c, s, r("/"), r("A"))
	hidden("filter")
	s, _ = press(c, s, enter)
	reveal("refresh")
	vars := treeVars()
	vars[4].Unrendered, vars[4].Rendered = valB, valB
	c.Store.ReplaceVariables("p1", vars)
	s, _ = s.Update(c, loaders.InventoryMsg{ProjectID: "p1"})
	hidden("refresh")
	reveal("grouping")
	s, _ = press(c, s, r("g"))
	hidden("grouping")
	s, _ = press(c, s, r("g"))
	reveal("fold")
	s, _ = press(c, s, left)
	hidden("fold")
	s, _ = press(c, s, right, down)
	reveal("suspend")
	s.(tui.Suspender).Suspend(c)
	hidden("suspend")
}

func TestTreeGroupEditPutsOnePerPlace(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, r("e"))
	if !s.(tui.Capturer).Capturing() {
		t.Fatal("e on a group should open the value input")
	}
	if out := treeFrame(c, s); !strings.Contains(out, "3 places") {
		t.Errorf("prompt should count the places:\n%s", out)
	}
	_, ms := press(c, s, r(newVal), enter, r(newVal), enter)
	cs := puts(t, ms)
	var got []string
	for _, ch := range cs {
		if ch.Kind != "variable.set" || ch.Target != "API_KEY" {
			t.Errorf("change = %+v", ch)
		}
		got = append(got, ch.Place.Key())
	}
	want := []string{pl("e1", "s1").Key(), pl("e1", "s2").Key(), pl("e2", "s1").Key()}
	if !slices.Equal(got, want) {
		t.Errorf("places = %v, want %v", got, want)
	}
}

func TestTreeGroupEditByValueKeepsNames(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	_, ms := press(c, s, r("g"), r("e"), r(newVal), enter, r(newVal), enter)
	cs := puts(t, ms)
	var got []string
	for _, ch := range cs {
		got = append(got, ch.Place.Key()+" "+ch.Target)
	}
	want := []string{pl("e1", "s1").Key() + " API_KEY", pl("e1", "s1").Key() + " TOKEN", pl("e1", "s2").Key() + " API_KEY"}
	if !slices.Equal(got, want) {
		t.Errorf("changes = %v, want %v", got, want)
	}
}

func TestTreeGroupDeleteConfirms(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, ms := press(c, s, r("x"))
	if len(ms) != 0 || !s.(tui.Capturer).Capturing() {
		t.Fatalf("x on a group should ask first: %#v", ms)
	}
	if out := body(c, s); !strings.Contains(out, "delete 3 places?") {
		t.Errorf("confirm missing:\n%s", out)
	}
	s, ms = press(c, s, r("n"))
	if len(ms) != 0 || s.(tui.Capturer).Capturing() {
		t.Fatalf("n should cancel: %#v", ms)
	}
	s, _ = press(c, s, r("x"))
	_, ms = press(c, s, r("y"))
	cs := puts(t, ms)
	if len(cs) != 3 {
		t.Fatalf("changes = %+v", cs)
	}
	for _, ch := range cs {
		if ch.Kind != "variable.delete" || ch.Target != "API_KEY" {
			t.Errorf("change = %+v", ch)
		}
	}
}

func TestTreeGroupUndoRemovesAll(t *testing.T) {
	c := treeCtx(100, 24)
	for _, p := range []domain.Place{pl("e1", "s1"), pl("e2", "s1")} {
		ch, err := kinds.SetVariable(c.Store, nil, p, "API_KEY", newVal, now)
		if err != nil {
			t.Fatal(err)
		}
		c.CS.Put(ch, nil)
	}
	s := variables.Build()
	s.Init(c)
	if out := body(c, s); !strings.Contains(out, "● 2") {
		t.Errorf("pending count missing:\n%s", out)
	}
	_, ms := press(c, s, r("u"))
	if len(ms) != 2 {
		t.Fatalf("msgs = %#v", ms)
	}
	for i, m := range ms {
		if rm, ok := m.(tui.RemoveChangeMsg); !ok || rm.ID != c.CS.Changes[i].ID {
			t.Errorf("msg = %#v", m)
		}
	}
}

func TestTreeRefusesOutsideWriteEnvs(t *testing.T) {
	c := treeCtx(100, 24)
	c.Settings.WriteEnvs = []string{"e1"}
	refusal := "acme/staging is not in write environments · Settings w"
	for _, k := range []string{"e", "x"} {
		s := variables.Build()
		s.Init(c)
		s, ms := press(c, s, r(k)) // API_KEY spans production and staging
		if f := flashOf(t, ms); f.Kind != '✗' || f.Text != refusal {
			t.Errorf("group %s: flash = %+v", k, f)
		}
		if s.(tui.Capturer).Capturing() {
			t.Errorf("group %s opened an input", k)
		}
	}
	// a production place may be edited, the staging one not
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter, down)
	s2, ms := press(c, s, r("e"))
	if len(ms) != 0 || !s2.(tui.Capturer).Capturing() {
		t.Errorf("production edit refused: %#v", ms)
	}
	s, _ = press(c, s, esc)
	s = variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter, down, down, down) // staging · api
	for _, k := range []string{"e", "x"} {
		s2, ms := press(c, s, r(k))
		if f := flashOf(t, ms); f.Kind != '✗' || f.Text != refusal {
			t.Errorf("staging %s: flash = %+v", k, f)
		}
		if s2.(tui.Capturer).Capturing() {
			t.Errorf("staging %s opened an input", k)
		}
	}
	// adding DEBUG (staging · web) to a staging place is refused at the pick
	s = variables.Build()
	s.Init(c)
	s, _ = press(c, s, down, r("a"))                   // DEBUG
	s, ms = press(c, s, down, down, down, down, enter) // staging · api
	if f := flashOf(t, ms); f.Kind != '✗' || f.Text != refusal {
		t.Errorf("add pick: flash = %+v", f)
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("refused pick left an input open")
	}
	// the scoped view refuses too
	_, ms = press(c, variables.New(pl("e2", "s1")), r("e"))
	if f := flashOf(t, ms); f.Kind != '✗' || f.Text != refusal {
		t.Errorf("scoped: flash = %+v", f)
	}
}

func TestTreeAddToAnotherPlace(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, down, r("a")) // DEBUG, at staging · web only
	if !s.(tui.Capturer).Capturing() {
		t.Fatal("a should open the place picker")
	}
	out := body(c, s)
	for _, want := range []string{"add DEBUG to", "production · shared", "production · api", "production · web", "staging · shared", "staging · api"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "staging · web") {
		t.Errorf("picker lists the place holding DEBUG:\n%s", out)
	}
	s, ms := press(c, s, down, down, enter) // production · web
	if len(ms) != 0 || !s.(tui.Capturer).Capturing() {
		t.Fatalf("pick: %#v", ms)
	}
	_, ms = press(c, s, r(newVal), enter, r(newVal), enter)
	cs := puts(t, ms)
	if len(cs) != 1 || cs[0].Place != pl("e1", "s2") || cs[0].Target != "DEBUG" || !cs[0].Base.Absent || cs[0].Kind != "variable.set" {
		t.Errorf("changes = %+v", cs)
	}
	// esc cancels the picker
	s = variables.Build()
	s.Init(c)
	s, _ = press(c, s, down, r("a"), esc)
	if s.(tui.Capturer).Capturing() {
		t.Error("esc should close the picker")
	}
}

func TestTreeHistoryKeyOpensHistory(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	_, ms := press(c, s, down, r("d"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if om, ok := ms[0].(tui.OpenMsg); !ok || om.Name != "history" || len(om.Args) != 1 || om.Args[0] != "DEBUG" {
		t.Errorf("msg = %#v", ms[0])
	}
}

func TestTreeScopedIsFlat(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build(pl("e1", "s1"))
	s.Init(c)
	out := body(c, s)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "API_KEY") || !strings.Contains(lines[2], "TOKEN") {
		t.Errorf("scoped:\n%s", out)
	}
	if strings.ContainsAny(out, "▸▾×") {
		t.Errorf("scoped view shows groups:\n%s", out)
	}
	if n := noteOf(c, s); n != "" {
		t.Errorf("note = %q", n)
	}
	// g does nothing; x deletes the cursor's variable at once
	s, _ = press(c, s, r("g"))
	if p, n := cursorPlace(t, c, s); p != pl("e1", "s1") || n != "API_KEY" {
		t.Errorf("x = %v %s", p, n)
	}
	var keys []string
	for _, h := range s.Keys() {
		keys = append(keys, h.Key)
	}
	for _, k := range []string{"e", "a", "x", "u", "v", "/"} {
		if !slices.Contains(keys, k) {
			t.Errorf("hint %s missing: %v", k, keys)
		}
	}
	if slices.Contains(keys, "g") {
		t.Errorf("scoped view hints g: %v", keys)
	}
}

// runCmd runs cmd and returns its messages, flattening batches.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	switch m := cmd().(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range m {
			out = append(out, runCmd(c)...)
		}
		return out
	case nil:
		return nil
	default:
		return []tea.Msg{m}
	}
}

// reads lists the history and members reads among msgs: "h:<env>" and
// "m:<project>".
func reads(msgs []tea.Msg) []string {
	var out []string
	for _, m := range msgs {
		switch v := m.(type) {
		case loaders.HistoryMsg:
			out = append(out, "h:"+v.EnvID)
		case loaders.MembersMsg:
			out = append(out, "m:"+v.ProjectID)
		}
	}
	slices.Sort(out)
	return out
}

func TestTreeReadsHistoryOncePerEnv(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", []railway.HistoryEntry{{
		ID: "h1", Object: "Variable", Action: "updated", Actor: "u1",
		ServiceIDs: []string{"s1"}, Names: []string{"API_KEY"}, CreatedAt: now.Add(-time.Hour),
	}})
	f.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Alice"}})
	c := treeCtx(100, 24)
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
	// only API_KEY (production ×2, staging) and TOKEN (production)
	vars := treeVars()[:4]
	c.Store.ReplaceVariables("p1", vars)

	s := variables.Build()
	msgs := runCmd(s.Init(c))
	if got := reads(msgs); !slices.Equal(got, []string{"h:e1", "m:p1"}) {
		t.Fatalf("Init reads = %v (staging is not shown yet)", got)
	}
	// a key while the reads are in flight reads nothing again
	s, cmd := s.Update(c, down)
	if got := reads(runCmd(cmd)); len(got) != 0 {
		t.Errorf("second update reads = %v", got)
	}
	// unfolding API_KEY shows a staging place: its history only
	s, _ = press(c, s, up)
	s, cmd = s.Update(c, enter)
	if got := reads(runCmd(cmd)); !slices.Equal(got, []string{"h:e2"}) {
		t.Errorf("unfold reads = %v", got)
	}
	// the answers land (the root applies them, then forwards)
	for _, m := range msgs {
		if u, ok := m.(store.Update); ok {
			u.ApplyTo(c.Store)
		}
		var cmd tea.Cmd
		s, cmd = s.Update(c, m)
		if got := reads(runCmd(cmd)); len(got) != 0 {
			t.Errorf("after %T reads = %v", m, got)
		}
	}
	out := body(c, s)
	if l := lineWith(out, "production · api"); !strings.Contains(l, "by Alice") {
		t.Errorf("author missing:\n%s", out)
	}
	if l := lineWith(out, "staging · api"); !strings.Contains(l, "by …") {
		t.Errorf("unread staging should show …:\n%s", out)
	}
	if l := lineWith(out, "TOKEN"); !strings.Contains(l, "by ?") {
		t.Errorf("no matching entry should show ?:\n%s", out)
	}
	// the scoped view reads its environment
	sc := variables.Build(pl("e2", "s1"))
	if got := reads(runCmd(sc.Init(c))); !slices.Equal(got, []string{"h:e2"}) {
		t.Errorf("scoped Init reads = %v", got)
	}
}

// A place whose history was read with no matching entry shows "?"; author
// and env strings are sanitized.
func TestTreeSanitizesAuthor(t *testing.T) {
	c := treeCtx(100, 24)
	c.Store.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{{
		ID: "h1", Object: "Variable", Action: "updated", Actor: "u1",
		ServiceIDs: []string{"s1"}, Names: []string{"TOKEN"}, CreatedAt: now,
	}}})
	c.Store.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Ev\x1b[31mil"}})
	s := variables.Build()
	s.Init(c)
	out := s.View(c, 96, 20)
	if strings.Contains(out, "\x1b[31m") {
		t.Errorf("escape reached the frame: %q", out)
	}
	if b := ansi.Strip(out); !strings.Contains(b, "by ?") {
		t.Errorf("unmatched place should show ?:\n%s", b)
	}
}

// Short of room, who changed a variable goes before when.
func TestTreeNarrowDropsByBeforeChanged(t *testing.T) {
	c := treeCtx(64, 24)
	ch, err := kinds.SetVariable(c.Store, nil, pl("e1", "s1"), "API_KEY", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(ch, nil)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter)
	l := placeLineWith(body(c, s), "production · api")
	if !strings.Contains(l, "changed 10-01") || strings.Contains(l, "by ") || !strings.Contains(l, "→ 11 chars") {
		t.Errorf("want the date and the overlay without by: %q", l)
	}
}

func TestTreeNarrowKeepsMarks(t *testing.T) {
	c := treeCtx(40, 24)
	ch, err := kinds.SetVariable(c.Store, nil, pl("e1", "s1"), "API_KEY", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(ch, nil)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter)
	out := body(c, s)
	if l := lineWith(out, "API_KEY"); !strings.HasSuffix(strings.TrimRight(l, " "), "● 1") {
		t.Errorf("group line lost its pending mark: %q", l)
	}
	if l := placeLineWith(out, "produc"); !strings.Contains(l, " chars") {
		t.Errorf("place line lost its overlay: %q", l)
	}
	for _, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > c.Width-tui.ChromeCols {
			t.Errorf("line %q is %d wide", l, w)
		}
	}
}

// placeLineWith is the first place line of a group at loc ("production ·
// api"), after its letter column; "" when none.
func placeLineWith(out, loc string) string {
	re := regexp.MustCompile(`^ {4}[a-z ] {2}` + regexp.QuoteMeta(loc))
	for _, l := range strings.Split(out, "\n") {
		if re.MatchString(l) {
			return l
		}
	}
	return ""
}

func lineWith(out, sub string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

func TestTreeGoldens(t *testing.T) {
	setup := func(w int) *tui.Ctx {
		c := treeCtx(w, 20)
		c.Store.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{{
			ID: "h1", Object: "Variable", Action: "updated", Actor: "u1",
			ServiceIDs: []string{"s1"}, Names: []string{"API_KEY"}, CreatedAt: now,
		}}})
		c.Store.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Alice"}})
		for _, op := range []func() (changeset.Change, error){
			func() (changeset.Change, error) {
				return kinds.SetVariable(c.Store, nil, pl("e1", "s1"), "API_KEY", newVal, now)
			},
			func() (changeset.Change, error) {
				return kinds.SetVariable(c.Store, nil, pl("e1", "s1"), "NEW_ONE", newVal, now)
			},
			func() (changeset.Change, error) { return kinds.DeleteVariable(c.Store, pl("e2", "s2"), "DEBUG", now) },
		} {
			ch, err := op()
			if err != nil {
				t.Fatal(err)
			}
			c.CS.Put(ch, nil)
		}
		return c
	}
	secrets := []string{valA, valB, valC, newVal}

	c := setup(80)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter)
	out := treeFrame(c, s)
	tuitest.NoSecrets(t, out, secrets...)
	tuitest.Golden(t, "variables_tree_name_80.golden", out)

	c = setup(80)
	s = variables.Build()
	s.Init(c)
	s, _ = press(c, s, r("g"), enter)
	out = treeFrame(c, s)
	tuitest.NoSecrets(t, out, secrets...)
	tuitest.Golden(t, "variables_tree_value_80.golden", out)

	c = setup(60)
	s = variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter)
	out = treeFrame(c, s)
	tuitest.NoSecrets(t, out, secrets...)
	tuitest.Golden(t, "variables_tree_60.golden", out)
}

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

// A change the root puts reaches no screen; the next frame hides the value.
func TestTreeRevealEndsOnChangeSetUpdate(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, right, down, r("v"))
	if !strings.Contains(body(c, s), valA) {
		t.Fatal("not revealed")
	}
	ch, err := kinds.SetVariable(c.Store, nil, pl("e2", "s2"), "DEBUG", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(ch, nil)
	if out := body(c, s); strings.Contains(out, valA) {
		t.Error("value still shown after a change set update")
	}
}

// Bulk actions act on the places shown: a hidden staging place is neither
// written nor refused.
func TestTreeGroupActsOnVisiblePlacesOnly(t *testing.T) {
	c := treeCtx(100, 24)
	c.Settings.WriteEnvs = []string{"e1"}
	open := func() tui.Screen {
		c.Filters = nil // the filter is kept per screen key
		s := variables.Build()
		s.Init(c)
		s, _ = press(c, s, r("/"), r("production"), enter) // API_KEY group: 2 of 3 shown
		return s
	}
	want := []string{pl("e1", "s1").Key(), pl("e1", "s2").Key()}
	s, ms := press(c, open(), r("x"))
	if len(ms) != 0 || !strings.Contains(body(c, s), "delete 2 places?") {
		t.Fatalf("x: msgs %#v, confirm missing", ms)
	}
	_, ms = press(c, s, r("y"))
	var got []string
	for _, ch := range puts(t, ms) {
		got = append(got, ch.Place.Key())
	}
	if !slices.Equal(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
	s, ms = press(c, open(), r("e"))
	if len(ms) != 0 || !strings.Contains(treeFrame(c, s), "2 places") {
		t.Fatalf("e: msgs %#v\n%s", ms, treeFrame(c, s))
	}
	_, ms = press(c, s, r(newVal), enter, r(newVal), enter)
	got = nil
	for _, ch := range puts(t, ms) {
		got = append(got, ch.Place.Key())
	}
	if !slices.Equal(got, want) {
		t.Errorf("edited %v, want %v", got, want)
	}
}

// By value, the place lines of a group with several names say which.
func TestTreeByValuePlaceLinesName(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, r("g"), enter)
	var names []string
	for _, l := range strings.Split(body(c, s), "\n") {
		if placeLineWith(l, "production · api") != "" {
			_, after, _ := strings.Cut(l, "production · api")
			names = append(names, strings.Fields(after)[0])
		}
	}
	if !slices.Equal(names, []string{"API_KEY", "TOKEN"}) {
		t.Errorf("place names = %v\n%s", names, body(c, s))
	}
}

// The confirm acts on the places as they are when y is pressed: a refresh
// that changed them refuses.
func TestTreeGroupDeleteRechecksOnConfirm(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, r("x"))
	c.Store.ReplaceVariables("p1", slices.Delete(treeVars(), 2, 3)) // staging · api gone
	s, ms := press(c, s, r("y"))
	if f := flashOf(t, ms); f.Kind != '!' || !strings.Contains(f.Text, "changed") {
		t.Errorf("flash = %+v", f)
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("confirm left open")
	}
	// a WriteEnvs change meanwhile refuses too
	s, _ = press(c, variables.Build(), r("x"))
	c.Settings.WriteEnvs = []string{"e2"}
	_, ms = press(c, s, r("y"))
	if f := flashOf(t, ms); f.Kind != '✗' || !strings.Contains(f.Text, "not in write environments") {
		t.Errorf("flash = %+v", f)
	}
}

// A revealed value floats in a panel over the list: the place line keeps its
// columns and marks, and no line moves or overflows.
func TestTreeRevealFloats(t *testing.T) {
	c := treeCtx(60, 24)
	long := strings.Repeat("fixture-tree-long-", 6)
	vars := treeVars()
	vars[0].Unrendered, vars[0].Rendered = long, long
	c.Store.ReplaceVariables("p1", vars)
	ch, err := kinds.SetVariable(c.Store, nil, pl("e1", "s1"), "API_KEY", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(ch, nil)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter, down)
	before := body(c, s)
	s, _ = press(c, s, r("v"))
	after := body(c, s)
	if !strings.Contains(after, "fixture-tree-long-") || !strings.Contains(after, "╭─ API_KEY") {
		t.Fatalf("not revealed in a panel:\n%s", after)
	}
	l := placeLineWith(after, "production · api")
	if l == "" || l != placeLineWith(before, "production · api") {
		t.Errorf("the place line changed: %q, was %q", l, placeLineWith(before, "production · api"))
	}
	if strings.Contains(l, "fixt") || !strings.Contains(l, " chars") {
		t.Errorf("the value is on the place line or its overlay is gone: %q", l)
	}
	for _, l := range strings.Split(after, "\n") {
		if w := ansi.StringWidth(l); w > c.Width-tui.ChromeCols {
			t.Errorf("line %q is %d wide", l, w)
		}
	}
	tuitest.NoSecrets(t, before, long)
}

// A filter hiding the cursor's place keeps it remembered: clearing the
// filter returns to it.
func TestTreeCursorReturnsAfterFilter(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, enter, down, down, down) // staging · api
	s, _ = press(c, s, r("/"), r("production"))
	_ = body(c, s)
	s, _ = press(c, s, esc)
	if p, _ := cursorPlace(t, c, s); p != pl("e2", "s1") {
		t.Errorf("cursor = %v", p)
	}
}

func TestTreeNoteFitsNarrow(t *testing.T) {
	c := treeCtx(60, 20)
	s := variables.Build()
	s.Init(c)
	if hdr := strings.Split(treeFrame(c, s), "\n")[1]; strings.Contains(hdr, "…") || !strings.Contains(hdr, "by name") {
		t.Errorf("header = %q", hdr)
	}
}

// The filter matches the service as shown (sanitized).
func TestTreeFilterMatchesShownText(t *testing.T) {
	c := treeCtx(100, 24)
	cat := treeCatalog
	cat.Projects = slices.Clone(cat.Projects)
	cat.Projects[0].Services = []railway.Service{{ID: "s1", Name: "a​pi"}, {ID: "s2", Name: "w\x1b[31meb"}}
	c.Store.ReplaceCatalog(cat)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, r("/"), r("staging · web"), enter)
	out := body(c, s)
	if !strings.Contains(out, "DEBUG") || strings.Contains(out, "TOKEN") {
		t.Errorf("filtered:\n%s", out)
	}
}

// Reads in flight when another screen covers this one answer that screen:
// on return the screen asks again what the store does not hold, and sees
// what it does.
func TestTreeReadsAgainAfterAnswersElsewhere(t *testing.T) {
	f := railwaytest.New(t)
	f.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Alice"}})
	c := treeCtx(100, 24)
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
	c.Store.ReplaceVariables("p1", treeVars()[:4])

	s := variables.Build()
	msgs := runCmd(s.Init(c))
	if got := reads(msgs); !slices.Equal(got, []string{"h:e1", "m:p1"}) {
		t.Fatalf("Init reads = %v", got)
	}
	s.(tui.Suspender).Suspend(c)
	// the answers go to the screen on top; nothing reached the store
	if got := reads(runCmd(s.(tui.Resumer).Resume(c))); !slices.Equal(got, []string{"h:e1", "m:p1"}) {
		t.Errorf("Resume reads = %v, want them asked again", got)
	}
	// this time they land in the store while covered
	s.(tui.Suspender).Suspend(c)
	for _, m := range msgs {
		if u, ok := m.(store.Update); ok {
			u.ApplyTo(c.Store)
		}
	}
	if got := reads(runCmd(s.(tui.Resumer).Resume(c))); len(got) != 0 {
		t.Errorf("Resume after the store read = %v", got)
	}
}

// land applies msgs as the root does and returns the reads the screen's
// answers ask.
func land(c *tui.Ctx, s tui.Screen, msgs []tea.Msg) (tui.Screen, []string) {
	var got []string
	for _, m := range msgs {
		if u, ok := m.(store.Update); ok {
			u.ApplyTo(c.Store)
		}
		var cmd tea.Cmd
		s, cmd = s.Update(c, m)
		got = append(got, reads(runCmd(cmd))...)
	}
	return s, got
}

// A failed history or members read stands in the notice; its places show
// "?" rather than "…" for ever; r asks the failed reads again.
func TestTreeFailedReadsNoticeAndRetry(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", []railway.HistoryEntry{{
		ID: "h1", Object: "Variable", Action: "updated", Actor: "u1",
		ServiceIDs: []string{"s1"}, Names: []string{"TOKEN"}, CreatedAt: now.Add(-time.Hour),
	}})
	f.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Alice"}})
	f.FailNext("EnvironmentHistory", "Problem \x1b[31mprocessing")
	f.FailNext("ProjectMembers", "members down")
	c := treeCtx(100, 24)
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
	c.Store.ReplaceVariables("p1", treeVars()[:4])

	s := variables.Build()
	s, again := land(c, s, runCmd(s.Init(c)))
	if len(again) != 0 {
		t.Errorf("a failed read was asked again at once: %v", again)
	}
	n := s.(tui.Noter).Notice(c)
	if n.Kind != '✗' || !strings.Contains(n.Text, "cannot read history of acme/production: ") ||
		!strings.HasSuffix(n.Text, " · r retries") || strings.Contains(n.Text, "\x1b") {
		t.Errorf("notice = %q", n.Text)
	}
	out := body(c, s)
	if l := lineWith(out, "TOKEN"); !strings.Contains(l, "by ?") {
		t.Errorf("a failed read should show ?:\n%s", out)
	}
	if strings.Contains(out, "by …") {
		t.Errorf("no place should wait for a failed read:\n%s", out)
	}

	// r asks the failed reads again; their answers clear the notice
	s, cmd := s.Update(c, r("r"))
	msgs := runCmd(cmd)
	if got := reads(msgs); !slices.Equal(got, []string{"h:e1", "m:p1"}) {
		t.Fatalf("r reads = %v", got)
	}
	s, _ = land(c, s, msgs)
	if n := s.(tui.Noter).Notice(c); n.Text != "" {
		t.Errorf("notice after the retry = %q", n.Text)
	}
	if l := lineWith(body(c, s), "TOKEN"); !strings.Contains(l, "by Alice") {
		t.Errorf("author missing after the retry:\n%s", body(c, s))
	}
}

// Members failing alone: the notice names the project.
func TestTreeFailedMembersNotice(t *testing.T) {
	f := railwaytest.New(t)
	f.FailNext("ProjectMembers", "members down")
	c := treeCtx(100, 24)
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
	c.Store.ReplaceVariables("p1", treeVars()[:4])
	s := variables.Build()
	s, _ = land(c, s, runCmd(s.Init(c)))
	if n := s.(tui.Noter).Notice(c); !strings.HasPrefix(n.Text, "cannot read members of acme: ") {
		t.Errorf("notice = %q", n.Text)
	}
}

// A group edit resolves its places again on submit: a refresh that removed
// one while typing must not turn its edit into an add.
func TestTreeGroupEditRechecksOnSubmit(t *testing.T) {
	c := treeCtx(100, 24)
	s := variables.Build()
	s.Init(c)
	s, _ = press(c, s, r("e"), r(newVal), enter)
	c.Store.ReplaceVariables("p1", slices.Delete(treeVars(), 2, 3)) // staging · api gone
	s, ms := press(c, s, r(newVal), enter)
	for _, m := range ms {
		if _, ok := m.(tui.PutChangeMsg); ok {
			t.Errorf("a change was put: %+v", m)
		}
	}
	if f := flashOf(t, ms); f.Kind != '!' || !strings.Contains(f.Text, "changed") {
		t.Errorf("flash = %+v", f)
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("input left open")
	}
	// a WriteEnvs change meanwhile refuses too
	s, _ = press(c, variables.Build(), r("e"), r(newVal), enter)
	c.Settings.WriteEnvs = []string{"e2"}
	_, ms = press(c, s, r(newVal), enter)
	for _, m := range ms {
		if _, ok := m.(tui.PutChangeMsg); ok {
			t.Errorf("a change was put: %+v", m)
		}
	}
	if f := flashOf(t, ms); f.Kind != '✗' || !strings.Contains(f.Text, "not in write environments") {
		t.Errorf("flash = %+v", f)
	}
}

func TestVariablesROpensRotate(t *testing.T) {
	c := treeCtx(100, 24)
	var fp string
	for _, v := range c.Store.VariablesAt(pl("e1", "s1")) {
		if v.Name == "API_KEY" {
			fp = v.Fingerprint
		}
	}
	if fp == "" {
		t.Fatal("no fingerprint for API_KEY")
	}
	s := variables.Build()
	s.Init(c)
	const refuse = "R works on a value group · g groups by value"
	// by name: the group line refuses
	_, ms := press(c, s, r("R"))
	if f := flashOf(t, ms); f.Kind != '!' || f.Text != refuse {
		t.Errorf("by name flash = %c %q", f.Kind, f.Text)
	}
	// by value: the cursor is on the valA group (API_KEY = TOKEN)
	s, ms = press(c, s, r("g"), r("R"))
	if len(ms) != 1 {
		t.Fatalf("R msgs = %#v", ms)
	}
	om, ok := ms[0].(tui.OpenMsg)
	if !ok || om.Name != "rotate" || len(om.Args) != 1 || om.Args[0] != fp {
		t.Errorf("R msg = %#v, want rotate %s", ms[0], fp)
	}
	// a place line of the unfolded group refuses
	_, ms = press(c, s, enter, down, r("R"))
	if f := flashOf(t, ms); f.Text != refuse {
		t.Errorf("place line flash = %q", f.Text)
	}
	// the scoped view refuses
	_, ms = press(c, variables.New(pl("e1", "s1")), r("R"))
	if f := flashOf(t, ms); f.Text != refuse {
		t.Errorf("scoped flash = %q", f.Text)
	}
}

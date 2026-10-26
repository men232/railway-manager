package history_test

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/history"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// payloadSecret is a value-like string a payload may hold; it never renders.
const payloadSecret = "fixture-history-payload-value"

var (
	enter = tea.KeyMsg{Type: tea.KeyEnter}
	down  = tea.KeyMsg{Type: tea.KeyDown}
)

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// catalog: project acme, environments production (e1) and staging (e2),
// both with api (s1) and web (s2).
var catalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "acme", Workspace: "ws",
	Environments: []railway.Environment{
		{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2"}},
		{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"s1", "s2"}},
	},
	Services: []railway.Service{{ID: "s1", Name: "api"}, {ID: "s2", Name: "web"}},
}}}

func newCtx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(catalog)
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views:    &view.Views{Store: st, Reg: kinds.NewRegistry()},
		Settings: &settings.Settings{Projects: []string{"p1"}, Envs: []string{"e1", "e2"}, WriteEnvs: []string{"e1"}},
		Theme:    tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Now: func() time.Time { return now },
	}
}

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

func withAPI(c *tui.Ctx, f *railwaytest.Fake) {
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
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

// reads lists the reads among msgs: "h:<env>[@<after>]", "p:<env>" and
// "m:<project>".
func reads(msgs []tea.Msg) []string {
	var out []string
	for _, m := range msgs {
		if p, ok := m.(interface{ Page() loaders.HistoryMsg }); ok {
			m = p.Page()
		}
		switch v := m.(type) {
		case loaders.HistoryMsg:
			s := "h:" + v.EnvID
			if v.After != "" {
				s += "@" + v.After
			}
			out = append(out, s)
		case loaders.PatchesMsg:
			out = append(out, "p:"+v.EnvID)
		case loaders.MembersMsg:
			out = append(out, "m:"+v.ProjectID)
		}
	}
	slices.Sort(out)
	return out
}

// deliver lands msgs as the root does: a store update is applied, then
// every message reaches the screen. It returns the messages the screen's
// answers produce.
func deliver(c *tui.Ctx, s tui.Screen, msgs []tea.Msg) (tui.Screen, []tea.Msg) {
	var out []tea.Msg
	for _, m := range msgs {
		if u, ok := m.(store.Update); ok {
			u.ApplyTo(c.Store)
		}
		var cmd tea.Cmd
		s, cmd = s.Update(c, m)
		out = append(out, runCmd(cmd)...)
	}
	return s, out
}

func press(c *tui.Ctx, s tui.Screen, keys ...tea.KeyMsg) (tui.Screen, []tea.Msg) {
	var out []tea.Msg
	for _, k := range keys {
		var cmd tea.Cmd
		s, cmd = s.Update(c, k)
		out = append(out, runCmd(cmd)...)
	}
	return s, out
}

func body(c *tui.Ctx, s tui.Screen) string {
	return ansi.Strip(s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows))
}

func frame(c *tui.Ctx, s tui.Screen) string {
	n := s.(tui.Noter)
	ft := tui.Footer{Hints: s.Keys(), Flash: n.Notice(c)}
	return tuitest.Plain(tui.Frame(c, s.Title(), n.Note(c), s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), ft))
}

func flashOf(t *testing.T, ms []tea.Msg) tui.FlashMsg {
	t.Helper()
	for _, m := range ms {
		if f, ok := m.(tui.FlashMsg); ok {
			return f
		}
	}
	t.Fatalf("no flash in %#v", ms)
	return tui.FlashMsg{}
}

func entry(id string, ago time.Duration, object, action, actor string, svcs []string, names ...string) railway.HistoryEntry {
	return railway.HistoryEntry{
		ID: id, Object: object, Action: action, Actor: actor,
		ServiceIDs: svcs, Names: names, CreatedAt: now.Add(-ago),
	}
}

// many is n Variable entries of env, newest first, one minute apart.
func many(env string, n int) []railway.HistoryEntry {
	out := make([]railway.HistoryEntry, n)
	for i := range out {
		out[i] = entry(env+"-"+strconv.Itoa(i), time.Duration(i+1)*time.Minute, "Variable", "updated", "u1", []string{"s1"}, "VAR_"+strconv.Itoa(i))
	}
	return out
}

func TestHistoryScreenLoadsOnce(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", many("e1", 3))
	f.SetHistory("e2", many("e2", 2))
	f.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Alice"}})
	c := newCtx(100, 30)
	withAPI(c, f)

	s := history.Build()
	msgs := runCmd(s.Init(c))
	if got := reads(msgs); !slices.Equal(got, []string{"h:e1", "h:e2", "m:p1", "p:e1", "p:e2"}) {
		t.Fatalf("Init reads = %v", got)
	}
	// while they are in flight neither Resume nor r asks again
	if got := reads(runCmd(s.(tui.Resumer).Resume(c))); len(got) != 0 {
		t.Errorf("Resume in flight reads = %v", got)
	}
	s, ms := press(c, s, r("r"))
	if got := reads(ms); len(got) != 0 {
		t.Errorf("r in flight reads = %v", got)
	}
	s, ms = deliver(c, s, msgs)
	if got := reads(ms); len(got) != 0 {
		t.Errorf("answers read = %v", got)
	}
	if got := reads(runCmd(s.(tui.Resumer).Resume(c))); len(got) != 0 {
		t.Errorf("Resume after read = %v", got)
	}
	out := body(c, s)
	if !strings.Contains(out, "Alice") || !strings.Contains(out, "VAR_0") || strings.Count(out, "VAR_") != 5 {
		t.Errorf("rows:\n%s", out)
	}
	// r re-reads the first pages (and the patches)
	_, ms = press(c, s, r("r"))
	if got := reads(ms); !slices.Equal(got, []string{"h:e1", "h:e2", "p:e1", "p:e2"}) {
		t.Errorf("r reads = %v", got)
	}
}

// Answers that land while another screen is on top reach that screen: the
// root stores them, and on return nothing waits for them.
func TestHistoryResumeAfterAnswersElsewhere(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", many("e1", 120))
	c := newCtx(100, 30)
	withAPI(c, f)
	s := history.Build()
	msgs := runCmd(s.Init(c))
	s.(tui.Suspender).Suspend(c)
	for _, m := range msgs {
		if u, ok := m.(store.Update); ok {
			u.ApplyTo(c.Store)
		}
	}
	if got := reads(runCmd(s.(tui.Resumer).Resume(c))); len(got) != 0 {
		t.Errorf("Resume reads = %v", got)
	}
	if _, ms := press(c, s, r("m")); !slices.Equal(reads(ms), []string{"h:e1@e1:100"}) {
		t.Errorf("m after return reads = %v", reads(ms))
	}
}

func TestHistoryMoreAppends(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", many("e1", 250))
	f.SetHistory("e2", many("e2", 2))
	c := newCtx(100, 30)
	withAPI(c, f)

	s := history.Build()
	s, _ = deliver(c, s, runCmd(s.Init(c)))
	if n := len(c.Views.Feed([]string{"e1", "e2"}, nil)); n != 102 {
		t.Fatalf("first pages rows = %d", n)
	}

	// m reads the next page of e1 only, once
	s, next1 := press(c, s, r("m"))
	if got := reads(next1); !slices.Equal(got, []string{"h:e1@e1:100"}) {
		t.Fatalf("m reads = %v", got)
	}
	if _, ms := press(c, s, r("m")); len(reads(ms)) != 0 {
		t.Errorf("m with a next read in flight reads = %v", reads(ms))
	}

	// a refresh while the next page is in flight: m waits for the new first
	// page, and the old next page is ignored when it lands
	s, first := press(c, s, r("r"))
	if got := reads(first); !slices.Equal(got, []string{"h:e1", "h:e2", "p:e1", "p:e2"}) {
		t.Fatalf("r reads = %v", got)
	}
	if _, ms := press(c, s, r("m")); len(reads(ms)) != 0 {
		t.Errorf("m with the first page in flight reads = %v", reads(ms))
	}
	s, _ = deliver(c, s, first)
	s, next2 := press(c, s, r("m"))
	if got := reads(next2); !slices.Equal(got, []string{"h:e1@e1:100"}) {
		t.Fatalf("m after refresh reads = %v", got)
	}
	s, _ = deliver(c, s, next1) // stale: from before the refresh
	if _, ms := press(c, s, r("m")); len(reads(ms)) != 0 {
		t.Errorf("a stale page cleared the read in flight: m reads = %v", reads(ms))
	}
	s, _ = deliver(c, s, next2) // the store drops it: the stale page got there first
	s, next3 := press(c, s, r("m"))
	if got := reads(next3); !slices.Equal(got, []string{"h:e1@e1:200"}) {
		t.Fatalf("m for the last page reads = %v", got)
	}
	s, _ = deliver(c, s, next3)

	rows := c.Views.Feed([]string{"e1", "e2"}, nil)
	if len(rows) != 252 {
		t.Errorf("rows = %d, want 252", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		k := row.Env + "/" + strings.Join(row.Names, ",")
		if seen[k] {
			t.Errorf("duplicate row %s", k)
		}
		seen[k] = true
	}
	// nothing more to read
	_, ms := press(c, s, r("m"))
	if len(reads(ms)) != 0 {
		t.Errorf("m with no next page reads = %v", reads(ms))
	}
	if fl := flashOf(t, ms); !strings.Contains(fl.Text, "no older") {
		t.Errorf("flash = %q", fl.Text)
	}
}

// patchCtx holds one EnvironmentPatch entry (newest) of patch pa in e1, and
// a Variable entry under it.
func patchCtx(message string, runs []apply.RunInfo) *tui.Ctx {
	c := newCtx(100, 20)
	c.Store.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		{ID: "h1", Object: "EnvironmentPatch", Action: "applied", Actor: "u1", CreatedAt: now.Add(-time.Minute), WorkflowID: "commitChanges/e1/pa"},
		entry("h2", 2*time.Minute, "Variable", "updated", "u1", []string{"s1"}, "API_KEY"),
	}})
	c.Store.SetPatches("e1", []railway.EnvPatch{{ID: "pa", Message: message, CreatedAt: now.Add(-time.Minute)}})
	c.Store.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Alice"}})
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: runs}
	return c
}

func TestHistoryEnterOpensLocalRun(t *testing.T) {
	c := patchCtx("railway-manager run1: 2 changes", []apply.RunInfo{{ID: "run1", PatchIDs: map[string]string{"pa": "e1"}}})
	s := history.Build()
	s.Init(c)
	if l := lineWith(body(c, s), "applied"); !strings.Contains(l, "◆ run run1") {
		t.Errorf("patch row not marked: %q", l)
	}
	_, ms := press(c, s, enter)
	if len(ms) != 1 {
		t.Fatalf("enter msgs = %#v", ms)
	}
	om, ok := ms[0].(tui.OpenMsg)
	if !ok || om.Name != "run" || len(om.Args) != 1 || om.Args[0] != "run1" {
		t.Errorf("enter = %#v", ms[0])
	}
	// another row: nothing
	if _, ms := press(c, s, down, enter); len(ms) != 0 {
		t.Errorf("enter on a variable row = %#v", ms)
	}
}

func TestHistoryForeignRunFlash(t *testing.T) {
	c := patchCtx("railway-manager run9: 2 changes", nil)
	s := history.Build()
	s.Init(c)
	if l := lineWith(body(c, s), "applied"); !strings.Contains(l, "◆ run run9") {
		t.Errorf("patch row not marked: %q", l)
	}
	_, ms := press(c, s, enter)
	fl := flashOf(t, ms)
	if fl.Kind != '!' || fl.Text != "run run9 was made on another machine" {
		t.Errorf("flash = %c %q", fl.Kind, fl.Text)
	}
	for _, m := range ms {
		if _, ok := m.(tui.OpenMsg); ok {
			t.Errorf("foreign run opened: %#v", m)
		}
	}
	// a patch that is not ours has no mark
	c = patchCtx("someone else", nil)
	s = history.Build()
	s.Init(c)
	if l := lineWith(body(c, s), "applied"); strings.Contains(l, "◆") {
		t.Errorf("foreign patch marked: %q", l)
	}
}

func TestHistoryFailureKeepsRows(t *testing.T) {
	c := patchCtx("", nil)
	f := railwaytest.New(t)
	withAPI(c, f)
	s := history.Build()
	s.Init(c)
	f.FailNext("EnvironmentHistory", "connection reset") // e1's is asked first
	s, first := press(c, s, r("r"))
	s, _ = deliver(c, s, first)
	if l := lineWith(body(c, s), "API_KEY"); l == "" {
		t.Errorf("rows dropped on failure:\n%s", body(c, s))
	}
	n := s.(tui.Noter).Notice(c)
	if n.Kind != '✗' || !strings.Contains(n.Text, "cannot read history of acme/production") || !strings.Contains(n.Text, "connection reset") {
		t.Errorf("notice = %c %q", n.Kind, n.Text)
	}
	// a later read that succeeds clears it
	s, again := press(c, s, r("r"))
	s, _ = deliver(c, s, again)
	if n := s.(tui.Noter).Notice(c); n.Text != "" {
		t.Errorf("notice after success = %q", n.Text)
	}
}

func TestHistoryNeverShowsPayload(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistoryRaw("e1", []map[string]any{
		{
			"id": "h1", "object": "Variable", "action": "updated",
			"createdAt":  now.Add(-time.Minute).Format(time.RFC3339Nano),
			"serviceIds": []string{"s1"},
			"payload":    map[string]any{"name": "API_KEY", "value": payloadSecret, "id": "x", "isSealed": false},
			"actor":      map[string]any{"principal": "u1"},
		},
		{
			"id": "h2", "object": "Deployment", "action": "created",
			"createdAt":  now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
			"serviceIds": []string{"s2"},
			"payload":    map[string]any{"commitMessage": payloadSecret, "variables": map[string]any{"K": payloadSecret}},
		},
	})
	c := newCtx(100, 20)
	withAPI(c, f)
	s := history.Build()
	s, _ = deliver(c, s, runCmd(s.Init(c)))
	out := frame(c, s)
	tuitest.NoSecrets(t, out, payloadSecret)
	if !strings.Contains(out, "API_KEY") || !strings.Contains(out, "deploy") {
		t.Errorf("rows missing:\n%s", out)
	}
	// the Deployment entry has no actor
	if l := lineWith(out, "deploy"); !strings.Contains(l, " ? ") {
		t.Errorf("no actor should show ?: %q", l)
	}
}

func TestHistorySanitizesStrings(t *testing.T) {
	c := newCtx(100, 20)
	c.Store.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		entry("h1", time.Minute, "Vari\x1b[31mable", "upd\x1b[32mated", "u1", []string{"s1"}, "NA\x1b[33mME", "OK\x1b]0;title\x07"),
	}})
	c.Store.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Ev\x1b[34mil"}})
	s := history.Build()
	s.Init(c)
	out := s.View(c, 96, 18)
	for _, esc := range []string{"\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[34m", "\x1b]0;"} {
		if strings.Contains(out, esc) {
			t.Errorf("escape %q reached the frame: %q", esc, out)
		}
	}
	if b := ansi.Strip(out); !strings.Contains(b, "NAME, OK") || !strings.Contains(b, "Evil") {
		t.Errorf("sanitized text missing:\n%s", b)
	}
}

func TestHistoryFilterPreset(t *testing.T) {
	c := patchCtx("", nil)
	s := history.Build("API_KEY")
	s.Init(c)
	out := body(c, s)
	if !strings.Contains(out, "API_KEY") || strings.Contains(out, "applied") {
		t.Errorf("preset filter:\n%s", out)
	}
	if !strings.Contains(out, "/ API_KEY") {
		t.Errorf("filter line missing:\n%s", out)
	}
	if history.Build(42) != nil || history.Build("a", "b") != nil {
		t.Error("Build accepts bad args")
	}
}

// goldenCtx: entries of both environments, ours (local and foreign) and
// others', with a payload value the fake serves.
func goldenCtx(t *testing.T, w int) (*tui.Ctx, tui.Screen) {
	t.Helper()
	f := railwaytest.New(t)
	f.SetHistoryRaw("e1", []map[string]any{
		{"id": "a1", "object": "EnvironmentPatch", "action": "applied", "createdAt": now.Add(-time.Minute).Format(time.RFC3339Nano),
			"workflowId": "commitChanges/e1/pa", "actor": map[string]any{"principal": "u1"}, "payload": map[string]any{}},
		{"id": "a2", "object": "Variable", "action": "updated", "createdAt": now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
			"serviceIds": []string{"s1"}, "actor": map[string]any{"principal": "u1"},
			"payload": map[string]any{"name": "DATABASE_URL", "value": payloadSecret}},
		{"id": "a3", "object": "Deployment", "action": "created", "createdAt": now.Add(-3 * time.Hour).Format(time.RFC3339Nano),
			"serviceIds": []string{"s1", "s2"}, "payload": map[string]any{"commitMessage": payloadSecret}},
	})
	f.SetHistoryRaw("e2", []map[string]any{
		{"id": "b1", "object": "EnvironmentPatch", "action": "applied", "createdAt": now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
			"workflowId": "commitChanges/e2/pb", "actor": map[string]any{"principal": "u2"}},
		{"id": "b2", "object": "Variable", "action": "deleted", "createdAt": now.Add(-26 * time.Hour).Format(time.RFC3339Nano),
			"serviceIds": []string{"s2"}, "actor": map[string]any{"principal": "u2"},
			"payload": map[string]any{"names": []string{"SESSION_SECRET_KEY", "LEGACY_TOKEN"}}},
	})
	f.SetPatches("e1", []railway.EnvPatch{{ID: "pa", Message: "railway-manager 20261024-115900-ab12: 2 changes", CreatedAt: now.Add(-time.Minute)}})
	f.SetPatches("e2", []railway.EnvPatch{{ID: "pb", Message: "railway-manager 20261024-113000-cd34: 1 change", CreatedAt: now.Add(-30 * time.Minute)}})
	f.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Alice Example"}, {ID: "u2", Email: "bob@example.com"}})
	c := newCtx(w, 16)
	withAPI(c, f)
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: []apply.RunInfo{{ID: "20261024-115900-ab12", PatchIDs: map[string]string{"pa": "e1"}}}}
	s := history.Build()
	s, _ = deliver(c, s, runCmd(s.Init(c)))
	return c, s
}

func TestHistoryGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c, s := goldenCtx(t, w)
		out := frame(c, s)
		tuitest.NoSecrets(t, out, payloadSecret)
		tuitest.Golden(t, "history_"+strconv.Itoa(w)+".golden", out)
	}
}

// Narrow widths: rows never exceed the width, and the ◆ mark (short) stays
// whole while less important columns go.
func TestHistoryNarrowKeepsMark(t *testing.T) {
	for _, w := range []int{80, 60, 50, 40} {
		c, s := goldenCtx(t, w)
		bw := w - tui.ChromeCols
		out := ansi.Strip(s.View(c, bw, c.Height-tui.ChromeRows))
		for _, l := range strings.Split(out, "\n") {
			if ansi.StringWidth(l) > bw {
				t.Errorf("w=%d line too wide (%d): %q", w, ansi.StringWidth(l), l)
			}
		}
		if !strings.Contains(out, "◆ ab12") {
			t.Errorf("w=%d mark cut:\n%s", w, out)
		}
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

// At 80 every column shows, objects by their short names: the mark shrinks
// to the run id's tail first.
// With room, the full mark shows.
func TestHistoryShortMarkKeepsColumns(t *testing.T) {
	c, s := goldenCtx(t, 80)
	out := body(c, s)
	for _, obj := range []string{"patch", "variable", "deploy"} {
		if !strings.Contains(out, obj) || strings.Contains(out, "EnvironmentPatch") {
			t.Errorf("object %s missing at 80:\n%s", obj, out)
		}
	}
	for _, who := range []string{"Alice", "bob@"} {
		if !strings.Contains(out, who) {
			t.Errorf("who %s missing at 80:\n%s", who, out)
		}
	}
	if !strings.Contains(out, "DATABASE_URL") {
		t.Errorf("names cut at 80:\n%s", out)
	}
	if !strings.Contains(out, "◆ ab12") || !strings.Contains(out, "◆ cd34") || strings.Contains(out, "◆ run") {
		t.Errorf("short mark not used at 80:\n%s", out)
	}
	c, s = goldenCtx(t, 160)
	if out := body(c, s); !strings.Contains(out, "◆ run 20261024-115900-ab12") {
		t.Errorf("full mark not used at 160:\n%s", out)
	}
}

// Enter keeps the full run id whatever the mark shows.
func TestHistoryForeignRunFlashNarrow(t *testing.T) {
	c, s := goldenCtx(t, 60)
	if l := lineWith(body(c, s), "◆ cd34"); l == "" {
		t.Fatalf("short mark missing:\n%s", body(c, s))
	}
	_, ms := press(c, s, down, down, enter)
	if fl := flashOf(t, ms); fl.Text != "run 20261024-113000-cd34 was made on another machine" {
		t.Errorf("flash = %q", fl.Text)
	}
	_, ms = press(c, s, tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeyUp}, enter)
	if len(ms) != 1 {
		t.Fatalf("enter msgs = %#v", ms)
	}
	if om, ok := ms[0].(tui.OpenMsg); !ok || om.Args[0] != "20261024-115900-ab12" {
		t.Errorf("enter = %#v", ms[0])
	}
}

func TestHistoryMembersFailureNotice(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", many("e1", 2))
	f.FailNext("ProjectMembers", "members denied")
	c := newCtx(100, 20)
	withAPI(c, f)
	s := history.Build()
	s, _ = deliver(c, s, runCmd(s.Init(c)))
	n := s.(tui.Noter).Notice(c)
	if n.Kind != '✗' || !strings.Contains(n.Text, "cannot read members of acme") || !strings.Contains(n.Text, "members denied") {
		t.Errorf("notice = %c %q", n.Kind, n.Text)
	}
	if !strings.Contains(body(c, s), "VAR_0") {
		t.Errorf("rows missing:\n%s", body(c, s))
	}
}

// r while only e1's first page is in flight asks e2's again, not e1's.
func TestHistoryRefreshWithOneFirstPageInFlight(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", many("e1", 2))
	f.SetHistory("e2", many("e2", 2))
	c := newCtx(100, 20)
	withAPI(c, f)
	s := history.Build()
	var rest []tea.Msg
	for _, m := range runCmd(s.Init(c)) {
		if got := reads([]tea.Msg{m}); len(got) == 1 && got[0] == "h:e1" {
			continue // still in flight
		}
		rest = append(rest, m)
	}
	s, _ = deliver(c, s, rest)
	_, ms := press(c, s, r("r"))
	if got := reads(ms); !slices.Equal(got, []string{"h:e2", "p:e1", "p:e2"}) {
		t.Errorf("r reads = %v", got)
	}
}

// A page one History screen asked for never answers another's read.
func TestHistoryPagesOfAnotherInstanceIgnored(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", many("e1", 2))
	f.SetHistory("e2", many("e2", 2))
	c := newCtx(100, 20)
	withAPI(c, f)
	a := history.Build()
	am := runCmd(a.Init(c))
	b := history.Build()
	runCmd(b.Init(c))
	b, _ = deliver(c, b, am)
	if _, ms := press(c, b, r("r")); len(reads(ms)) != 2 || !slices.Equal(reads(ms), []string{"p:e1", "p:e2"}) {
		t.Errorf("b's first pages were answered by a's: r reads = %v", reads(ms))
	}
}

// The filter matches the text as shown: each part sanitized on its own.
func TestHistoryFilterMatchesShownText(t *testing.T) {
	c := newCtx(100, 20)
	c.Store.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		entry("h1", time.Minute, "Variable", "updated", "u1", []string{"s1"}, "TOKEN"),
	}})
	c.Store.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Evil\x1b"}})
	c.Filters = map[string]string{"history": "evil upd"}
	s := history.Build()
	s.Init(c)
	if !strings.Contains(body(c, s), "TOKEN") {
		t.Errorf("shown text not matched:\n%s", body(c, s))
	}
}

// An environment is shown by its own name, even with "/" in the project's.
func TestHistorySlashInProjectName(t *testing.T) {
	c := newCtx(100, 20)
	cat := catalog
	cat.Projects = slices.Clone(catalog.Projects)
	cat.Projects[0].Name = "team/acme"
	c.Store.ReplaceCatalog(cat)
	c.Store.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		entry("h1", time.Minute, "Variable", "updated", "u1", []string{"s1"}, "TOKEN"),
	}})
	s := history.Build()
	s.Init(c)
	if l := lineWith(body(c, s), "TOKEN"); !strings.Contains(l, "production · api") || strings.Contains(l, "acme/production") {
		t.Errorf("env shown as %q", l)
	}
}

// A non-Variable entry's payload `name`/`names` is not a variable name and
// never renders.
func TestHistoryNoNamesForOtherObjects(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistoryRaw("e1", []map[string]any{
		{"id": "h1", "object": "SSHSession", "action": "created",
			"createdAt": now.Add(-time.Minute).Format(time.RFC3339Nano),
			"payload":   map[string]any{"name": "SSH_PAYLOAD_NAME"}},
		{"id": "h2", "object": "Volume", "action": "updated",
			"createdAt": now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
			"payload":   map[string]any{"names": []any{"VOLUME_PAYLOAD_NAME"}}},
		{"id": "h3", "object": "Domain", "action": "created",
			"createdAt": now.Add(-3 * time.Minute).Format(time.RFC3339Nano),
			"payload":   map[string]any{"name": "DOMAIN_PAYLOAD_NAME"}},
	})
	c := newCtx(100, 20)
	withAPI(c, f)
	s := history.Build()
	s, _ = deliver(c, s, runCmd(s.Init(c)))
	out := frame(c, s)
	for _, obj := range []string{"ssh", "Volume", "Domain"} {
		if !strings.Contains(out, obj) {
			t.Errorf("%s row missing:\n%s", obj, out)
		}
	}
	if strings.Contains(out, "PAYLOAD_NAME") {
		t.Errorf("a non-Variable payload name rendered:\n%s", out)
	}
}

// The run mark is an icon: Nerd form under Nerd icons, and no Private Use
// rune in plain mode.
func TestHistoryRunMarkIcon(t *testing.T) {
	c := patchCtx("railway-manager run1: 2 changes", []apply.RunInfo{{ID: "run1", PatchIDs: map[string]string{"pa": "e1"}}})
	s := history.Build()
	s.Init(c)
	out := frame(c, s)
	for _, ru := range out {
		if ru >= 0xE000 && ru <= 0xF8FF || ru >= 0xF0000 {
			t.Errorf("plain icons emitted a Private Use rune %U", ru)
		}
	}
	c.Icons = tui.Icons{Nerd: true}
	if l := lineWith(body(c, s), "applied"); !strings.Contains(l, c.Icons.RunMark()+" run run1") || strings.Contains(l, "◆") {
		t.Errorf("Nerd run mark missing: %q", l)
	}
}

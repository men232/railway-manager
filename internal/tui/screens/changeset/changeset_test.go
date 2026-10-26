package changeset_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	cs "github.com/men232/railway-manager/internal/tui/screens/changeset"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

const secret = "fixture-value-alpha"

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

var catalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2"}}},
	Services:     []railway.Service{{ID: "s1", Name: "api"}, {ID: "s2", Name: "worker"}},
}}}

func place(svc string) domain.Place {
	return domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: svc}
}

type fixture struct {
	ctx *tui.Ctx
	ids []string // change ids in display order
}

// newCtx has an api change (API_KEY), a worker change (QUEUE) and the
// redeploys of both places.
func newCtx(t *testing.T, w, h int) fixture {
	t.Helper()
	st := store.New()
	st.ReplaceCatalog(catalog)
	st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "API_KEY", Unrendered: secret, Rendered: secret},
		{EnvID: "e1", ServiceID: "s2", Name: "QUEUE", Unrendered: "q", Rendered: "q"},
	})
	set := &changeset.ChangeSet{}
	var ids []string
	for _, v := range []struct{ svc, name string }{{"s1", "API_KEY"}, {"s2", "QUEUE"}} {
		c, err := kinds.SetVariable(st, nil, place(v.svc), v.name, "new-"+v.name, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := set.Put(c, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	views := &view.Views{Store: st, Reg: kinds.NewRegistry(),
		Compile: func(c changeset.ChangeSet) (apply.Graph, error) {
			var g apply.Graph
			for _, pk := range []string{"e1/s1", "e1/s2"} {
				if !slices.Contains(c.RedeployOff, pk) {
					g.Order = append(g.Order, "redeploy:"+pk)
				}
			}
			return g, nil
		}}
	return fixture{ids: ids, ctx: &tui.Ctx{RunIndex: tui.RunIndex{Loaded: true},
		Store: st, CS: set, Views: views, Settings: &settings.Settings{Projects: []string{"p1"}},
		Theme: tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Now: func() time.Time { return now },
	}}
}

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	enter = tea.KeyMsg{Type: tea.KeyEnter}
	down  = tea.KeyMsg{Type: tea.KeyDown}
	space = tea.KeyMsg{Type: tea.KeySpace}
	esc   = tea.KeyMsg{Type: tea.KeyEsc}
)

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
		case components.ConfirmedMsg, components.CancelMsg:
			var next tea.Cmd
			s, next = s.Update(c, m)
			run(next)
		case nil:
		default:
			out = append(out, m)
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

func body(c *tui.Ctx, s tui.Screen) string {
	return ansi.Strip(s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows))
}

func frame(c *tui.Ctx, s tui.Screen) string {
	return tuitest.Plain(tui.Frame(c, s.Title(), "", s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
}

func typed(w string) []tea.KeyMsg {
	var ks []tea.KeyMsg
	for _, ch := range w {
		ks = append(ks, r(string(ch)))
	}
	return ks
}

// First change row is the cursor after one down (row 0 is the empty runs note
// only when there are no runs; without runs the first row is the first change).
func TestChangeSetApplySelection(t *testing.T) {
	f := newCtx(t, 80, 24)
	s := cs.New()
	s, _ = press(f.ctx, s, space) // cursor on the first change
	keys := append([]tea.KeyMsg{r("a")}, typed("apply")...)
	_, ms := press(f.ctx, s, append(keys, enter)...)
	want := tui.ApplyMsg{IDs: []string{f.firstID(t, s)}}
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], want) {
		t.Fatalf("msgs = %#v, want %#v", ms, want)
	}
}

// firstID is the id of the change at the first display position.
func (f fixture) firstID(t *testing.T, _ tui.Screen) string {
	t.Helper()
	v := f.ctx.Views.ChangeSet(f.ctx.CS, nil)
	return v.Groups[0].Changes[0].Change.ID
}

func TestChangeSetApplyWrongWord(t *testing.T) {
	f := newCtx(t, 80, 24)
	s, _ := press(f.ctx, cs.New(), space)
	keys := append([]tea.KeyMsg{r("a")}, typed("aply")...)
	s, ms := press(f.ctx, s, append(keys, enter)...)
	if len(ms) != 0 {
		t.Fatalf("msgs = %#v", ms)
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("confirm should be closed after a wrong word")
	}
}

func TestChangeSetApplyAllWhenNoneSelected(t *testing.T) {
	f := newCtx(t, 80, 24)
	s, _ := press(f.ctx, cs.New(), r("a"))
	if !s.(tui.Capturer).Capturing() {
		t.Fatal("confirm should capture keys")
	}
	if v := body(f.ctx, s); !strings.Contains(v, "apply all 2 changes") {
		t.Errorf("prompt missing:\n%s", v)
	}
	_, ms := press(f.ctx, s, append(typed("apply"), enter)...)
	want := tui.ApplyMsg{IDs: nil}
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], want) {
		t.Fatalf("msgs = %#v", ms)
	}
}

func TestChangeSetLockedCannotBeSelected(t *testing.T) {
	f := newCtx(t, 80, 24)
	f.ctx.CS.Changes[0].RunID = "r9"
	s, ms := press(f.ctx, cs.New(), space)
	if len(ms) == 0 {
		t.Fatal("selecting a locked change should flash")
	}
	if fl, ok := ms[0].(tui.FlashMsg); !ok || fl.Kind != '!' {
		t.Fatalf("msg = %#v", ms[0])
	}
	if v := body(f.ctx, s); !strings.Contains(v, "⟳ r9") || strings.Contains(v, "* QUEUE") || strings.Contains(v, "* API_KEY") {
		t.Errorf("locked change should show its run and no mark:\n%s", v)
	}
	// apply all excludes the locked change: only one unlocked remains
	s, _ = press(f.ctx, s, r("a"))
	if v := body(f.ctx, s); !strings.Contains(v, "apply all 1 change ") {
		t.Errorf("prompt:\n%s", v)
	}
}

func TestChangeSetRemove(t *testing.T) {
	f := newCtx(t, 80, 24)
	_, ms := press(f.ctx, cs.New(), r("x"))
	id := f.ctx.Views.ChangeSet(f.ctx.CS, nil).Groups[0].Changes[0].Change.ID
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], tui.RemoveChangeMsg{ID: id}) {
		t.Fatalf("msgs = %#v", ms)
	}
}

func TestChangeSetToggleRedeploy(t *testing.T) {
	f := newCtx(t, 80, 24)
	// two changes, then the first redeploy row
	_, ms := press(f.ctx, cs.New(), down, down, r("t"))
	want := tui.ToggleRedeployMsg{PlaceKey: "e1/s1"}
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], want) {
		t.Fatalf("msgs = %#v", ms)
	}
	// on a change row t does nothing
	_, ms = press(f.ctx, cs.New(), r("t"))
	if len(ms) != 0 {
		t.Fatalf("msgs = %#v", ms)
	}
}

func writeRun(t *testing.T, dir, id, state string, created time.Time, steps map[string]apply.StepRecord) {
	t.Helper()
	d := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	j := map[string]any{"runId": id, "state": state, "created": created, "steps": steps, "changes": map[string]any{}}
	b, _ := json.Marshal(j)
	if err := os.WriteFile(filepath.Join(d, "journal.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "changeset.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withRuns(t *testing.T, f fixture) {
	t.Helper()
	dir := t.TempDir()
	st := func(s string) apply.StepRecord { return apply.StepRecord{Status: s} }
	writeRun(t, dir, "r0", apply.RunFinished, now.Add(-3*time.Hour), map[string]apply.StepRecord{"a": st(apply.StatusDone)})
	writeRun(t, dir, "r1", apply.RunFinished, now.Add(-2*time.Hour), map[string]apply.StepRecord{"a": st(apply.StatusDone)})
	writeRun(t, dir, "r2", apply.RunFailed, now.Add(-time.Hour), map[string]apply.StepRecord{
		"a": st(apply.StatusDone), "b": st(apply.StatusDone), "c": st(apply.StatusFailed), "d": st(apply.StatusBlocked), "e": st(apply.StatusPending)})
	writeRun(t, dir, "r3", apply.RunRunning, now, map[string]apply.StepRecord{
		"a": st(apply.StatusDone), "b": st(apply.StatusStarted), "c": st(apply.StatusPending)})
	f.ctx.Runs = &apply.Runs{Dir: dir}
	f.ctx.RunIndex = indexOf(t, f.ctx.Runs)
	f.ctx.Running = map[string]tui.RunProgress{"r3": {Done: 3, Total: 7, State: apply.RunRunning}}
}

func TestChangeSetRunsShowUnfinishedAndLastFinished(t *testing.T) {
	f := newCtx(t, 80, 24)
	withRuns(t, f)
	v := body(f.ctx, cs.New())
	for _, want := range []string{"⟳ r3 running 3/7", "✗ r2 failed 2/5", "✓ r1 finished 1/1"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "r0") {
		t.Errorf("older finished run shown:\n%s", v)
	}
}

func TestChangeSetEnterOpensRun(t *testing.T) {
	f := newCtx(t, 80, 24)
	withRuns(t, f)
	// runs are newest first: r3 is the first cursor row
	_, ms := press(f.ctx, cs.New(), enter)
	want := tui.OpenMsg{Name: "run", Args: []any{"r3"}}
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], want) {
		t.Fatalf("msgs = %#v", ms)
	}
}

func TestChangeSetBuild(t *testing.T) {
	if cs.Build() == nil {
		t.Error("Build() = nil")
	}
	if cs.Build("x") != nil {
		t.Error("Build with args should be nil")
	}
	if cs.Build("select", []string{"a1"}) == nil {
		t.Error("Build(\"select\", ids) = nil")
	}
}

func TestChangeSetGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		f := newCtx(t, w, 24)
		withRuns(t, f)
		f.ctx.CS.Changes[0].Flag = apply.FlagNotDeployed
		f.ctx.CS.RedeployOff = []string{"e1/s2"}
		// the second change is held by a run
		f.ctx.CS.Changes[1].RunID = "r3"
		s := cs.New()
		s, _ = press(f.ctx, s, down, down, down, space) // select the flagged change
		out := frame(f.ctx, s)
		tuitest.NoSecrets(t, out, secret, "new-API_KEY", "new-QUEUE")
		tuitest.Golden(t, "changeset_"+map[int]string{80: "80", 60: "60"}[w]+".golden", out)
	}
}

func confirmKeys() []tea.KeyMsg {
	return append(append([]tea.KeyMsg{r("a")}, typed("apply")...), enter)
}

func TestChangeSetSelectionDropsVanishedAndLocked(t *testing.T) {
	f := newCtx(t, 80, 24)
	s, _ := press(f.ctx, cs.New(), space, down, space) // both selected
	f.ctx.CS.Changes[0].RunID = "r9"                   // first locks
	f.ctx.CS.Rev++
	s, ms := press(f.ctx, s, confirmKeys()...)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	am, ok := ms[0].(tui.ApplyMsg)
	if !ok || len(am.IDs) != 1 || am.IDs[0] != f.ctx.CS.Changes[1].ID {
		t.Fatalf("msg = %#v", ms[0])
	}
	_ = s
}

func TestChangeSetConfirmRaceLockedNoApplyAll(t *testing.T) {
	f := newCtx(t, 80, 24)
	s, _ := press(f.ctx, cs.New(), space, r("a"))
	f.ctx.CS.Changes[0].RunID = "r9" // locks while the confirm is open
	f.ctx.CS.Rev++
	_, ms := press(f.ctx, s, append(typed("apply"), enter)...)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	fl, ok := ms[0].(tui.FlashMsg)
	if !ok || fl.Kind != '!' || !strings.Contains(fl.Text, "nothing left to apply") {
		t.Fatalf("msg = %#v", ms[0])
	}
}

func TestChangeSetConfirmRaceRemoved(t *testing.T) {
	f := newCtx(t, 80, 24)
	s, _ := press(f.ctx, cs.New(), space, r("a"))
	f.ctx.CS.Changes = f.ctx.CS.Changes[1:]
	f.ctx.CS.Rev++
	_, ms := press(f.ctx, s, append(typed("apply"), enter)...)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if _, ok := ms[0].(tui.FlashMsg); !ok {
		t.Fatalf("msg = %#v", ms[0])
	}
}

func TestChangeSetStrayConfirmedIgnored(t *testing.T) {
	f := newCtx(t, 80, 24)
	s := cs.New()
	for _, m := range []tea.Msg{components.ConfirmedMsg{}, components.CancelMsg{}} {
		_, cmd := s.Update(f.ctx, m)
		if cmd != nil {
			t.Errorf("%T emitted a command", m)
		}
	}
}

func TestChangeSetRemoveLockedFlashes(t *testing.T) {
	f := newCtx(t, 80, 24)
	f.ctx.CS.Changes[0].RunID = "r9"
	_, ms := press(f.ctx, cs.New(), r("x"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if fl, ok := ms[0].(tui.FlashMsg); !ok || fl.Kind != '✗' {
		t.Fatalf("msg = %#v", ms[0])
	}
}

func TestChangeSetFilterNarrowsRows(t *testing.T) {
	f := newCtx(t, 80, 24)
	s, _ := press(f.ctx, cs.New(), r("/"), r("queue"))
	if !s.(tui.Capturer).Capturing() {
		t.Fatal("filter should capture keys")
	}
	out := body(f.ctx, s)
	if strings.Contains(out, "API_KEY") || !strings.Contains(out, "QUEUE") || !strings.Contains(out, "/ queue") {
		t.Errorf("filtered:\n%s", out)
	}
}

func TestChangeSetFilterKeepsHiddenSelection(t *testing.T) {
	f := newCtx(t, 80, 24)
	s, _ := press(f.ctx, cs.New(), space) // select the first change (API_KEY)
	s, _ = press(f.ctx, s, r("/"), r("queue"), enter, r("j"))
	if v := body(f.ctx, s); strings.Contains(v, "API_KEY") {
		t.Fatalf("API_KEY should be hidden:\n%s", v)
	}
	// the apply prompt counts the hidden selected change
	s, _ = press(f.ctx, s, r("a"))
	if v := body(f.ctx, s); !strings.Contains(v, "apply 1 selected") {
		t.Errorf("prompt should count the hidden selection:\n%s", v)
	}
	s, _ = press(f.ctx, s, tea.KeyMsg{Type: tea.KeyEsc})
	s, _ = press(f.ctx, s, r("/"), esc, r("j"))
	s, _ = press(f.ctx, s, r("a"))
	if v := body(f.ctx, s); !strings.Contains(v, "apply 1 selected") || !strings.Contains(v, "* API_KEY") {
		t.Errorf("selection lost:\n%s", v)
	}
}

func TestChangeSetFilterDropsEmptyHeadings(t *testing.T) {
	f := newCtx(t, 80, 24)
	withRuns(t, f)
	s, _ := press(f.ctx, cs.New(), r("/"), r("queue"))
	v := body(f.ctx, s)
	for _, l := range strings.Split(v, "\n") {
		if strings.TrimSpace(l) == "runs" || strings.TrimSpace(l) == "redeploys" {
			t.Errorf("heading %q kept:\n%s", l, v)
		}
	}
}

func TestChangeSetConfirmLineSurvivesTinyBody(t *testing.T) {
	f := newCtx(t, 80, tui.ChromeRows+2)
	s, _ := press(f.ctx, cs.New(), r("/"), r("queue"), enter, r("a"))
	v := body(f.ctx, s)
	if n := len(strings.Split(v, "\n")); n > 2 || !strings.Contains(v, "apply") {
		t.Errorf("confirm line lost or body too tall (%d lines):\n%s", n, v)
	}
}

// withInvalid adds two changes as a corrupt change set file could hold them:
// limits whose data its kind refuses on api, and a kind this build does not
// know on worker (its name carrying an escape sequence).
func withInvalid(f fixture) (limitsID, unknownID string) {
	f.ctx.CS.Changes = append(f.ctx.CS.Changes,
		changeset.Change{ID: "bad-limits", Kind: "limits.set", Place: place("s1"), Target: "limits", New: json.RawMessage(`{"cpu":2}`), Created: now},
		changeset.Change{ID: "bad-kind", Kind: "service.\x1b[31mteleport", Place: place("s2"), Target: "teleport", Created: now},
	)
	f.ctx.CS.Rev++
	return "bad-limits", "bad-kind"
}

func lineWith(out, s string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			return strings.TrimRight(l, " ")
		}
	}
	return ""
}

func TestChangeSetInvalidRows(t *testing.T) {
	f := newCtx(t, 120, 24)
	withInvalid(f)
	out := body(f.ctx, cs.New())
	if l := lineWith(out, "limits"); l != "      limits  ✗ invalid: limits.set: bad change data: cpu and memoryBytes are both required" {
		t.Errorf("limits row = %q", l)
	}
	if l := lineWith(out, "teleport  "); l != "      teleport  ✗ invalid: unknown change kind service.teleport" {
		t.Errorf("unknown kind row = %q", l)
	}
	if l := lineWith(out, "API_KEY"); l != "      API_KEY  → 11 chars" {
		t.Errorf("valid row = %q", l)
	}
}

func TestChangeSetInvalidCannotBeSelectedOrApplied(t *testing.T) {
	f := newCtx(t, 80, 24)
	limitsID, _ := withInvalid(f)
	// rows: API_KEY, limits (api), QUEUE, teleport (worker)
	s, ms := press(f.ctx, cs.New(), down, space)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if fl, ok := ms[0].(tui.FlashMsg); !ok || fl.Kind != '✗' || fl.Text != "invalid: it cannot be applied · x removes it" {
		t.Fatalf("msg = %#v", ms[0])
	}
	if v := body(f.ctx, s); strings.Contains(v, "* limits") {
		t.Errorf("an invalid change was selected:\n%s", v)
	}
	s, _ = press(f.ctx, s, r("a"))
	if v := body(f.ctx, s); !strings.Contains(v, "apply all 2 changes · 2 invalid left out") {
		t.Errorf("prompt should count the valid changes only:\n%s", v)
	}
	s, _ = press(f.ctx, s, esc)
	// x removes it, like any change
	_, ms = press(f.ctx, s, r("x"))
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], tui.RemoveChangeMsg{ID: limitsID}) {
		t.Fatalf("x: msgs = %#v", ms)
	}
}

func TestChangeSetOnlyInvalidNothingToApply(t *testing.T) {
	f := newCtx(t, 80, 24)
	withInvalid(f)
	f.ctx.CS.Changes = f.ctx.CS.Changes[2:]
	f.ctx.CS.Rev++
	_, ms := press(f.ctx, cs.New(), r("a"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if fl, ok := ms[0].(tui.FlashMsg); !ok || fl.Text != "nothing to apply: every change left is invalid · x removes it" {
		t.Fatalf("msg = %#v", ms[0])
	}
}

func TestChangeSetInvalidGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		f := newCtx(t, w, 24)
		withInvalid(f)
		out := frame(f.ctx, cs.New())
		tuitest.NoSecrets(t, out, secret, "new-API_KEY", "new-QUEUE")
		tuitest.Golden(t, "changeset_invalid_"+map[int]string{80: "80", 60: "60"}[w]+".golden", out)
	}
}

// indexOf is the runs index the root would read for r.
func indexOf(t *testing.T, r *apply.Runs) tui.RunIndex {
	t.Helper()
	infos, errs := r.List()
	if len(errs) != 0 {
		t.Fatalf("List: %v", errs)
	}
	return tui.RunIndex{Loaded: true, Runs: infos}
}

// withDrift lets Railway move under both changes once the inventory is read:
// API_KEY holds another value, QUEUE is gone (the worker still holds another
// variable, so its place was read).
func withDrift(f fixture) {
	f.ctx.Store.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "API_KEY", Unrendered: secret + "-rotated", Rendered: secret + "-rotated"},
		{EnvID: "e1", ServiceID: "s2", Name: "OTHER", Unrendered: "o", Rendered: "o"},
	})
	f.ctx.Store.FinishLoad(store.InventoryKey("p1"), now)
}

func TestChangeSetDriftRows(t *testing.T) {
	f := newCtx(t, 120, 24)
	withDrift(f)
	out := body(f.ctx, cs.New())
	if l := lineWith(out, "API_KEY"); l != "      API_KEY  → 11 chars  ⚠ drifted: value changed on Railway" {
		t.Errorf("API_KEY row = %q", l)
	}
	if l := lineWith(out, "QUEUE"); l != "      QUEUE  → 9 chars  ⚠ drifted: variable gone" {
		t.Errorf("QUEUE row = %q", l)
	}
	// a drifted change still applies
	s, _ := press(f.ctx, cs.New(), r("a"))
	if v := body(f.ctx, s); !strings.Contains(v, "apply all 2 changes") {
		t.Errorf("drifted changes must stay applicable:\n%s", v)
	}
	// the filter matches the drift reason
	s, _ = press(f.ctx, cs.New(), append([]tea.KeyMsg{r("/")}, typed("gone")...)...)
	if v := body(f.ctx, s); strings.Contains(v, "API_KEY") || !strings.Contains(v, "QUEUE") {
		t.Errorf("filter on the reason:\n%s", v)
	}
}

func TestChangeSetDriftGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		f := newCtx(t, w, 24)
		withDrift(f)
		out := frame(f.ctx, cs.New())
		tuitest.NoSecrets(t, out, secret, "new-API_KEY", "new-QUEUE")
		tuitest.Golden(t, "changeset_drift_"+map[int]string{80: "80", 60: "60"}[w]+".golden", out)
	}
}

// The drift mark is an icon: its Nerd form under Nerd icons.
func TestChangeSetDriftMarkIcon(t *testing.T) {
	f := newCtx(t, 120, 24)
	withDrift(f)
	f.ctx.Icons = tui.Icons{Nerd: true}
	out := body(f.ctx, cs.New())
	if l := lineWith(out, "QUEUE"); !strings.Contains(l, f.ctx.Icons.Drift()+" drifted: variable gone") || strings.Contains(l, "⚠") {
		t.Errorf("QUEUE row = %q", l)
	}
}

// withCoverage gives API_KEY a long value a worker variable holds too and
// puts the API_KEY change in the rotate group of that value: 1 of 2 places. With two, a TOKEN
// change in its own group, held at both places, joins it: 1 of 2 again.
// QUEUE goes into the group of its value, changed at its only place.
func withCoverage(t *testing.T, f fixture, two bool) {
	t.Helper()
	recs := []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "API_KEY", Unrendered: apiValue, Rendered: apiValue},
		{EnvID: "e1", ServiceID: "s2", Name: "API_COPY", Unrendered: apiValue, Rendered: apiValue},
		{EnvID: "e1", ServiceID: "s2", Name: "QUEUE", Unrendered: "q", Rendered: "q"},
	}
	if two {
		recs = append(recs,
			railway.VarRecord{EnvID: "e1", ServiceID: "s1", Name: "TOKEN", Unrendered: tokenValue, Rendered: tokenValue},
			railway.VarRecord{EnvID: "e1", ServiceID: "s2", Name: "TOKEN", Unrendered: tokenValue, Rendered: tokenValue})
	}
	f.ctx.Store.ReplaceVariables("p1", recs)
	fp := func(svc, name string) string {
		for _, v := range f.ctx.Store.VariablesAt(place(svc)) {
			if v.Name == name {
				return v.Fingerprint
			}
		}
		t.Fatalf("no %s", name)
		return ""
	}
	f.ctx.CS.Changes[0].Group = rotation.GroupKey(fp("s1", "API_KEY"))
	f.ctx.CS.Changes[1].Group = rotation.GroupKey(fp("s2", "QUEUE"))
	if two {
		c, err := kinds.SetVariable(f.ctx.Store, f.ctx.CS.Changes, place("s1"), "TOKEN", "new-TOKEN", now)
		if err != nil {
			t.Fatal(err)
		}
		c.Group = rotation.GroupKey(fp("s1", "TOKEN"))
		if err := f.ctx.CS.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.ctx.CS.Rev++
}

// Values of 20+ characters share a fingerprint across names.
const (
	apiValue   = "fixture-api-value-charlie"
	tokenValue = "fixture-token-value-bravo"
)

func TestChangeSetCoverageLines(t *testing.T) {
	f := newCtx(t, 120, 24)
	withCoverage(t, f, false)
	s := cs.New()
	out := body(f.ctx, s)
	if l := lineWith(out, "places"); l != "  API_COPY = API_KEY · 1 of 2 places · 1 keeps the old value" {
		t.Errorf("coverage line = %q\n%s", l, out)
	}
	if strings.Contains(out, "QUEUE ·") {
		t.Errorf("a fully rotated group shows coverage:\n%s", out)
	}
	s, _ = press(f.ctx, s, r("a"))
	out = body(f.ctx, s)
	if l := lineWith(out, "apply all"); !strings.HasPrefix(l, "apply all 2 changes type") {
		t.Errorf("prompt = %q", l)
	}
	if l := lineWith(out, "partly"); l != "! 1 group partly rotated" {
		t.Errorf("note = %q\n%s", l, out)
	}
	s, _ = press(f.ctx, s, esc)
	if out := body(f.ctx, s); strings.Contains(out, "partly") {
		t.Errorf("note stays after esc:\n%s", out)
	}
	s, _ = press(f.ctx, s, space, r("a")) // API_KEY, in the partial group
	if l := lineWith(body(f.ctx, s), "partly"); l != "! 1 group partly rotated" {
		t.Errorf("selected note = %q", l)
	}
	// a selection outside every partial group: no note
	s, _ = press(f.ctx, cs.New(), down, space, r("a")) // QUEUE
	out = body(f.ctx, s)
	if l := lineWith(out, "apply 1"); !strings.HasPrefix(l, "apply 1 selected type") {
		t.Errorf("selected prompt = %q", l)
	}
	if strings.Contains(out, "partly") {
		t.Errorf("note for a selection outside the group:\n%s", out)
	}

	f = newCtx(t, 120, 24)
	withCoverage(t, f, true)
	s = cs.New()
	out = body(f.ctx, s)
	if l := lineWith(out, "TOKEN ·"); l != "  TOKEN · 1 of 2 places · 1 keeps the old value" {
		t.Errorf("second coverage line = %q\n%s", l, out)
	}
	s, _ = press(f.ctx, s, r("a"))
	if l := lineWith(body(f.ctx, s), "partly"); l != "! 2 groups partly rotated" {
		t.Errorf("note = %q", l)
	}
	tuitest.NoSecrets(t, frame(f.ctx, s), secret, apiValue, tokenValue, "new-API_KEY", "new-QUEUE", "new-TOKEN")
}

func TestChangeSetCoverageGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		f := newCtx(t, w, 24)
		withCoverage(t, f, true)
		s, _ := press(f.ctx, cs.New(), append([]tea.KeyMsg{r("a")}, typed("app")...)...)
		out := frame(f.ctx, s)
		tuitest.NoSecrets(t, out, secret, apiValue, tokenValue, "new-API_KEY", "new-QUEUE", "new-TOKEN")
		tuitest.Golden(t, "changeset_coverage_"+map[int]string{80: "80", 60: "60"}[w]+".golden", out)
	}
}

func TestChangeSetPreselect(t *testing.T) {
	f := newCtx(t, 80, 24)
	f.ctx.CS.Changes[0].RunID = "r9" // locked: never selected
	s := cs.Build("select", []string{f.ids[0], f.ids[1], "gone"})
	s.Init(f.ctx)
	out := body(f.ctx, s)
	if !strings.Contains(out, "* QUEUE") || strings.Contains(out, "* API_KEY") {
		t.Errorf("preselection:\n%s", out)
	}
	s, _ = press(f.ctx, s, r("a"))
	if l := lineWith(body(f.ctx, s), "apply"); !strings.Contains(l, "apply 1 selected") {
		t.Errorf("prompt = %q", l)
	}
	_, ms := press(f.ctx, s, append(typed("apply"), enter)...)
	want := tui.ApplyMsg{IDs: []string{f.ids[1]}}
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], want) {
		t.Fatalf("msgs = %#v, want %#v", ms, want)
	}
}

// With the partly rotated note up, a one-line body keeps the confirm line.
func TestChangeSetConfirmWithNoteSurvivesOneLineBody(t *testing.T) {
	f := newCtx(t, 80, tui.ChromeRows+1)
	withCoverage(t, f, false)
	s, _ := press(f.ctx, cs.New(), r("a"))
	if v := body(f.ctx, s); strings.Contains(v, "\n") || !strings.HasPrefix(v, "apply all 2 changes") {
		t.Errorf("body = %q", v)
	}
}

// A copy says where its value comes from.
func TestChangeSetShowsCopySource(t *testing.T) {
	f := newCtx(t, 80, 24)
	src, _ := f.ctx.CS.Get(f.ids[0])
	cp, err := kinds.CopyPlanned(f.ctx.Store, f.ctx.CS.Changes, place("s2"), "QUEUE", src, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ctx.CS.Put(cp, nil); err != nil {
		t.Fatal(err)
	}
	got := body(f.ctx, cs.New())
	if l := lineWith(got, "QUEUE"); !strings.Contains(l, "⇉ from production · api") {
		t.Errorf("QUEUE line %q:\n%s", l, got)
	}
}

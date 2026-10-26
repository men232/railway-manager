package variables_test

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/tui/screens/variables"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

const (
	secretA = "fixture-value-alpha"
	secretB = "fixture-value-bravo"
	secretC = "fixture-value-charlie"
	newVal  = "newvalue123"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

var place = domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}

var catalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1"}}},
	Services:     []railway.Service{{ID: "s1", Name: "api"}},
}}}

func ctx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(catalog)
	st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "API_KEY", Unrendered: secretA, Rendered: secretA, UpdatedAt: "2026-10-01T09:30:00"},
		{EnvID: "e1", ServiceID: "s1", Name: "DATABASE_URL", Unrendered: secretB, Rendered: secretB, UpdatedAt: "2026-10-02T10:15:00"},
		{EnvID: "e1", ServiceID: "s1", Name: "SESSION", Unrendered: secretC, Rendered: secretC, UpdatedAt: "2026-10-03T11:45:00"},
		{EnvID: "e1", ServiceID: "s1", Name: "SIGNING", Sealed: true, UpdatedAt: "2026-10-04T08:00:00"},
	})
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views:    &view.Views{Store: st, Reg: kinds.NewRegistry()},
		Settings: &settings.Settings{WriteEnvs: []string{"e1"}},
		Theme:    tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Now: func() time.Time { return now },
	}
}

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
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
		case components.SubmitMsg, components.CancelMsg, components.ConfirmedMsg:
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

// press sends keys one by one and collects every intent.
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

func flashOf(t *testing.T, ms []tea.Msg) tui.FlashMsg {
	t.Helper()
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	f, ok := ms[0].(tui.FlashMsg)
	if !ok {
		t.Fatalf("msg = %#v", ms[0])
	}
	return f
}

func TestVariablesEditFlow(t *testing.T) {
	c := ctx(80, 20)
	s := variables.New(place) // cursor on API_KEY
	s, _ = press(c, s, r("e"))
	if !s.(tui.Capturer).Capturing() {
		t.Fatal("edit should capture keys")
	}
	s, _ = press(c, s, r(newVal))
	if f := frame(c, s); strings.Contains(f, newVal) {
		t.Fatalf("frame leaks the value:\n%s", f)
	}
	s, ms := press(c, s, enter)
	if len(ms) != 0 {
		t.Fatalf("first enter emitted %#v", ms)
	}
	s, _ = press(c, s, r(newVal))
	if f := frame(c, s); strings.Contains(f, newVal) {
		t.Fatalf("repeat frame leaks the value:\n%s", f)
	}
	s, ms = press(c, s, enter)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	pm, ok := ms[0].(tui.PutChangeMsg)
	if !ok || pm.Change.Kind != "variable.set" || pm.Change.Target != "API_KEY" || pm.Change.Place != place || !pm.Change.Created.Equal(now) {
		t.Fatalf("msg = %#v", ms[0])
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("still capturing after submit")
	}
	if f := frame(c, s); strings.Contains(f, newVal) {
		t.Errorf("final frame leaks the value:\n%s", f)
	}
}

func TestVariablesEditResolvesPendingReference(t *testing.T) {
	c := ctx(80, 20)
	add, err := kinds.SetVariable(c.Store, nil, place, "NEWBASE", "x", now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(add, nil)
	s, _ := press(c, variables.New(place), r("e"), r("${{NEWBASE}}"), enter, r("${{NEWBASE}}"))
	_, ms := press(c, s, enter)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if _, ok := ms[0].(tui.PutChangeMsg); !ok {
		t.Fatalf("a reference to a pending variable should resolve, got %#v", ms[0])
	}
}

func TestVariablesEditMismatchKeepsInput(t *testing.T) {
	c := ctx(80, 20)
	s, ms := press(c, variables.New(place), r("e"), r("one"), enter, r("two"), enter)
	f := flashOf(t, ms)
	if f.Kind != '✗' || !strings.Contains(f.Text, "differ") {
		t.Errorf("flash = %+v", f)
	}
	if !s.(tui.Capturer).Capturing() {
		t.Error("input should stay open after a mismatch")
	}
}

func TestVariablesEditCancel(t *testing.T) {
	c := ctx(80, 20)
	s, ms := press(c, variables.New(place), r("e"), r("abc"), tea.KeyMsg{Type: tea.KeyEsc})
	if len(ms) != 0 || s.(tui.Capturer).Capturing() {
		t.Errorf("msgs = %#v capturing = %v", ms, s.(tui.Capturer).Capturing())
	}
}

func TestVariablesAddFlow(t *testing.T) {
	c := ctx(80, 20)
	s, _ := press(c, variables.New(place), r("a"), r("NEW_ONE"), enter)
	if v := frame(c, s); !strings.Contains(v, "NEW_ONE") {
		t.Errorf("value prompt should name the variable:\n%s", v)
	}
	s, ms := press(c, s, r(newVal), enter, r(newVal), enter)
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	pm, ok := ms[0].(tui.PutChangeMsg)
	if !ok || pm.Change.Kind != "variable.set" || pm.Change.Target != "NEW_ONE" || !pm.Change.Base.Absent {
		t.Fatalf("msg = %#v", ms[0])
	}
	if strings.Contains(frame(c, s), newVal) {
		t.Error("frame leaks the value")
	}
}

func TestVariablesAddRefusesRailwayName(t *testing.T) {
	c := ctx(80, 20)
	s, ms := press(c, variables.New(place), r("a"), r("RAILWAY_X"), enter)
	f := flashOf(t, ms)
	if f.Kind != '✗' || !strings.Contains(f.Text, "managed by Railway") {
		t.Errorf("flash = %+v", f)
	}
	if !s.(tui.Capturer).Capturing() {
		t.Error("the name input should stay open")
	}
}

func TestVariablesAddRefusesExistingName(t *testing.T) {
	c := ctx(80, 20)
	_, ms := press(c, variables.New(place), r("a"), r("API_KEY"), enter)
	if f := flashOf(t, ms); f.Kind != '!' || !strings.Contains(f.Text, "exists") {
		t.Errorf("flash = %+v", f)
	}
}

func TestVariablesDeleteAndUndo(t *testing.T) {
	c := ctx(80, 20)
	s, ms := press(c, variables.New(place), r("x"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	pm, ok := ms[0].(tui.PutChangeMsg)
	if !ok || pm.Change.Kind != "variable.delete" || pm.Change.Target != "API_KEY" {
		t.Fatalf("msgs = %#v", ms)
	}
	// nothing pending: undo flashes
	_, ms = press(c, s, r("u"))
	if f := flashOf(t, ms); f.Kind != '!' {
		t.Errorf("flash = %+v", f)
	}
	// with the delete pending, undo removes that change
	c.CS.Put(pm.Change, nil)
	_, ms = press(c, s, r("u"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	rm, ok := ms[0].(tui.RemoveChangeMsg)
	if !ok || rm.ID != c.CS.Changes[0].ID {
		t.Fatalf("msgs = %#v", ms)
	}
}

func TestVariablesRevealClearsOnMove(t *testing.T) {
	c := ctx(80, 20)
	s, _ := press(c, variables.New(place), r("v"))
	if !strings.Contains(body(c, s), secretA) {
		t.Fatal("reveal missing")
	}
	if strings.Contains(body(c, s), secretB) {
		t.Error("only the cursor row is revealed")
	}
	s, _ = press(c, s, down)
	if out := body(c, s); strings.Contains(out, secretA) || strings.Contains(out, secretB) {
		t.Error("reveal should clear on move")
	}
	// v again hides
	s, _ = press(c, s, r("v"))
	s, _ = press(c, s, r("v"))
	if strings.Contains(body(c, s), secretB) {
		t.Error("v again should hide")
	}
	// any other key hides
	s, _ = press(c, s, r("v"))
	s, _ = press(c, s, r("z"))
	if strings.Contains(body(c, s), secretB) {
		t.Error("another key should hide")
	}
	// suspend hides
	s, _ = press(c, s, r("v"))
	s.(tui.Suspender).Suspend(c)
	if strings.Contains(body(c, s), secretB) {
		t.Error("suspend should hide")
	}
}

func TestVariablesRevealNeedsTTY(t *testing.T) {
	c := ctx(80, 20)
	c.IsTTY = false
	s, ms := press(c, variables.New(place), r("v"))
	if f := flashOf(t, ms); f.Kind != '!' || f.Text != "reveal needs a terminal" {
		t.Errorf("flash = %+v", f)
	}
	if strings.Contains(body(c, s), secretA) {
		t.Error("revealed without a terminal")
	}
}

func TestVariablesRevealSanitizes(t *testing.T) {
	c := ctx(80, 20)
	c.Store.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "A", Unrendered: "x\x1b[31mred\x1b[0m", Rendered: "x\x1b[31mred\x1b[0m"},
	})
	s, _ := press(c, variables.New(place), r("v"))
	if out := s.View(c, 76, 10); strings.Contains(out, "\x1b[31m") {
		t.Error("escape sequence reached the frame")
	}
}

func TestVariablesRevealSealed(t *testing.T) {
	c := ctx(80, 20)
	_, ms := press(c, variables.New(place), down, down, down, r("v"))
	if f := flashOf(t, ms); f.Kind != '!' || !strings.Contains(f.Text, "sealed") {
		t.Errorf("flash = %+v", f)
	}
}

func TestVariablesLockedRow(t *testing.T) {
	c := ctx(80, 20)
	ch, err := kinds.SetVariable(c.Store, nil, place, "API_KEY", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	ch.RunID = "r1"
	c.CS.Changes = append(c.CS.Changes, ch) // as the root holds it during a run
	c.CS.Rev++
	s := variables.New(place)
	if out := body(c, s); !strings.Contains(out, "⟳ r1") {
		t.Errorf("lock marker missing:\n%s", out)
	}
	for _, k := range []string{"e", "x", "u"} {
		s2, ms := press(c, s, r(k))
		f := flashOf(t, ms)
		if f.Kind != '✗' || f.Text != "in run r1" {
			t.Errorf("%s: flash = %+v", k, f)
		}
		if s2.(tui.Capturer).Capturing() {
			t.Errorf("%s opened an input on a locked row", k)
		}
	}
}

func TestVariablesMissingRowShown(t *testing.T) {
	c := ctx(80, 20)
	ch, err := kinds.SetVariable(c.Store, nil, place, "SESSION", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(ch, nil)
	c.Store.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "API_KEY", Unrendered: secretA, Rendered: secretA},
	})
	if out := body(c, variables.New(place)); !strings.Contains(out, "SESSION") || !strings.Contains(out, "! gone from Railway") {
		t.Errorf("missing row not flagged:\n%s", out)
	}
}

func TestVariablesBuild(t *testing.T) {
	if variables.Build(place) == nil {
		t.Error("Build(place) = nil")
	}
	if variables.Build() == nil {
		t.Error("Build() = nil: no argument is the tree")
	}
	for _, args := range [][]any{{"x"}, {place, place}, {&place}} {
		if variables.Build(args...) != nil {
			t.Errorf("Build(%v) should be nil", args)
		}
	}
}

func TestVariablesGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 18)
		for _, op := range []func() (changeset.Change, error){
			func() (changeset.Change, error) {
				return kinds.SetVariable(c.Store, nil, place, "API_KEY", newVal, now)
			},
			func() (changeset.Change, error) {
				return kinds.SetVariable(c.Store, nil, place, "NEW_ONE", newVal, now)
			},
			func() (changeset.Change, error) { return kinds.DeleteVariable(c.Store, place, "SESSION", now) },
		} {
			ch, err := op()
			if err != nil {
				t.Fatal(err)
			}
			c.CS.Put(ch, nil)
		}
		s := variables.New(place)
		out := frame(c, s)
		tuitest.NoSecrets(t, out, secretA, secretB, secretC, newVal)
		tuitest.Golden(t, "variables_"+map[int]string{80: "80", 60: "60"}[w]+".golden", out)
	}
}

func TestVariablesDeletePendingAddRemovesTheChange(t *testing.T) {
	c := ctx(80, 20)
	add, err := kinds.SetVariable(c.Store, nil, place, "AAA_NEW", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(add, nil)
	// AAA_NEW sorts first
	_, ms := press(c, variables.New(place), r("x"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	rm, ok := ms[0].(tui.RemoveChangeMsg)
	if !ok || rm.ID != c.CS.Changes[0].ID {
		t.Fatalf("msg = %#v", ms[0])
	}
}

func TestVariablesDeleteMissingRowRemovesTheChange(t *testing.T) {
	c := ctx(80, 20)
	ch, err := kinds.SetVariable(c.Store, nil, place, "API_KEY", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(ch, nil)
	c.Store.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "SESSION", Unrendered: secretC, Rendered: secretC},
	})
	s := variables.New(place) // API_KEY (gone) sorts first
	if out := body(c, s); !strings.Contains(out, "gone from Railway") {
		t.Fatalf("row not flagged:\n%s", out)
	}
	_, ms := press(c, s, r("x"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if rm, ok := ms[0].(tui.RemoveChangeMsg); !ok || rm.ID != c.CS.Changes[0].ID {
		t.Fatalf("msg = %#v", ms[0])
	}
}

func TestVariablesNilStoreIsInert(t *testing.T) {
	c := ctx(80, 20)
	c.Store = nil
	s := variables.New(place)
	if out := s.View(c, 76, 10); out != "" {
		t.Errorf("view = %q", out)
	}
	if _, cmd := s.Update(c, r("a")); cmd != nil {
		t.Error("update acted without a store")
	}
}

func TestVariablesFilterNarrowsAndRemembersPerPlace(t *testing.T) {
	c := ctx(80, 20)
	s := variables.New(place)
	s, _ = press(c, s, r("/"), r("SESS"))
	if !s.(tui.Capturer).Capturing() {
		t.Fatal("filter should capture keys")
	}
	out := body(c, s)
	if !strings.Contains(out, "SESSION") || strings.Contains(out, "API_KEY") || !strings.Contains(out, "1/4") {
		t.Errorf("filtered:\n%s", out)
	}
	s, _ = press(c, s, enter)
	// the same place reopens with the filter, another place does not
	if out := body(c, variables.New(place)); strings.Contains(out, "API_KEY") {
		t.Errorf("same place should keep its filter:\n%s", out)
	}
	other := domain.Place{ProjectID: "p1", EnvID: "e1"}
	if out := body(c, variables.New(other)); strings.Contains(out, "/ SESS") {
		t.Errorf("other place leaked the filter:\n%s", out)
	}
}

func TestVariablesFilterGoldens(t *testing.T) {
	c := ctx(60, 18)
	s, _ := press(c, variables.New(place), r("/"), r("S"))
	out := frame(c, s)
	tuitest.NoSecrets(t, out, secretA, secretB, secretC, newVal)
	tuitest.Golden(t, "variables_filter_60.golden", out)
}

func TestVariablesFilterCursorClampsAndReleasesKeys(t *testing.T) {
	c := ctx(80, 20)
	s, _ := press(c, variables.New(place), down, down, down) // SIGNING
	s, _ = press(c, s, r("/"), r("SESS"), enter)
	if s.(tui.Capturer).Capturing() {
		t.Error("enter should release keys")
	}
	_, ms := press(c, s, r("x"))
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	if pm, ok := ms[0].(tui.PutChangeMsg); !ok || pm.Change.Target != "SESSION" {
		t.Errorf("x should delete the shown row: %#v", ms[0])
	}
}

func TestVariablesAddHiddenByFilterFlashes(t *testing.T) {
	c := ctx(80, 20)
	s, _ := press(c, variables.New(place), r("/"), r("SESS"), enter)
	_, ms := press(c, s, r("a"), r("NEW_ONE"), enter, r(newVal), enter, r(newVal), enter)
	var put, flashed bool
	for _, m := range ms {
		switch v := m.(type) {
		case tui.PutChangeMsg:
			put = true
		case tui.FlashMsg:
			flashed = strings.Contains(v.Text, "hidden by the filter")
		}
	}
	if !put || !flashed {
		t.Errorf("put=%v flashed=%v msgs=%#v", put, flashed, ms)
	}
}

// A pending edit whose variable changed on Railway since is marked in Warn,
// once the inventory has been read; the value is never shown.
func TestVariablesDriftedRowMarked(t *testing.T) {
	c := ctx(120, 20)
	ch, err := kinds.SetVariable(c.Store, nil, place, "SESSION", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(ch, nil)
	c.Store.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "SESSION", Unrendered: secretB, Rendered: secretB},
	})
	if out := body(c, variables.New(place)); strings.Contains(out, "drifted") {
		t.Errorf("drift shown before the inventory read:\n%s", out)
	}
	c.Store.FinishLoad(store.InventoryKey("p1"), now)
	out := body(c, variables.New(place))
	if !strings.Contains(out, "→ 11 chars  ⚠ drifted: value changed on Railway") {
		t.Errorf("drift not marked:\n%s", out)
	}
	tuitest.NoSecrets(t, out, secretA, secretB, secretC, newVal)
}

// The drift mark is an icon: its Nerd form under Nerd icons.
func TestVariablesDriftMarkIcon(t *testing.T) {
	c := ctx(120, 20)
	c.Icons = tui.Icons{Nerd: true}
	ch, err := kinds.SetVariable(c.Store, nil, place, "SESSION", newVal, now)
	if err != nil {
		t.Fatal(err)
	}
	c.CS.Put(ch, nil)
	c.Store.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "SESSION", Unrendered: secretB, Rendered: secretB},
	})
	c.Store.FinishLoad(store.InventoryKey("p1"), now)
	out := body(c, variables.New(place))
	if !strings.Contains(out, c.Icons.Drift()+" drifted: value changed on Railway") || strings.Contains(out, "⚠") {
		t.Errorf("Nerd drift mark missing:\n%s", out)
	}
}

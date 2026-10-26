package root_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/root"
	"github.com/men232/railway-manager/internal/tui/screens/picker"
	runscreen "github.com/men232/railway-manager/internal/tui/screens/run"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
	"github.com/men232/railway-manager/internal/workdir"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

const (
	secretA = "fixture-value-alpha"
	secretB = "fixture-value-bravo"
)

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

var catalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2"}}},
	Services:     []railway.Service{{ID: "s1", Name: "api"}, {ID: "s2", Name: "web"}},
}}}

type rig struct {
	t    *testing.T
	m    *root.Model
	fake *railwaytest.Fake
	dir  string
	st   *store.Store
	cs   *changeset.ChangeSet
	runs *apply.Runs
	sent chan tea.Msg
	set  *settings.Settings
	deps root.Deps
	snap *atomic.Pointer[store.Store]
}

type opt func(*root.Deps)

func newRig(t *testing.T, opts ...opt) *rig {
	t.Helper()
	f := railwaytest.New(t)
	f.SetCatalog(catalog)
	f.SetVar("e1", "s1", "DATABASE_URL", secretA, false)
	f.SetVar("e1", "s1", "API_KEY", secretB, false)
	f.SetVar("e1", "", "SHARED", "plain", false)
	f.SetActiveDeployment("e1", "s1", "dep-0")
	r := &rig{t: t, fake: f, dir: t.TempDir(), sent: make(chan tea.Msg, 1000), snap: &atomic.Pointer[store.Store]{}}
	writeEnvs := &atomic.Pointer[map[string]bool]{}
	client := &railway.Client{
		T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}},
		WriteAllowed: func(envID string) bool {
			w := writeEnvs.Load()
			return w != nil && (*w)[envID]
		},
	}
	r.st = store.New()
	reg := kinds.NewRegistry()
	compileOn := func(st func() *store.Store) func(cs changeset.ChangeSet) (apply.Graph, error) {
		return func(cs changeset.ChangeSet) (apply.Graph, error) {
			return apply.Compile(cs, st(), reg,
				func(envID string, changes []changeset.Change) apply.Step {
					return kinds.NewPatchStep(envID, changes, reg, changes[0].Place.ProjectID)
				},
				kinds.NewRedeployStep)
		}
	}
	r.runs = &apply.Runs{
		Dir: r.dir, Reg: reg, Exec: &apply.Executor{},
		NewEnv: func(runID string, j *apply.Journal) *apply.Env {
			return &apply.Env{API: client, Store: r.snap.Load(), Sleep: func(context.Context, time.Duration) error { return nil }}
		},
		NewRedeploy: kinds.NewRedeployStep,
		Snapshot:    r.snap.Load,
		Compile: func(s *store.Store, cs changeset.ChangeSet) (apply.Graph, error) {
			return compileOn(func() *store.Store { return s })(cs)
		},
	}
	r.cs = &changeset.ChangeSet{}
	r.set = &settings.Settings{Projects: []string{"p1"}, WriteEnvs: []string{"e1"}, Icons: "plain"}
	r.deps = root.Deps{
		Client: client, Store: r.st,
		Views: &view.Views{Store: r.st, Reg: reg, Compile: compileOn(func() *store.Store { return r.st })},
		Reg:   reg, Runs: r.runs, Settings: r.set,
		SettingsPath:  filepath.Join(r.dir, "settings.json"),
		ChangeSetPath: filepath.Join(r.dir, "changeset.json"),
		CS:            r.cs,
		Snapshot:      r.snap,
		WriteEnvs:     writeEnvs,
		Now:           func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) },
		Send:          func(m tea.Msg) { r.sent <- m },
		Theme:         tui.NewTheme(termenv.Ascii),
		Screens:       map[string]func(args ...any) tui.Screen{},
	}
	for _, o := range opts {
		o(&r.deps)
	}
	r.m = root.New(r.deps)
	r.m.Update(tea.WindowSizeMsg{Width: 160, Height: 24})
	return r
}

// update feeds msg and returns the command.
func (r *rig) update(msg tea.Msg) tea.Cmd {
	_, cmd := r.m.Update(msg)
	return cmd
}

// drain runs cmd and feeds every message it yields back into the model,
// recursively, until nothing is left. Messages sent through Deps.Send are fed
// too. It returns every message seen.
func (r *rig) drain(cmd tea.Cmd) []tea.Msg {
	var seen []tea.Msg
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 || len(r.sent) > 0 {
		var msg tea.Msg
		if len(r.sent) > 0 {
			msg = <-r.sent
		} else {
			c := queue[0]
			queue = queue[1:]
			if c == nil {
				continue
			}
			msg = c()
		}
		if msg == nil {
			continue
		}
		if b, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, b...)
			continue
		}
		if seq := sequenceOf(msg); seq != nil {
			queue = append(queue, seq...)
			continue
		}
		if _, ok := msg.(tea.QuitMsg); ok {
			seen = append(seen, msg)
			continue
		}
		seen = append(seen, msg)
		queue = append(queue, r.update(msg))
	}
	return seen
}

// prepare feeds an intent whose disk work runs in its commands (an apply or
// immediate waiting for the prepare lock, checking and freezing; a resume
// reading its run), then each command's message in turn, and returns the
// command that starts the run.
func (r *rig) prepare(msg tea.Msg) tea.Cmd {
	cmd := r.update(msg)
	if cmd == nil {
		r.t.Fatalf("%T returned no command", msg)
	}
	for {
		m := cmd()
		next := r.update(m)
		if !root.PrepareStep(m) || next == nil {
			return next
		}
		cmd = next
	}
}

func (r *rig) view() string { return ansi.Strip(r.m.View()) }

func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func setChange(target, value string) changeset.Change {
	nv, _ := json.Marshal(map[string]any{"value": value})
	return changeset.Change{
		Kind: "variable.set", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"},
		Target: target, New: nv, Base: changeset.Base{Absent: true},
	}
}

func TestAppRoutesInventoryToStore(t *testing.T) {
	r := newRig(t)
	r.update(loaders.CatalogMsg{Catalog: catalog})
	before := r.st.Rev(store.Variables)
	r.update(loaders.InventoryMsg{ProjectID: "p1", Result: railway.DiscoveryResult{ProjectID: "p1", Vars: []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "A", Unrendered: secretA, Rendered: secretA},
		{EnvID: "e1", ServiceID: "s2", Name: "B", Unrendered: secretB, Rendered: secretB},
	}}})
	if r.st.Rev(store.Variables) == before {
		t.Errorf("variables revision did not move")
	}
	v := r.view()
	if !strings.Contains(v, "2 variables") {
		t.Errorf("home count missing:\n%s", v)
	}
	tuitest.NoSecrets(t, v, secretA, secretB)
}

type testUpdate struct{ applied *bool }

func (u testUpdate) ApplyTo(s *store.Store) { *u.applied = true; s.BeginLoad("test") }

func TestAppAppliesAnyStoreUpdate(t *testing.T) {
	r := newRig(t)
	applied := false
	r.update(testUpdate{applied: &applied})
	if !applied || !r.st.Load("test").Loading {
		t.Errorf("store update not applied")
	}
}

// fakeScreen records Suspend and Resume.
type fakeScreen struct {
	title              string
	suspended, resumed int
	msgs               []tea.Msg
	capture            bool
}

type resumedMsg struct{ title string }

func (s *fakeScreen) Init(*tui.Ctx) tea.Cmd { return nil }
func (s *fakeScreen) Update(_ *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	s.msgs = append(s.msgs, msg)
	return s, nil
}
func (s *fakeScreen) View(*tui.Ctx, int, int) string { return s.title + " body" }
func (s *fakeScreen) Title() string                  { return s.title }
func (s *fakeScreen) Keys() []tui.KeyHint            { return []tui.KeyHint{{Key: "x", Label: "do " + s.title}} }
func (s *fakeScreen) Suspend(*tui.Ctx)               { s.suspended++ }
func (s *fakeScreen) Resume(*tui.Ctx) tea.Cmd {
	s.resumed++
	return func() tea.Msg { return resumedMsg{s.title} }
}
func (s *fakeScreen) Capturing() bool { return s.capture }

func TestAppSuspendResume(t *testing.T) {
	r := newRig(t)
	a, b := &fakeScreen{title: "A"}, &fakeScreen{title: "B"}
	r.update(tui.PushMsg{Screen: a})
	r.update(tui.PushMsg{Screen: b})
	if a.suspended != 1 || b.suspended != 0 {
		t.Fatalf("after push: A suspended %d, B %d", a.suspended, b.suspended)
	}
	if !strings.Contains(r.view(), "B body") {
		t.Fatalf("B not on top")
	}
	cmd := r.update(key("esc"))
	if a.resumed != 1 || b.suspended != 1 {
		t.Fatalf("after pop: A resumed %d, B suspended %d", a.resumed, b.suspended)
	}
	if cmd == nil {
		t.Fatalf("pop returned no command")
	}
	if msg, ok := cmd().(resumedMsg); !ok || msg.title != "A" {
		t.Errorf("pop command = %#v", msg)
	}
}

func TestAppAuthErrorPushesAuth(t *testing.T) {
	r := newRig(t)
	r.update(loaders.CatalogMsg{Err: &railway.AuthError{Msg: "no Railway token found"}})
	if v := r.view(); !strings.Contains(v, "railway-manager Sign in") || !strings.Contains(v, "no Railway token found") {
		t.Fatalf("auth screen not shown:\n%s", v)
	} else {
		tuitest.NoSecrets(t, v, "test-token")
	}
	// a second auth error does not stack another Auth
	r.update(loaders.CatalogMsg{Err: &railway.AuthError{Msg: "again"}})
	r.update(key("esc"))
	if v := r.view(); strings.Contains(v, "Sign in") {
		t.Errorf("auth stacked twice:\n%s", v)
	}
}

func TestAuthEnterSignsInAndReloads(t *testing.T) {
	r := newRig(t)
	r.update(loaders.AccountMsg{Err: &railway.AuthError{Msg: "token rejected"}})
	msgs := r.drain(r.update(key("enter")))
	if v := r.view(); strings.Contains(v, "Sign in") {
		t.Fatalf("auth not popped:\n%s", v)
	}
	var catalogs, inventories int
	for _, m := range msgs {
		switch m.(type) {
		case loaders.CatalogMsg:
			catalogs++
		case loaders.InventoryMsg:
			inventories++
		}
	}
	if catalogs != 1 || inventories != 1 {
		t.Errorf("catalog %d, inventory %d loads after sign-in", catalogs, inventories)
	}
	if v := r.view(); !strings.Contains(v, "fake@example.test") {
		t.Errorf("account missing:\n%s", v)
	}
}

func TestInventoryAlsoLoadsConfig(t *testing.T) {
	r := newRig(t)
	r.fake.SetEnvConfig("e1", railway.EnvConfig{Etag: "cfg-1"})
	r.drain(r.m.Init())
	if c, ok := r.st.Config("e1"); !ok || c.Etag != "cfg-1" {
		t.Errorf("config not loaded with the inventory: %+v, %v", c, ok)
	}
	if l := r.st.Load(store.ConfigKey("e1")); l.Loading || l.ReadAt.IsZero() {
		t.Errorf("config load = %+v", l)
	}
}

func TestStartupLoadsChain(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	v := r.view()
	for _, want := range []string{"1 project · 1 environment · 2 services · 3 variables", "fake@example.test"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	tuitest.NoSecrets(t, v, secretA, secretB)
}

func TestAppPutChangeSaves(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", secretA)}))
	info, err := os.Stat(r.deps.ChangeSetPath)
	if err != nil {
		t.Fatalf("change set not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	saved, err := changeset.Load(r.deps.ChangeSetPath)
	if err != nil || len(saved.Changes) != 1 {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	v := r.view()
	if !strings.Contains(v, "● 1 change") {
		t.Errorf("footer indicator missing:\n%s", v)
	}
	tuitest.NoSecrets(t, v, secretA)
	// invalid change: refused with a flash, nothing added
	bad := setChange("bad name", "x")
	r.drain(r.update(tui.PutChangeMsg{Change: bad}))
	if len(r.cs.Changes) != 1 || !strings.Contains(r.view(), "✗") {
		t.Errorf("invalid change accepted:\n%s", r.view())
	}
}

// plannedCopy is a set of target copying the planned value of change src.
func plannedCopy(target, value, src string) changeset.Change {
	nv, _ := json.Marshal(map[string]any{"value": value, "planned": map[string]string{"loc": domain.LocationID("e1", "s1", "A_NEW"), "change": src}})
	c := setChange(target, "")
	c.New = nv
	return c
}

// A copy of a planned value follows its source and goes with it.
func TestPlannedCopiesFollowAndGo(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "fixture-first-0001")}))
	aID := r.cs.Changes[0].ID
	r.drain(r.update(tui.PutChangeMsg{Change: plannedCopy("B_NEW", "fixture-first-0001", aID)}))
	r.drain(r.update(tui.PutChangeMsg{Change: plannedCopy("C_NEW", "fixture-first-0001", aID)}))
	if len(r.cs.Changes) != 3 {
		t.Fatalf("changes = %d", len(r.cs.Changes))
	}

	// a new value at the source: the copies follow
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "fixture-second-0002")}))
	for _, c := range r.cs.Changes {
		if v, _ := kinds.SetValue(c); v != "fixture-second-0002" {
			t.Errorf("%s holds the old value", c.Target)
		}
	}
	if v := r.view(); !strings.Contains(v, "2 copies follow") {
		t.Errorf("flash:\n%s", v)
	}

	// removing the source takes the copies
	r.drain(r.update(tui.RemoveChangeMsg{ID: aID}))
	if len(r.cs.Changes) != 0 || !strings.Contains(r.view(), "removed A_NEW · 2 copies of it too") {
		t.Errorf("changes left %d:\n%s", len(r.cs.Changes), r.view())
	}
	tuitest.NoSecrets(t, r.view(), "fixture-first-0001", "fixture-second-0002")
}

// Removing a source and its copy in one message is not a failure.
func TestPlannedCopyRemovedWithSource(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "fixture-first-0001")}))
	aID := r.cs.Changes[0].ID
	r.drain(r.update(tui.PutChangeMsg{Change: plannedCopy("B_NEW", "fixture-first-0001", aID)}))
	bID := r.cs.Changes[1].ID
	r.drain(r.update(tui.ChangesMsg{Remove: []string{aID, bID}, Done: "both undone"}))
	if len(r.cs.Changes) != 0 || !strings.Contains(r.view(), "✓ both undone") {
		t.Errorf("changes left %d:\n%s", len(r.cs.Changes), r.view())
	}
}

func TestRemoveReferencedChangeRefused(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	ref := setChange("B_NEW", "${{A_NEW}}")
	nv, _ := json.Marshal(map[string]any{"value": "${{A_NEW}}", "refs": []string{domain.LocationID("e1", "s1", "A_NEW")}})
	ref.New = nv
	r.drain(r.update(tui.PutChangeMsg{Change: ref}))
	if len(r.cs.Changes) != 2 {
		t.Fatalf("changes = %d: %s", len(r.cs.Changes), r.view())
	}
	aID := r.cs.Changes[0].ID
	r.drain(r.update(tui.RemoveChangeMsg{ID: aID}))
	if len(r.cs.Changes) != 2 || !strings.Contains(r.view(), "✗ A_NEW is referenced by a pending change: remove that first") {
		t.Errorf("referenced change removed:\n%s", r.view())
	}
	r.drain(r.update(tui.RemoveChangeMsg{ID: r.cs.Changes[1].ID}))
	r.drain(r.update(tui.RemoveChangeMsg{ID: aID}))
	if len(r.cs.Changes) != 0 {
		t.Errorf("changes left: %d", len(r.cs.Changes))
	}
}

func TestToggleRedeploySaves(t *testing.T) {
	r := newRig(t)
	r.drain(r.update(tui.ToggleRedeployMsg{PlaceKey: "e1/s1"}))
	saved, err := changeset.Load(r.deps.ChangeSetPath)
	if err != nil || len(saved.RedeployOff) != 1 {
		t.Errorf("saved = %+v, %v", saved, err)
	}
}

func TestApplyRunsAndReconciles(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	rev := r.cs.Rev
	msgs := r.drain(r.update(tui.ApplyMsg{}))
	var events, done int
	for _, m := range msgs {
		switch m.(type) {
		case tui.RunEventMsg:
			events++
		case tui.RunDoneMsg:
			done++
		}
	}
	if events == 0 || done != 1 {
		t.Fatalf("events %d, done %d", events, done)
	}
	if v, _ := r.fake.Var("e1", "s1", "NEW_VAR"); v != "fresh" {
		t.Errorf("variable not written: %q", v)
	}
	if len(r.cs.Changes) != 0 || r.cs.Rev <= rev {
		t.Errorf("change set after run: %+v rev %d", r.cs.Changes, r.cs.Rev)
	}
	saved, _ := changeset.Load(r.deps.ChangeSetPath)
	if len(saved.Changes) != 0 {
		t.Errorf("saved set not reconciled: %+v", saved.Changes)
	}
	if v := r.view(); !strings.Contains(v, "✓ applied 1 changes") {
		t.Errorf("flash missing:\n%s", v)
	}
	if r.st.Variable(domain.LocationID("e1", "s1", "NEW_VAR")) == nil {
		t.Errorf("inventory not re-read after the run")
	}
	var prog tui.RunProgress
	for _, p := range r.m.Ctx().Running {
		prog = p
	}
	if prog.State != apply.RunFinished || prog.Total == 0 || prog.Done != prog.Total {
		t.Errorf("progress = %+v", prog)
	}
}

func TestApplyWriteNotAllowed(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.set.WriteEnvs = nil
	r.drain(r.update(tui.SaveSettingsMsg{}))
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	r.drain(r.update(tui.ApplyMsg{}))
	if v := r.view(); !strings.Contains(v, "✗ shop/production is not in write environments · Settings w") {
		t.Errorf("flash missing:\n%s", v)
	}
	// Refused before a run exists.
	if len(r.cs.Changes) != 1 || r.cs.Changes[0].RunID != "" {
		t.Errorf("refused change should stay unlocked: %+v", r.cs.Changes)
	}
	if runs, _ := r.runs.List(); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
	if n := len(r.fake.Mutations()); n != 0 {
		t.Errorf("mutations = %d, want none", n)
	}

	// An immediate action is refused the same way.
	r.drain(r.update(tui.ImmediateMsg{Change: kinds.Restart(domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, time.Now())}))
	if v := r.view(); !strings.Contains(v, "✗ shop/production is not in write environments · Settings w") {
		t.Errorf("immediate flash missing:\n%s", v)
	}
	if runs, _ := r.runs.List(); len(runs) != 0 {
		t.Errorf("runs after immediate = %+v, want none", runs)
	}
}

func TestApplyConflictWithUnselected(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	ref := setChange("B_NEW", "${{A_NEW}}")
	nv, _ := json.Marshal(map[string]any{"value": "${{A_NEW}}", "refs": []string{domain.LocationID("e1", "s1", "A_NEW")}})
	ref.New = nv
	r.drain(r.update(tui.PutChangeMsg{Change: ref}))
	r.drain(r.update(tui.ApplyMsg{IDs: []string{r.cs.Changes[1].ID}}))
	if v := r.view(); !strings.Contains(v, "✗ needs an unselected change: shop/production api.A_NEW") {
		t.Errorf("conflict flash missing:\n%s", v)
	}

	// The other direction: the selected change writes what an unselected
	// one reads.
	r.drain(r.update(tui.ApplyMsg{IDs: []string{r.cs.Changes[0].ID}}))
	if v := r.view(); !strings.Contains(v, "✗ an unselected change needs shop/production api.A_NEW: include it or remove it") {
		t.Errorf("reverse conflict flash missing:\n%s", v)
	}
	if r.cs.Changes[0].RunID != "" || r.cs.Changes[1].RunID != "" {
		t.Errorf("a refused selection must not lock: %+v", r.cs.Changes)
	}
}

func TestImmediateLeavesLiveSet(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("KEEP", "k")}))
	rev := r.cs.Rev
	r.drain(r.update(tui.ImmediateMsg{Change: setChange("NOW", "n")}))
	if v, _ := r.fake.Var("e1", "s1", "NOW"); v != "n" {
		t.Errorf("immediate not applied")
	}
	if len(r.cs.Changes) != 1 || r.cs.Rev != rev {
		t.Errorf("live set touched: %+v", r.cs.Changes)
	}
}

func TestStartupUnlocksStaleRuns(t *testing.T) {
	cs := &changeset.ChangeSet{Changes: []changeset.Change{setChange("A", "a"), setChange("B", "b")}}
	cs.Changes[0].ID, cs.Changes[0].RunID = "c1", "gone-run"
	cs.Changes[1].ID = "c2"
	r := newRig(t, func(d *root.Deps) { d.CS = cs })
	r.drain(r.m.Init())
	if cs.Changes[0].RunID != "" {
		t.Errorf("stale lock kept")
	}
	saved, err := changeset.Load(r.deps.ChangeSetPath)
	if err != nil || len(saved.Changes) != 2 || saved.Changes[0].RunID != "" {
		t.Errorf("saved = %+v, %v", saved, err)
	}
}

func TestQuitDuringRunAsks(t *testing.T) {
	r := newRig(t)
	r.m.Ctx().Running["r1"] = tui.RunProgress{State: apply.RunRunning}
	if cmd := r.update(key("q")); cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatalf("quit without asking")
		}
	}
	if v := r.view(); !strings.Contains(v, "a run is applying: quit anyway? it can be resumed (y/n)") {
		t.Fatalf("confirm missing:\n%s", v)
	}
	r.update(key("n"))
	if v := r.view(); strings.Contains(v, "quit anyway") {
		t.Errorf("confirm still shown after n")
	}
	r.update(key("q"))
	cmd := r.update(key("y"))
	if cmd == nil {
		t.Fatalf("y returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("y did not quit")
	}
}

func TestQuitAtHome(t *testing.T) {
	r := newRig(t)
	cmd := r.update(key("q"))
	if cmd == nil {
		t.Fatalf("q at home returned nothing")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("q at home did not quit")
	}
}

func TestStartupCorruptChangeSetNote(t *testing.T) {
	r := newRig(t, func(d *root.Deps) { d.LoadNote = "change set file is corrupt, moved to changeset.json.corrupt-1" })
	if v := r.view(); !strings.Contains(v, "! change set file is corrupt, moved to changeset.json.corrupt-1") {
		t.Errorf("note missing:\n%s", v)
	}
}

func TestOpenByName(t *testing.T) {
	opened := &fakeScreen{title: "Changes"}
	r := newRig(t, func(d *root.Deps) {
		d.Screens["changeset"] = func(args ...any) tui.Screen { return opened }
	})
	r.update(key("C"))
	if !strings.Contains(r.view(), "Changes body") {
		t.Fatalf("C did not open the change set:\n%s", r.view())
	}
	r.update(key("esc"))
	r.drain(r.update(tui.OpenMsg{Name: "services"}))
	if !strings.Contains(r.view(), "! not available yet") {
		t.Errorf("unknown screen not flashed:\n%s", r.view())
	}
	r.update(key("?"))
	v := r.view()
	if !strings.Contains(v, "railway-manager Keys") || !strings.Contains(v, "services") {
		t.Errorf("key list missing:\n%s", v)
	}
}

func TestCapturingScreenGetsGlobalKeys(t *testing.T) {
	r := newRig(t)
	s := &fakeScreen{title: "Input", capture: true}
	r.update(tui.PushMsg{Screen: s})
	r.update(key("q"))
	r.update(key("C"))
	if len(s.msgs) != 2 || !strings.Contains(r.view(), "Input body") {
		t.Errorf("capturing screen lost keys: %v", s.msgs)
	}
}

func TestAuthRemovedWhenNotOnTop(t *testing.T) {
	r := newRig(t, func(d *root.Deps) {
		d.Screens["keys"] = func(args ...any) tui.Screen { return &fakeScreen{title: "Keys"} }
	})
	r.update(loaders.AccountMsg{Err: &railway.AuthError{Msg: "token rejected"}})
	cmd := r.update(key("enter")) // Auth: checking…
	r.update(key("?"))            // a screen over Auth
	if v := r.view(); !strings.Contains(v, "Keys body") {
		t.Fatalf("keys not on top:\n%s", v)
	}
	r.drain(cmd) // AccountMsg success arrives while Keys is on top
	if v := r.view(); !strings.Contains(v, "Keys body") {
		t.Fatalf("top changed:\n%s", v)
	}
	r.update(key("esc"))
	if v := r.view(); strings.Contains(v, "Sign in") || !strings.Contains(v, "railway-manager Home") {
		t.Errorf("auth left on the stack:\n%s", v)
	}
	// a later refusal pushes exactly one Auth; a failure reaches it anywhere
	r.update(loaders.CatalogMsg{Err: &railway.AuthError{Msg: "first"}})
	r.update(key("?"))
	r.update(loaders.AccountMsg{Err: &railway.AuthError{Msg: "second reason"}})
	r.update(key("esc"))
	if v := r.view(); !strings.Contains(v, "second reason") {
		t.Errorf("reason not delivered to auth below the top:\n%s", v)
	}
	r.update(key("esc"))
	if v := r.view(); !strings.Contains(v, "railway-manager Home") {
		t.Errorf("auth stacked twice:\n%s", v)
	}
}

func TestSnapshotPublished(t *testing.T) {
	r := newRig(t)
	if r.snap.Load() == nil || r.snap.Load() == r.st {
		t.Fatalf("no snapshot at startup, or it is the live store")
	}
	r.update(loaders.CatalogMsg{Catalog: catalog})
	r.update(loaders.InventoryMsg{ProjectID: "p1", Seq: 1, Result: railway.DiscoveryResult{Vars: []railway.VarRecord{{EnvID: "e1", ServiceID: "s1", Name: "A"}}}})
	snap := r.snap.Load()
	if snap == r.st || snap.Variable("e1/s1/A") == nil || snap.EnvLabel("e1") != "shop/production" {
		t.Errorf("snapshot not refreshed after store updates")
	}
}

func TestStaleInventoryDropped(t *testing.T) {
	r := newRig(t)
	r.update(loaders.CatalogMsg{Catalog: catalog})
	r.update(loaders.InventoryMsg{ProjectID: "p1", Seq: 2, Result: railway.DiscoveryResult{Vars: []railway.VarRecord{{EnvID: "e1", ServiceID: "s1", Name: "NEW"}}}})
	r.update(loaders.InventoryMsg{ProjectID: "p1", Seq: 1, Result: railway.DiscoveryResult{Vars: []railway.VarRecord{{EnvID: "e1", ServiceID: "s1", Name: "OLD"}}}})
	if r.st.Variable("e1/s1/NEW") == nil || r.st.Variable("e1/s1/OLD") != nil {
		t.Errorf("older inventory applied over a newer one")
	}
}

// sequenceOf returns the commands of a tea.Sequence message (an unexported
// []tea.Cmd type), nil for any other message.
func sequenceOf(msg tea.Msg) []tea.Cmd {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Slice || v.Type().Elem() != reflect.TypeOf(tea.Cmd(nil)) {
		return nil
	}
	out := make([]tea.Cmd, v.Len())
	for i := range out {
		out[i] = v.Index(i).Interface().(tea.Cmd)
	}
	return out
}

// split runs cmd and returns the commands of its batch or sequence (or cmd
// itself).
func split(cmd tea.Cmd) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		return b
	}
	if seq := sequenceOf(msg); seq != nil {
		return seq
	}
	return []tea.Cmd{func() tea.Msg { return msg }}
}

func TestQuitWaitsForCancelledRuns(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "x")}))
	cmds := split(r.prepare(tui.ApplyMsg{}))
	if len(cmds) != 2 {
		t.Fatalf("apply commands = %d", len(cmds))
	}
	r.update(cmds[0]()) // save
	exec := cmds[1]
	r.update(key("q"))
	tick := r.update(key("y"))
	if tick == nil {
		t.Fatalf("y returned nothing")
	}
	if !strings.Contains(r.view(), "stopping runs") {
		t.Errorf("no stopping note:\n%s", r.view())
	}
	done := exec() // cancelled: Execute returns at once
	for len(r.sent) > 0 {
		r.update(<-r.sent)
	}
	cmd := r.update(done)
	if cmd == nil {
		t.Fatalf("run end while quitting returned nothing")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("did not quit after the run ended")
	}
	saved, _ := changeset.Load(r.deps.ChangeSetPath)
	if len(saved.Changes) != 1 || saved.Changes[0].RunID == "" {
		t.Errorf("cancelled run's change should stay held: %+v", saved.Changes)
	}
}

func TestQuitTimeout(t *testing.T) {
	defer root.SetQuitWait(time.Millisecond)()
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "x")}))
	_ = split(r.prepare(tui.ApplyMsg{})) // the run command is never run
	r.update(key("q"))
	tick := r.update(key("y"))
	cmd := r.update(tick())
	if cmd == nil {
		t.Fatalf("timeout returned nothing")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("timeout did not quit")
	}
}

func TestRunEventsDoNotBlockExecutor(t *testing.T) {
	release := make(chan struct{})
	sent := make(chan tea.Msg, 1000)
	r2 := newRig(t, func(d *root.Deps) {
		d.Send = func(m tea.Msg) { <-release; sent <- m }
	})
	r2.drain(r2.m.Init())
	r2.drain(r2.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "x")}))
	cmds := split(r2.prepare(tui.ApplyMsg{}))
	r2.update(cmds[0]())
	result := make(chan tea.Msg, 1)
	go func() { result <- cmds[1]() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		infos, err := r2.runs.List()
		if err == nil && len(infos) == 1 && infos[0].State == apply.RunFinished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("executor did not finish while Send was blocked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-result:
		t.Fatalf("RunDone returned before the events were forwarded")
	default:
	}
	close(release)
	done := <-result
	if _, ok := done.(tui.RunDoneMsg); !ok {
		t.Fatalf("result = %#v", done)
	}
	n := len(sent)
	if n == 0 {
		t.Fatalf("no events forwarded")
	}
	// every step reports started before done
	seen := map[string]string{}
	for i := 0; i < n; i++ {
		ev := (<-sent).(tui.RunEventMsg).Event
		if ev.Status == apply.StatusDone && seen[ev.StepKey] != apply.StatusStarted {
			t.Errorf("%s: done before started", ev.StepKey)
		}
		seen[ev.StepKey] = ev.Status
	}
}

func TestResumeSeedsProgressFromJournal(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	r.fake.FailNext("CommitPatch", "boom")
	r.drain(r.update(tui.ApplyMsg{}))
	runID := r.cs.Changes[0].RunID
	if p := r.m.Ctx().Running[runID]; p.State != apply.RunFailed {
		t.Fatalf("first run = %+v", p)
	}
	// journal the patch step done, as if an earlier attempt had written it
	j, err := apply.OpenJournal(filepath.Join(r.dir, "runs", runID, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Set("patch:e1", apply.StepRecord{Status: apply.StatusDone}); err != nil {
		t.Fatal(err)
	}

	cmd := r.prepare(tui.ResumeMsg{RunID: runID}) // the run is read off Update
	p := r.m.Ctx().Running[runID]
	if p.State != apply.RunRunning || p.Done != 1 || p.Total != 2 {
		t.Errorf("seeded progress = %+v", p)
	}
	r.drain(cmd)
	if p := r.m.Ctx().Running[runID]; p.State != apply.RunFinished || p.Done != p.Total {
		t.Errorf("after resume = %+v", p)
	}
	if len(r.cs.Changes) != 0 {
		t.Errorf("resumed run not reconciled: %+v", r.cs.Changes)
	}
}

func TestApplyConflictWithRun(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	_ = split(r.prepare(tui.ApplyMsg{})) // run prepared, not executed: still unfinished
	runID := r.cs.Changes[0].RunID
	ref := setChange("B_NEW", "${{A_NEW}}")
	nv, _ := json.Marshal(map[string]any{"value": "${{A_NEW}}", "refs": []string{domain.LocationID("e1", "s1", "A_NEW")}})
	ref.New = nv
	r.drain(r.update(tui.PutChangeMsg{Change: ref}))
	r.drain(r.update(tui.ApplyMsg{IDs: []string{r.cs.Changes[1].ID}}))
	if v := r.view(); !strings.Contains(v, "✗ conflicts with run "+runID+": shop/production api.A_NEW") {
		t.Errorf("conflict flash missing:\n%s", v)
	}
}

func TestRepeatedApplyStartsNoSecondRun(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	_ = split(r.prepare(tui.ApplyMsg{}))
	id := r.cs.Changes[0].ID
	if cmd := r.update(tui.ApplyMsg{}); cmd != nil {
		t.Errorf("second apply returned a command")
	}
	if cmd := r.update(tui.ApplyMsg{IDs: []string{id}}); cmd != nil {
		t.Errorf("apply of a held change returned a command")
	}
	if !strings.Contains(r.view(), "✗") {
		t.Errorf("no refusal flash:\n%s", r.view())
	}
	if infos, err := r.runs.List(); err != nil || len(infos) != 1 {
		t.Errorf("runs = %d, %v", len(infos), err)
	}
}

func TestSaveFailureFlash(t *testing.T) {
	r := newRig(t, func(d *root.Deps) { d.ChangeSetPath = filepath.Join(t.TempDir(), "missing", "changeset.json") })
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	if v := r.view(); !strings.Contains(v, "✗ change set not saved: ") {
		t.Errorf("save failure not flashed:\n%s", v)
	}
}

func TestRunDoneWithoutStateDoesNotReconcile(t *testing.T) {
	r := newRig(t)
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	r.cs.Changes[0].RunID = "r9"
	r.m.Ctx().Running["r9"] = tui.RunProgress{State: apply.RunRunning}
	rev := r.cs.Rev
	r.drain(r.update(tui.RunDoneMsg{RunID: "r9", Err: errors.New("run lock held")}))
	if r.cs.Rev != rev || len(r.cs.Changes) != 1 || r.cs.Changes[0].RunID != "r9" {
		t.Errorf("change set touched: %+v", r.cs.Changes)
	}
	if _, ok := r.m.Ctx().Running["r9"]; ok {
		t.Errorf("progress kept")
	}
	if v := r.view(); !strings.Contains(v, "✗ run r9 failed: run lock held") {
		t.Errorf("flash missing:\n%s", v)
	}
}

func TestFailedRunFlashesFirstStepError(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.fake.FailPatches("patch rejected by fixture")
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "x")}))
	r.drain(r.update(tui.ApplyMsg{}))
	var p tui.RunProgress
	for _, x := range r.m.Ctx().Running {
		p = x
	}
	if p.State != apply.RunFailed || len(p.Errors) == 0 {
		t.Fatalf("progress = %+v", p)
	}
	if v := r.view(); !strings.Contains(v, "failed: ") || !strings.Contains(v, "patch rejected by fixture") {
		t.Errorf("flash missing the step error:\n%s", v)
	}
}

// runAll runs cmd and every command of the batches and sequences it yields,
// in order, and returns the messages that are not batches or sequences.
func runAll(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	// tea.Sequence yields an unexported []tea.Cmd type; reach it by reflection.
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeOf(tea.Cmd(nil)) {
		var out []tea.Msg
		for i := 0; i < v.Len(); i++ {
			out = append(out, runAll(v.Index(i).Interface().(tea.Cmd))...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestRunFinishingWhileQuittingSavesBeforeQuit(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	cmds := split(r.prepare(tui.ApplyMsg{}))
	r.update(cmds[0]()) // save: the change is held by the run on disk
	done := cmds[1]()   // the run finishes before the quit is confirmed
	for len(r.sent) > 0 {
		r.update(<-r.sent)
	}
	r.update(key("q"))
	if tick := r.update(key("y")); tick == nil {
		t.Fatalf("y returned nothing")
	}
	if saved, _ := changeset.Load(r.deps.ChangeSetPath); len(saved.Changes) != 1 {
		t.Fatalf("precondition: saved = %+v", saved.Changes)
	}
	msgs := runAll(r.update(done))
	if len(msgs) == 0 {
		t.Fatalf("no commands after the run ended")
	}
	if _, ok := msgs[len(msgs)-1].(tea.QuitMsg); !ok {
		t.Fatalf("last message = %#v, want quit", msgs[len(msgs)-1])
	}
	for _, m := range msgs[:len(msgs)-1] {
		if _, ok := m.(tea.QuitMsg); ok {
			t.Errorf("quit before the save")
		}
		if _, ok := m.(loaders.InventoryMsg); ok {
			t.Errorf("inventory read while quitting")
		}
	}
	saved, err := changeset.Load(r.deps.ChangeSetPath)
	if err != nil || len(saved.Changes) != 0 {
		t.Errorf("reconciled set not saved before quit: %+v, %v", saved.Changes, err)
	}
}

// flatten runs cmd; a tea.Sequence is run in order.
func flatten(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	v := reflect.ValueOf(msg)
	if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Func {
		var out []tea.Msg
		for i := range v.Len() {
			out = append(out, flatten(v.Index(i).Interface().(tea.Cmd))...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

func pickerRig(t *testing.T) *rig {
	return newRig(t, func(d *root.Deps) {
		d.Screens["picker"] = picker.Build
		d.Settings.Projects = nil
	})
}

func TestStartupPickerSelectsProjects(t *testing.T) {
	r := pickerRig(t)
	r.drain(r.m.Init())
	if v := r.view(); !strings.Contains(v, "Select projects") || !strings.Contains(v, "[ ] shop") {
		t.Fatalf("picker not opened:\n%s", v)
	}
	r.update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	cmd := r.update(key("enter"))
	var queue []tea.Msg
	queue = append(queue, flatten(cmd)...)
	for len(queue) > 0 {
		msg := queue[0]
		queue = queue[1:]
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				queue = append(queue, flatten(c)...)
			}
			continue
		}
		queue = append(queue, flatten(r.update(msg))...)
	}
	r.drain(nil)
	if !reflect.DeepEqual(r.set.Projects, []string{"p1"}) {
		t.Fatalf("projects = %v", r.set.Projects)
	}
	v := r.view()
	if !strings.Contains(v, "railway-manager Home") || !strings.Contains(v, "1 project · ") {
		t.Errorf("home not showing the project:\n%s", v)
	}
	if st, err := settings.Load(r.deps.SettingsPath); err != nil || !reflect.DeepEqual(st.Projects, []string{"p1"}) {
		t.Errorf("saved = %+v, %v", st, err)
	}
}

func TestStartupPickerCancelAndOnce(t *testing.T) {
	r := pickerRig(t)
	r.drain(r.m.Init())
	r.update(key("esc"))
	v := r.view()
	if !strings.Contains(v, "no projects selected · o settings") {
		t.Fatalf("home line missing:\n%s", v)
	}
	r.drain(r.update(tui.RefreshMsg{}))
	if v := r.view(); strings.Contains(v, "Select projects") {
		t.Errorf("picker offered twice:\n%s", v)
	}
}

func TestNoStartupPickerWithProjects(t *testing.T) {
	r := newRig(t, func(d *root.Deps) { d.Screens["picker"] = picker.Build })
	r.drain(r.m.Init())
	if v := r.view(); strings.Contains(v, "Select projects") {
		t.Errorf("picker opened:\n%s", v)
	}
}

func TestPutBackToBaseOfReferencedChangeRefused(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	ref := setChange("B_NEW", "${{A_NEW}}")
	nv, _ := json.Marshal(map[string]any{"value": "${{A_NEW}}", "refs": []string{domain.LocationID("e1", "s1", "A_NEW")}})
	ref.New = nv
	r.drain(r.update(tui.PutChangeMsg{Change: ref}))
	del := changeset.Change{
		Kind: "variable.delete", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"},
		Target: "A_NEW", Base: changeset.Base{Absent: true},
	}
	r.drain(r.update(tui.PutChangeMsg{Change: del}))
	if len(r.cs.Changes) != 2 || !strings.Contains(r.view(), "✗ A_NEW is referenced by a pending change: remove that first") {
		t.Errorf("referenced add removed through a put:\n%s", r.view())
	}
	// once the referrer is gone the same put removes the add
	r.drain(r.update(tui.RemoveChangeMsg{ID: r.cs.Changes[1].ID}))
	r.drain(r.update(tui.PutChangeMsg{Change: del}))
	if len(r.cs.Changes) != 0 {
		t.Errorf("changes left: %d", len(r.cs.Changes))
	}
}

func TestTokenInfoRefreshedOnSignIn(t *testing.T) {
	n := 0
	r := newRig(t, func(d *root.Deps) {
		d.TokenInfo = func() string { n++; return fmt.Sprintf("config · call %d", n) }
	})
	if got := r.m.Ctx().Token; got != "config · call 1" {
		t.Fatalf("startup Token = %q", got)
	}
	r.update(loaders.AccountMsg{Account: railway.Account{Email: "a@example.test"}})
	if got := r.m.Ctx().Token; got != "config · call 2" {
		t.Errorf("Token after sign-in = %q", got)
	}
	r.update(loaders.AccountMsg{Err: errors.New("offline")})
	if n != 2 {
		t.Errorf("TokenInfo called %d times, want 2 (not on a failed WhoAmI)", n)
	}
}

func TestNerdDetectedReachesCtx(t *testing.T) {
	r := newRig(t, func(d *root.Deps) { d.NerdDetected = true })
	if !r.m.Ctx().NerdDetected {
		t.Errorf("Ctx.NerdDetected = false")
	}
}

var twoProjects = railway.Catalog{Projects: []railway.Project{
	catalog.Projects[0],
	{
		ID: "p2", Name: "acme", Workspace: "ws",
		Environments: []railway.Environment{
			{ID: "e2", Name: "dev", CanAccess: true},
			{ID: "e3", Name: "production", CanAccess: true},
		},
	},
}}

func TestSelectionOverrideAfterCatalog(t *testing.T) {
	r := newRig(t, func(d *root.Deps) {
		d.Settings.Projects = []string{"p1"}
		d.Settings.Envs = []string{"e1"}
		d.SelectProjects = []string{"acme", "nope"}
		d.SelectEnvs = []string{"dev"}
	})
	r.update(loaders.CatalogMsg{Catalog: twoProjects})
	if !reflect.DeepEqual(r.set.Projects, []string{"p2"}) || !reflect.DeepEqual(r.set.Envs, []string{"e2"}) {
		t.Fatalf("selection = %v / %v, want [p2] / [e2]", r.set.Projects, r.set.Envs)
	}
	if v := r.view(); !strings.Contains(v, "! no project nope") {
		t.Errorf("unknown project not flashed:\n%s", v)
	}
	// A second catalog read keeps the override and does not flash again.
	r.update(key("x"))
	r.update(loaders.CatalogMsg{Catalog: twoProjects})
	if strings.Contains(r.view(), "no project nope") || !reflect.DeepEqual(r.set.Projects, []string{"p2"}) {
		t.Errorf("override re-applied or lost: %v\n%s", r.set.Projects, r.view())
	}

	// Saving settings keeps the saved selection, not the override.
	r.set.Icons = "nerd"
	r.drain(r.update(tui.SaveSettingsMsg{}))
	saved, err := settings.Load(filepath.Join(r.dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Projects, []string{"p1"}) || !reflect.DeepEqual(saved.Envs, []string{"e1"}) || saved.Icons != "nerd" {
		t.Errorf("saved = %+v, want projects [p1] envs [e1] icons nerd", saved)
	}
}

// The picker returns ids in catalog order: an override given in another
// order, confirmed unchanged, must still not be saved.
func TestSelectionOverrideComparedAsSet(t *testing.T) {
	r := newRig(t, func(d *root.Deps) {
		d.Settings.Projects = []string{"p1"}
		d.Settings.Envs = []string{"e1"}
		d.SelectProjects = []string{"acme", "shop"}
		d.SelectEnvs = []string{"production", "dev"}
	})
	r.update(loaders.CatalogMsg{Catalog: twoProjects})
	if !reflect.DeepEqual(r.set.Projects, []string{"p2", "p1"}) || !reflect.DeepEqual(r.set.Envs, []string{"e1", "e3", "e2"}) {
		t.Fatalf("selection = %v / %v", r.set.Projects, r.set.Envs)
	}
	// What the pickers hand back when confirmed unchanged: catalog order.
	r.set.Projects = []string{"p1", "p2"}
	r.set.Envs = []string{"e1", "e2", "e3"}
	r.drain(r.update(tui.SaveSettingsMsg{}))
	saved, err := settings.Load(filepath.Join(r.dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Projects, []string{"p1"}) || !reflect.DeepEqual(saved.Envs, []string{"e1"}) {
		t.Errorf("saved = %v / %v, want the original [p1] / [e1]", saved.Projects, saved.Envs)
	}
}

func TestSelectionOverrideByIDAndUnknownEnv(t *testing.T) {
	r := newRig(t, func(d *root.Deps) {
		d.Settings.Projects = nil
		d.SelectProjects = []string{"p1", "p2"}
		d.SelectEnvs = []string{"production", "staging"}
	})
	r.update(loaders.CatalogMsg{Catalog: twoProjects})
	if !reflect.DeepEqual(r.set.Projects, []string{"p1", "p2"}) || !reflect.DeepEqual(r.set.Envs, []string{"e1", "e3"}) {
		t.Fatalf("selection = %v / %v", r.set.Projects, r.set.Envs)
	}
	if v := r.view(); !strings.Contains(v, "! no env staging") {
		t.Errorf("unknown env not flashed:\n%s", v)
	}
}

func TestSelectionOverrideAllUnknownKeepsSaved(t *testing.T) {
	r := newRig(t, func(d *root.Deps) { d.SelectProjects = []string{"nope"} })
	r.update(loaders.CatalogMsg{Catalog: twoProjects})
	if !reflect.DeepEqual(r.set.Projects, []string{"p1"}) {
		t.Errorf("Projects = %v, want the saved [p1]", r.set.Projects)
	}
}

func TestSelectionEditedDuringOverrideIsSaved(t *testing.T) {
	r := newRig(t, func(d *root.Deps) { d.SelectProjects = []string{"acme"} })
	r.update(loaders.CatalogMsg{Catalog: twoProjects})
	r.set.Projects = []string{"p1", "p2"} // the user picked projects in Settings
	r.drain(r.update(tui.SaveSettingsMsg{}))
	saved, err := settings.Load(filepath.Join(r.dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Projects, []string{"p1", "p2"}) {
		t.Errorf("saved Projects = %v, want the edited [p1 p2]", saved.Projects)
	}
}

// interruptedRun applies one change and never executes the run, as when the
// app quit or crashed during the run's first attempt: the run stays running
// on disk. It returns the run id and the work dir.
func interruptedRun(t *testing.T) (runID, dir string) {
	t.Helper()
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	cmds := split(r.prepare(tui.ApplyMsg{}))
	if len(cmds) != 2 {
		t.Fatalf("apply commands = %d", len(cmds))
	}
	r.update(cmds[0]()) // save; the run command never runs
	runID = r.cs.Changes[0].RunID
	if s := journalState(t, r.dir, runID); s != apply.RunRunning {
		t.Fatalf("state = %q, want running", s)
	}
	return runID, r.dir
}

func journalState(t *testing.T, dir, runID string) string {
	t.Helper()
	j, err := apply.OpenJournal(filepath.Join(dir, "runs", runID, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	return j.State
}

// restarted builds a new model over the work dir and opens the run view.
func restarted(t *testing.T, dir, runID string) *rig {
	t.Helper()
	cs, err := changeset.Load(filepath.Join(dir, "changeset.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(d *root.Deps) {
		d.Runs.Dir = dir
		d.CS = &cs
		d.ChangeSetPath = filepath.Join(dir, "changeset.json")
		d.Screens["run"] = runscreen.Build
	})
	r.dir = dir
	r.drain(r.m.Init())
	r.drain(r.update(tui.OpenMsg{Name: "run", Args: []any{runID}}))
	return r
}

// A run left running by a quit or crash is resumed from the run view of a
// new app instance.
func TestInterruptedRunResumes(t *testing.T) {
	runID, dir := interruptedRun(t)
	r := restarted(t, dir, runID)
	r.drain(r.update(key("r")))
	if s := journalState(t, dir, runID); s != apply.RunFinished {
		t.Fatalf("state = %q, want finished; view:\n%s", s, r.view())
	}
	if v, _ := r.fake.Var("e1", "s1", "NEW_VAR"); v != "fresh" {
		t.Errorf("variable not written: %q", v)
	}
	if len(r.m.Ctx().CS.Changes) != 0 {
		t.Errorf("change set after the resumed run = %+v", r.m.Ctx().CS.Changes)
	}
}

// ... or abandoned there, which unlocks its changes.
func TestInterruptedRunAbandons(t *testing.T) {
	runID, dir := interruptedRun(t)
	r := restarted(t, dir, runID)
	r.drain(r.update(key("X")))
	r.drain(r.update(key("y")))
	if s := journalState(t, dir, runID); s != apply.RunAbandoned {
		t.Fatalf("state = %q, want abandoned; view:\n%s", s, r.view())
	}
	cs := r.m.Ctx().CS
	if len(cs.Changes) != 1 || cs.Changes[0].RunID != "" {
		t.Errorf("changes after abandon = %+v, want one unlocked", cs.Changes)
	}
}

// A run executing in another live instance is refused: the run view says
// where it runs.
func TestInterruptedRunHeldElsewhere(t *testing.T) {
	runID, dir := interruptedRun(t)
	lock := filepath.Join(dir, "runs", runID, "lock")
	if err := os.WriteFile(lock, []byte(fmt.Sprint(os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	r := restarted(t, dir, runID)
	r.drain(r.update(key("r")))
	if v := r.view(); !strings.Contains(v, "running in another window") {
		t.Errorf("no lock flash:\n%s", v)
	}
	if s := journalState(t, dir, runID); s != apply.RunRunning {
		t.Errorf("state = %q, want running", s)
	}
}

// Conflict flashes name limits, volume and domain targets by service, not
// by raw ids, and never carry a control sequence from Railway data.
func TestTargetLabelReadable(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.st.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{{EnvID: "e1", ServiceID: "s1", VolumeID: "v1", MountPath: "/data"}})
	p := r.st.PlaceOf("e1", "s1")
	r.st.SetDomains(p, railway.Domains{Service: []railway.ServiceDomain{{ID: "sd-1", Domain: "api-production.up.railway.app\x1b[31m"}}})
	for _, tc := range []struct{ target, want string }{
		{"var:e1/s1/A_NEW", "shop/production api.A_NEW"},
		{"limits:e1/s1", "shop/production api limits"},
		{"volume:e1/v1", "shop/production api volume /data"},
		{"volume:e1/v9", "shop/production volume v9"},
		{"domain:e1/s1/custom-domain:shop.example.com", "shop/production api domain shop.example.com"},
		{"domain:e1/s1/service-domain:new", "shop/production api new railway domain"},
		{"domain:e1/s1/service-domain:sd-1", "shop/production api domain api-production.up.railway.app"},
		{"domain:e1/s1/service-domain:sd-9", "shop/production api railway domain sd-9"},
		{"limits:e1/s1\x1b[2J", "shop/production s1 limits"},
		{"other:thing", "other:thing"},
	} {
		got := r.m.TargetLabel(tc.target)
		if strings.ContainsRune(got, '\x1b') {
			t.Errorf("%q: control sequence in %q", tc.target, got)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.target, got, tc.want)
		}
	}
}

// A resize reaches the top screen after the context has the new size, so a
// screen can act on its new width (the Services panel re-reads its charts).
func TestResizeForwardedToTop(t *testing.T) {
	r := newRig(t)
	a := &fakeScreen{title: "A"}
	r.update(tui.PushMsg{Screen: a})
	r.update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if len(a.msgs) == 0 {
		t.Fatal("the top screen got no resize")
	}
	if m, ok := a.msgs[len(a.msgs)-1].(tea.WindowSizeMsg); !ok || m.Width != 120 {
		t.Errorf("last message = %#v", a.msgs[len(a.msgs)-1])
	}
}

// Changes loaded invalid from disk stay in the set: they are never applied,
// naming one is refused with its reason, and they can be removed.
func TestApplyLeavesInvalidChanges(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	r.cs.Changes = append(r.cs.Changes,
		changeset.Change{ID: "bad-limits", Kind: "limits.set", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s2"}, Target: "limits", New: json.RawMessage(`{"cpu":2}`)},
		changeset.Change{ID: "bad-kind", Kind: "service.teleport", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s2"}, Target: "teleport"},
	)
	r.cs.Rev++

	r.drain(r.update(tui.ApplyMsg{IDs: []string{"bad-limits"}}))
	if v := r.view(); !strings.Contains(v, "✗ limits is invalid and cannot be applied: limits.set: bad change data") {
		t.Errorf("refusal flash missing:\n%s", v)
	}
	if runs, _ := r.runs.List(); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none", runs)
	}

	r.drain(r.update(tui.ApplyMsg{}))
	if v, _ := r.fake.Var("e1", "s1", "NEW_VAR"); v != "fresh" {
		t.Errorf("the valid change was not applied: %q", v)
	}
	var left []string
	for _, c := range r.cs.Changes {
		if c.RunID != "" {
			t.Errorf("%s locked by %s", c.ID, c.RunID)
		}
		left = append(left, c.ID)
	}
	if !reflect.DeepEqual(left, []string{"bad-limits", "bad-kind"}) {
		t.Fatalf("left = %v, want the invalid changes kept", left)
	}
	saved, _ := changeset.Load(r.deps.ChangeSetPath)
	if len(saved.Changes) != 2 {
		t.Errorf("saved set = %+v, want the invalid changes kept", saved.Changes)
	}

	r.drain(r.update(tui.RemoveChangeMsg{ID: "bad-kind"}))
	if _, ok := r.cs.Get("bad-kind"); ok {
		t.Error("an invalid change could not be removed")
	}
}

// Run events refresh the runs index off Update, coalesced: while a read is
// in flight, any number of events queue one more read.
func TestRunEventsRefreshIndexCoalesced(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	if !r.m.Ctx().RunIndex.Loaded {
		t.Fatalf("index not read at start")
	}
	var reads []loaders.RunsMsg
	collect := func(cmd tea.Cmd) {
		for _, m := range runAll(cmd) {
			if rm, ok := m.(loaders.RunsMsg); ok {
				reads = append(reads, rm)
			}
		}
	}
	ev := tui.RunEventMsg{Event: apply.Event{RunID: "r-x", StepKey: "patch:e1", Status: apply.StatusStarted}}
	for range 10 {
		collect(r.update(ev))
	}
	if len(reads) != 1 {
		t.Fatalf("reads after 10 events = %d, want 1", len(reads))
	}
	collect(r.update(reads[0])) // lands: the queued read starts
	if len(reads) != 2 {
		t.Fatalf("reads after the first landed = %d, want 2", len(reads))
	}
	collect(r.update(reads[1])) // lands: nothing is queued
	if len(reads) != 2 {
		t.Errorf("reads = %d, want 2", len(reads))
	}
}

// The index follows a run to its end.
func TestRunsIndexFollowsRun(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	r.drain(r.update(tui.ApplyMsg{}))
	idx := r.m.Ctx().RunIndex
	if len(idx.Runs) != 1 || idx.Runs[0].State != apply.RunFinished {
		t.Errorf("index = %+v", idx)
	}
}

// A corrupt run is skipped by the index and flashed once, with nothing of
// its files; the other runs stay listed.
func TestRunsIndexSkipsCorruptRun(t *testing.T) {
	r := newRig(t)
	d := filepath.Join(r.dir, "runs", "bad")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "journal.json"), []byte(`{"state":`+secretA), 0o600); err != nil {
		t.Fatal(err)
	}
	r.drain(r.m.Init())
	r.update(tea.WindowSizeMsg{Width: 400, Height: 24}) // the temp dir path is long
	idx := r.m.Ctx().RunIndex
	if !idx.Loaded || len(idx.Runs) != 0 || len(idx.Corrupt) != 1 || idx.Corrupt[0] != "bad" {
		t.Errorf("index = %+v", idx)
	}
	v := r.view()
	if !strings.Contains(v, "! skipped run bad: journal is corrupt · move "+filepath.Join(r.dir, "runs", "bad")+" out of "+filepath.Join(r.dir, "runs")) || strings.Contains(v, secretA) {
		t.Errorf("flash:\n%s", v)
	}
	// Applying beside it works.
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	r.drain(r.update(tui.ApplyMsg{}))
	if len(r.cs.Changes) != 0 {
		t.Errorf("apply beside a corrupt run: %+v\n%s", r.cs.Changes, r.view())
	}
}

// Update returns at once while another instance holds the prepare lock: the
// wait runs in the prepare command, and a second apply meanwhile is refused.
func TestPrepareWaitsOffUpdate(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	lock, err := workdir.AcquireLock(filepath.Join(r.dir, "prepare.lock"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cmd := r.update(tui.ApplyMsg{})
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("Update took %v while the prepare lock was held", d)
	}
	if cmd == nil {
		t.Fatalf("apply returned no command")
	}
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }()
	select {
	case <-got:
		t.Fatalf("the prepare command did not wait for the held lock")
	case <-time.After(100 * time.Millisecond):
	}
	if c := r.update(tui.ApplyMsg{}); c != nil {
		t.Errorf("second apply while preparing returned a command")
	}
	if c := r.update(tui.ImmediateMsg{Change: kinds.Restart(domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, time.Now())}); c != nil {
		t.Errorf("immediate while preparing returned a command")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	var msg tea.Msg
	select {
	case msg = <-got:
	case <-time.After(5 * time.Second):
		t.Fatalf("the prepare command did not end after the lock was released")
	}
	r.drain(r.update(msg))
	if len(r.cs.Changes) != 0 {
		t.Errorf("run not applied: %+v\n%s", r.cs.Changes, r.view())
	}
}

// A change set edited while the prepare lock is awaited is not frozen: the
// apply is refused and asks to apply again.
func TestPrepareRefusesChangeSetChangedWhilePreparing(t *testing.T) {
	for name, edit := range map[string]func(r *rig){
		"edit named": func(r *rig) {
			r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "edited")}))
		},
		"added": func(r *rig) {
			r.drain(r.update(tui.PutChangeMsg{Change: setChange("OTHER_VAR", "added")}))
		},
		"redeploy toggled": func(r *rig) {
			r.drain(r.update(tui.ToggleRedeployMsg{PlaceKey: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}.Key()}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.drain(r.m.Init())
			r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
			ids := []string(nil)
			if name == "edit named" {
				ids = []string{r.cs.Changes[0].ID}
			}
			cmd := r.update(tui.ApplyMsg{IDs: ids})
			if cmd == nil {
				t.Fatal("apply returned no command")
			}
			msg := cmd() // the lock is taken
			edit(r)
			r.drain(r.update(msg))
			if v := r.view(); !strings.Contains(v, "✗ the change set changed while preparing · apply again") {
				t.Errorf("flash missing:\n%s", v)
			}
			if runs, _ := r.runs.List(); len(runs) != 0 {
				t.Errorf("runs = %+v, want none", runs)
			}
			if _, err := os.Stat(filepath.Join(r.dir, "prepare.lock")); !os.IsNotExist(err) {
				t.Errorf("prepare lock left held: %v", err)
			}
			// Applying again works.
			r.drain(r.update(tui.ApplyMsg{}))
			if len(r.cs.Changes) != 0 {
				t.Errorf("second apply: %+v\n%s", r.cs.Changes, r.view())
			}
		})
	}
}

func TestApplyNamesMissingChange(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	id := r.cs.Changes[0].ID
	if cmd := r.update(tui.ApplyMsg{IDs: []string{id, "gone-1"}}); cmd != nil {
		t.Errorf("apply with a missing id returned a command")
	}
	if v := r.view(); !strings.Contains(v, "✗ change gone-1 not found") {
		t.Errorf("flash missing:\n%s", v)
	}
	// A duplicated id is one change.
	r.drain(r.update(tui.ApplyMsg{IDs: []string{id, id}}))
	if len(r.cs.Changes) != 0 {
		t.Errorf("duplicated id not applied: %+v\n%s", r.cs.Changes, r.view())
	}
}

// An immediate change is checked again after the wait for the lock.
func TestImmediateRecheckedAfterWait(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	cmd := r.update(tui.ImmediateMsg{Change: kinds.Restart(domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, time.Now())})
	if cmd == nil {
		t.Fatal("immediate returned no command")
	}
	msg := cmd()
	r.set.WriteEnvs = nil
	r.drain(r.update(tui.SaveSettingsMsg{}))
	r.drain(r.update(msg))
	if v := r.view(); !strings.Contains(v, "✗ shop/production is not in write environments · Settings w") {
		t.Errorf("flash missing:\n%s", v)
	}
	if runs, _ := r.runs.List(); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "prepare.lock")); !os.IsNotExist(err) {
		t.Errorf("prepare lock left held: %v", err)
	}
}

// Quitting while a prepare waits for the lock waits for it, releases the
// lock without preparing and then quits.
func TestQuitWhilePreparing(t *testing.T) {
	defer root.SetQuitWait(time.Millisecond)()
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	cmd := r.update(tui.ApplyMsg{})
	if q := r.update(key("q")); q != nil {
		for _, m := range runAll(q) {
			if _, ok := m.(tea.QuitMsg); ok {
				t.Fatal("quit while preparing did not wait")
			}
		}
	}
	quit := false
	// The lock is released in a command; the quit follows its result.
	for _, m := range r.drain(r.update(cmd())) {
		if _, ok := m.(tea.QuitMsg); ok {
			quit = true
		}
	}
	if !quit {
		t.Error("no quit after the prepare ended")
	}
	if runs, _ := r.runs.List(); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "prepare.lock")); !os.IsNotExist(err) {
		t.Errorf("prepare lock left held: %v", err)
	}
}

// An unreadable runs dir at startup unlocks nothing: no run is known gone.
func TestUnlockStaleKeepsHeldWhenRunsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 dir")
	}
	r := newRig(t, func(d *root.Deps) {
		c := setChange("NEW_VAR", "fresh")
		c.ID, c.RunID = "c-1", "20261024-120000-abcd"
		d.CS.Changes = append(d.CS.Changes, c)
		runs := filepath.Join(d.Runs.Dir, "runs")
		if err := os.MkdirAll(runs, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(runs, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(runs, 0o700) })
	})
	r.drain(r.m.Init())
	if len(r.cs.Changes) != 1 || r.cs.Changes[0].RunID == "" {
		t.Errorf("held change unlocked: %+v", r.cs.Changes)
	}
	if v := r.view(); !strings.Contains(v, "✗ runs directory is unreadable: changes held by runs stay held") {
		t.Errorf("flash missing:\n%s", v)
	}
}

// A run ending while quitting does not quit while a prepare still waits for
// (or holds) the prepare lock: the prepare ends first.
func TestRunDoneWhileQuittingWaitsForPrepare(t *testing.T) {
	defer root.SetQuitWait(time.Millisecond)()
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("B_NEW", "b")}))
	a, b := r.cs.Changes[0].ID, r.cs.Changes[1].ID
	cmds := split(r.prepare(tui.ApplyMsg{IDs: []string{a}}))
	if len(cmds) != 2 {
		t.Fatalf("apply commands = %d", len(cmds))
	}
	r.update(cmds[0]()) // save
	done := cmds[1]()   // the run ends; its events are not fed
	pending := r.update(tui.ApplyMsg{IDs: []string{b}})
	if pending == nil {
		t.Fatal("second apply returned no command")
	}
	r.update(key("q"))
	r.update(key("y"))
	hasQuit := func(cmd tea.Cmd) bool {
		for _, m := range runAll(cmd) {
			if _, ok := m.(tea.QuitMsg); ok {
				return true
			}
		}
		return false
	}
	if hasQuit(r.update(done)) {
		t.Fatal("quit on RunDone while a prepare is pending")
	}
	// The lock is released in a command; the quit follows its result.
	if !slices.ContainsFunc(r.drain(r.update(pending())), func(m tea.Msg) bool { _, ok := m.(tea.QuitMsg); return ok }) {
		t.Error("no quit after the prepare ended")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "prepare.lock")); !os.IsNotExist(err) {
		t.Errorf("prepare lock left held: %v", err)
	}
}

// A selection that cannot be fingerprinted is refused: drift could not be
// seen.
func TestApplyRefusedWhenSelectionUnmarshalable(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("NEW_VAR", "fresh")}))
	restore := root.SetMarshalSelection(func(any) ([]byte, error) { return nil, errors.New("zx" + secretA) })
	defer restore()
	if cmd := r.update(tui.ApplyMsg{}); cmd != nil {
		t.Errorf("apply returned a command")
	}
	v := r.view()
	if !strings.Contains(v, "✗ the change set cannot be checked for changes · not applied") || strings.Contains(v, secretA) {
		t.Errorf("flash:\n%s", v)
	}
	if runs, _ := r.runs.List(); len(runs) != 0 {
		t.Errorf("runs = %+v", runs)
	}
}

// claimer is a screen that claims every key.
type claimer struct{ fakeScreen }

func (*claimer) Claims(string) bool { return true }

func TestKeyClaimerGetsGlobalKeys(t *testing.T) {
	r := newRig(t)
	s := &claimer{fakeScreen{title: "Claim"}}
	r.update(tui.PushMsg{Screen: s})
	r.update(key("C"))
	if !strings.Contains(r.view(), "Claim body") {
		t.Fatalf("C left the claiming screen:\n%s", r.view())
	}
	if len(s.msgs) != 1 || s.msgs[0].(tea.KeyMsg).String() != "C" {
		t.Errorf("screen got %#v", s.msgs)
	}
	// ctrl+c is never claimed
	r.update(key("ctrl+c"))
	if len(s.msgs) != 1 {
		t.Fatalf("ctrl+c forwarded: %#v", s.msgs)
	}
}

func TestChangesMsg(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.ChangesMsg{
		Put:  []changeset.Change{setChange("A_NEW", "a"), setChange("C_NEW", "c")},
		Done: "2 places: new value",
	}))
	if len(r.cs.Changes) != 2 || !strings.Contains(r.view(), "✓ 2 places: new value") {
		t.Fatalf("changes = %d:\n%s", len(r.cs.Changes), r.view())
	}
	if saved, err := changeset.Load(r.deps.ChangeSetPath); err != nil || len(saved.Changes) != 2 {
		t.Errorf("saved = %+v, %v", saved, err)
	}

	ref := setChange("B_NEW", "${{A_NEW}}")
	nv, _ := json.Marshal(map[string]any{"value": "${{A_NEW}}", "refs": []string{domain.LocationID("e1", "s1", "A_NEW")}})
	ref.New = nv
	r.drain(r.update(tui.PutChangeMsg{Change: ref}))
	aID, cID := r.cs.Changes[0].ID, r.cs.Changes[1].ID

	// A refused remove shows its ✗, not Done; the other remove is kept.
	r.drain(r.update(tui.ChangesMsg{Remove: []string{cID, aID}, Done: "2 places: decision undone"}))
	v := r.view()
	if !strings.Contains(v, "✗ A_NEW is referenced by a pending change: remove that first") || strings.Contains(v, "decision undone") {
		t.Errorf("refusal hidden:\n%s", v)
	}
	if _, ok := r.cs.Get(cID); ok || len(r.cs.Changes) != 2 {
		t.Errorf("the succeeding remove was not kept: %d changes", len(r.cs.Changes))
	}

	// A refused put (invalid) shows its ✗ too; Kind defaults to ✓ and is
	// kept otherwise.
	r.drain(r.update(tui.ChangesMsg{Put: []changeset.Change{setChange("bad name", "x")}, Done: "1 place: new value"}))
	if v := r.view(); !strings.Contains(v, "✗") || strings.Contains(v, "1 place: new value") {
		t.Errorf("invalid put not refused:\n%s", v)
	}
	r.drain(r.update(tui.ChangesMsg{Put: []changeset.Change{setChange("D_NEW", "d")}, Kind: '!', Done: "some places are already rotated"}))
	if v := r.view(); !strings.Contains(v, "! some places are already rotated") {
		t.Errorf("Done kind lost:\n%s", v)
	}
}

// A keep (a marker) writes nothing: it is applied in a read-only
// environment, where a write is refused.
func TestApplyKeepInReadOnlyEnv(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.set.WriteEnvs = nil
	r.drain(r.update(tui.SaveSettingsMsg{}))
	keep := kinds.Keep(domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, "DB_URL", "rotate-db:e1/s9:root", time.Now())
	r.drain(r.update(tui.PutChangeMsg{Change: keep}))
	r.drain(r.update(tui.ApplyMsg{}))
	if v := r.view(); strings.Contains(v, "not in write environments") {
		t.Errorf("a keep was refused in a read-only environment:\n%s", v)
	}
	if len(r.cs.Changes) != 0 {
		t.Errorf("change set after the run: %+v", r.cs.Changes)
	}
	if n := len(r.fake.Mutations()); n != 0 {
		t.Errorf("mutations = %d, want none", n)
	}
}

// Removing a database password change removes the changes its decision
// made by itself; the places the user decided stay, and the flash says so.
func TestRemovePasswordChangeTakesRewrites(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	group := "rotate-db:e1/s1:root"
	auto := &changeset.Origin{Producer: "rotate", Row: group, Letter: changeset.LetterRewrite}
	pw := changeset.Change{ID: "pw", Kind: "db.password", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, Target: "root", Group: group, Origin: auto}
	rw := setChange("URL_A", "a")
	rw.ID, rw.Group, rw.Origin, rw.Requires = "rw", group, auto, []string{"pw"}
	dec := setChange("URL_B", "b")
	dec.ID, dec.Group, dec.Requires = "dec", group, []string{"pw"}
	dec.Origin = &changeset.Origin{Producer: "rotate", Row: group, Letter: "p"}
	other := setChange("OTHER", "o")
	other.ID = "other"
	for _, c := range []changeset.Change{pw, rw, dec, other} {
		if err := r.cs.Put(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	r.drain(r.update(tui.RemoveChangeMsg{ID: "pw"}))
	var left []string
	for _, c := range r.cs.Changes {
		left = append(left, c.ID)
	}
	if !slices.Equal(left, []string{"dec", "other"}) {
		t.Errorf("left = %v", left)
	}
	if !strings.Contains(r.view(), "1 decided places kept · they no longer follow a password change") {
		t.Errorf("flash:\n%s", r.view())
	}
}

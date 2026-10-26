package services_test

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/tui/screens/services"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	services.FakeTicks()
	os.Exit(m.Run())
}

const secret = "fixture-value-alpha"

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

var catalog = railway.Catalog{Projects: []railway.Project{
	{
		ID: "p1", Name: "shop", Workspace: "ws",
		Environments: []railway.Environment{
			{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2"}},
			{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"s1"}},
		},
		Services: []railway.Service{{ID: "s1", Name: "api"}, {ID: "s2", Name: "web"}},
	},
}}

func ctx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(catalog)
	st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "DATABASE_URL", Unrendered: secret, Rendered: secret},
	})
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views:    &view.Views{Store: st, Reg: apply.Registry{}},
		Settings: &settings.Settings{Projects: []string{"p1"}, WriteEnvs: []string{"e1", "e2"}},
		Theme:    tui.NewTheme(tuitest.Profile()),
		Width:    w, Height: h,
		Now: func() time.Time { return now },
	}
}

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// settle runs cmd and feeds the component messages it yields back into the
// screen, returning the other messages (the intents for the root).
func settle(t *testing.T, c *tui.Ctx, s tui.Screen, cmd tea.Cmd) (tui.Screen, []tea.Msg) {
	t.Helper()
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
		case components.ConfirmedMsg, components.CancelMsg, components.SubmitMsg:
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

func render(c *tui.Ctx, s tui.Screen) string {
	return tuitest.Plain(tui.Frame(c, s.Title(), "", s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
}

func TestServicesPendingMark(t *testing.T) {
	c := ctx(150, 20) // wide enough for the env names
	c.CS.Put(changeset.Change{ID: "c1", Kind: "service.redeploy", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, Target: "redeploy", Created: now}, nil)
	out := render(c, services.New())
	var apiProd, web string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "production") && strings.Contains(l, "api") {
			apiProd = l
		}
		if strings.Contains(l, "production") && strings.Contains(l, "web") {
			web = l
		}
	}
	if !strings.Contains(apiProd, "● 1") {
		t.Errorf("api row = %q", apiProd)
	}
	if web == "" || strings.Contains(web, "●") {
		t.Errorf("web row = %q", web)
	}
}

func down(t *testing.T, c *tui.Ctx, s tui.Screen, n int) tui.Screen {
	for range n {
		s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyDown})
	}
	return s
}

func TestServicesRestartConfirm(t *testing.T) {
	c := ctx(80, 20)
	s := services.New()
	s, cmd := s.Update(c, r("R"))
	if cmd != nil || !s.(tui.Capturer).Capturing() {
		t.Fatalf("R should open a prompt and capture keys")
	}
	if v := render(c, s); !strings.Contains(v, "restart production · api now? (y/n)") {
		t.Errorf("prompt missing:\n%s", v)
	}
	s, cmd = s.Update(c, r("y"))
	_, msgs := settle(t, c, s, cmd)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	im, ok := msgs[0].(tui.ImmediateMsg)
	if !ok || im.Change.Kind != "service.restart" || im.Change.Place.ServiceID != "s1" || !im.Change.Created.Equal(now) {
		t.Fatalf("msg = %#v", msgs[0])
	}

	// n cancels without an intent and releases the keys
	s = services.New()
	s, _ = s.Update(c, r("R"))
	s, cmd = s.Update(c, r("n"))
	s, msgs = settle(t, c, s, cmd)
	if len(msgs) != 0 || s.(tui.Capturer).Capturing() {
		t.Errorf("n: msgs = %#v capturing = %v", msgs, s.(tui.Capturer).Capturing())
	}
}

func TestServicesEnterOpensVariables(t *testing.T) {
	c := ctx(80, 20)
	s := down(t, c, services.New(), 1)
	_, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	om, ok := cmd().(tui.OpenMsg)
	want := c.Views.Services(c.CS, []string{"e1", "e2"})[1].Place
	if !ok || om.Name != "variables" || len(om.Args) != 1 || om.Args[0] != want {
		t.Fatalf("msg = %#v, want place %v", om, want)
	}
}

func TestServicesRedeployQueues(t *testing.T) {
	c := ctx(80, 20)
	_, cmd := services.New().Update(c, r("d"))
	pm, ok := cmd().(tui.PutChangeMsg)
	if !ok || pm.Change.Kind != "service.redeploy" || pm.Change.Place.EnvID != "e1" {
		t.Fatalf("msg = %#v", pm)
	}
	if want := kinds.Redeploy(pm.Change.Place, now); want.Kind != pm.Change.Kind {
		t.Error("kind mismatch")
	}
}

// An environment outside the write environments takes no change here, as
// on Variables: d, R, l and z say so at once instead of staging what apply
// would refuse.
func TestServicesRefusesReadOnlyEnv(t *testing.T) {
	c := ctx(80, 20)
	c.Settings.WriteEnvs = []string{"e2"}
	for _, key := range []string{"d", "R", "l", "z"} {
		s, cmd := services.New().Update(c, r(key))
		if s.(tui.Capturer).Capturing() {
			t.Errorf("%s opened a prompt for a read-only environment", key)
		}
		_, msgs := settle(t, c, s, cmd)
		if len(msgs) != 1 {
			t.Fatalf("%s: msgs = %#v", key, msgs)
		}
		if f, ok := msgs[0].(tui.FlashMsg); !ok || !strings.Contains(f.Text, "not in write environments") {
			t.Errorf("%s: msg = %#v", key, msgs[0])
		}
	}
}

func TestServicesFilter(t *testing.T) {
	c := ctx(80, 20)
	s := services.New()
	s, _ = s.Update(c, r("/"))
	if !s.(tui.Capturer).Capturing() {
		t.Fatal("filter should capture keys")
	}
	s, _ = s.Update(c, r("web"))
	out := render(c, s)
	if strings.Contains(out, "api") || !strings.Contains(out, "web") {
		t.Errorf("filtered:\n%s", out)
	}
	s, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	s, _ = settle(t, c, s, cmd)
	if s.(tui.Capturer).Capturing() || strings.Contains(render(c, s), "api") {
		t.Errorf("enter should keep the filter and release keys")
	}
	// esc clears
	s, _ = s.Update(c, r("/"))
	s, cmd = s.Update(c, tea.KeyMsg{Type: tea.KeyEsc})
	s, _ = settle(t, c, s, cmd)
	if !strings.Contains(render(c, s), "api") {
		t.Errorf("esc should clear the filter")
	}
}

func TestServicesBuild(t *testing.T) {
	if services.Build() == nil {
		t.Error("Build() = nil")
	}
	if services.Build("x") != nil {
		t.Error("Build with args should be nil")
	}
}

func TestServicesWindowOnlyVisible(t *testing.T) {
	c := ctx(80, 3+tui.ChromeRows) // body height 3: header + 2 rows
	out := render(c, services.New())
	if n := strings.Count(out, "shop "); n != 2 {
		t.Errorf("rendered %d rows:\n%s", n, out)
	}
}

func TestServicesGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 16)
		c.CS.Put(changeset.Change{ID: "c1", Kind: "service.redeploy", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, Target: "redeploy", Created: now}, nil)
		s := down(t, c, services.New(), 1)
		out := render(c, s)
		tuitest.NoSecrets(t, out, secret)
		tuitest.Golden(t, "services_"+map[int]string{80: "80", 60: "60"}[w]+".golden", out)
	}
}

func TestFilterArrowsLeaveEditingAndMoveCursor(t *testing.T) {
	c := ctx(80, 20)
	s := services.New()
	s, _ = s.Update(c, r("/"))
	s, _ = s.Update(c, r("o")) // matches production (3 rows) and staging
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyDown})
	if s.(tui.Capturer).Capturing() {
		t.Fatal("down should leave editing")
	}
	s, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	_, msgs := settle(t, c, s, cmd)
	om, ok := only(t, msgs).(tui.OpenMsg)
	if !ok || om.Args[0].(domain.Place).ServiceID != "s2" {
		t.Errorf("cursor should have moved to the second filtered row: %#v", msgs)
	}
	if !strings.Contains(render(c, s), "/ o  ") {
		t.Errorf("field should stay with its text:\n%s", render(c, s))
	}
}

func TestFilterRestoredOnReopen(t *testing.T) {
	c := ctx(80, 20)
	s := services.New()
	s, _ = s.Update(c, r("/"))
	s, _ = s.Update(c, r("web"))
	_, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	out := render(c, services.New())
	if strings.Contains(out, "api") || !strings.Contains(out, "/ web") {
		t.Errorf("reopened screen should restore the filter:\n%s", out)
	}
}

func TestServicesFilterGoldens(t *testing.T) {
	for _, w := range []int{80} {
		c := ctx(w, 16)
		s := services.New()
		s, _ = s.Update(c, r("/"))
		s, _ = s.Update(c, r("api"))
		out := render(c, s)
		tuitest.Golden(t, "services_filter_80.golden", out)
	}
}

func TestFilterCursorClampsToShownRows(t *testing.T) {
	c := ctx(80, 20)
	s := down(t, c, services.New(), 2) // third row: staging/api
	s, _ = s.Update(c, r("/"))
	s, _ = s.Update(c, r("web")) // one row left
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	s, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	_, msgs := settle(t, c, s, cmd)
	om, ok := only(t, msgs).(tui.OpenMsg)
	if !ok || om.Args[0].(domain.Place).ServiceID != "s2" {
		t.Errorf("enter should act on the shown row: %#v", msgs)
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("enter should release keys")
	}
}

func TestServicesFilterKeptGolden(t *testing.T) {
	c := ctx(80, 16)
	s := services.New()
	s, _ = s.Update(c, r("/"))
	s, _ = s.Update(c, r("api"))
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	out := render(c, s)
	if strings.Contains(out, "▏") {
		t.Error("no caret when not editing")
	}
	tuitest.Golden(t, "services_filter_kept_80.golden", out)
}

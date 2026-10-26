package home_test

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/home"
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
			{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2"}},
			{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"s1"}},
		},
		Services: []railway.Service{{ID: "s1", Name: "api"}, {ID: "s2", Name: "web"}},
	},
	{
		ID: "p2", Name: "blog", Workspace: "ws",
		Environments: []railway.Environment{{ID: "e3", Name: "production", CanAccess: true, ServiceIDs: []string{"s3"}}},
		Services:     []railway.Service{{ID: "s3", Name: "site"}},
	},
}}

func ctx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(catalog)
	return &tui.Ctx{
		Store:    st,
		CS:       &changeset.ChangeSet{},
		Settings: &settings.Settings{Projects: []string{"p1", "p2"}},
		Theme:    tui.NewTheme(tuitest.Profile()),
		Account:  "fake@example.test",
		Width:    w, Height: h,
		Now:     func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) },
		Running: map[string]tui.RunProgress{},
	}
}

func render(c *tui.Ctx, s tui.Screen) string {
	note := ""
	if n, ok := s.(tui.Noter); ok {
		note = n.Note(c)
	}
	return tuitest.Plain(tui.Frame(c, s.Title(), note, s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
}

// Home shows the signed-in account under its logo, not in the frame's
// header; before the account is known it says so.
func TestHomeHeaderAccount(t *testing.T) {
	c := ctx(80, 28)
	out := render(c, home.New())
	header := strings.Split(out, "\n")[1]
	if strings.TrimSpace(strings.Trim(header, "┃")) != "railway-manager Home" || !strings.Contains(out, "fake@example.test") {
		t.Errorf("account not in the body:\n%s", out)
	}
	c.Account = ""
	if out := render(c, home.New()); !strings.Contains(out, "not signed in") {
		t.Errorf("no account:\n%s", out)
	}
}

func loaded(c *tui.Ctx) {
	c.Store.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "DATABASE_URL", Unrendered: secret, Rendered: secret},
		{EnvID: "e1", ServiceID: "", Name: "SHARED", Unrendered: "x", Rendered: "x"},
	})
	c.Store.ReplaceVariables("p2", []railway.VarRecord{{EnvID: "e3", ServiceID: "s3", Name: "TOKEN", Unrendered: secret, Rendered: secret}})
	c.Store.FinishLoad("inventory:p1", c.Now())
	c.Store.FinishLoad("inventory:p2", c.Now())
}

func TestHomeGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 22)
		s := home.New()
		c.Store.FinishLoad("catalog", c.Now())
		c.Store.BeginLoad("inventory:p1")
		c.Store.BeginLoad("inventory:p2")
		c.Store.FinishLoad("inventory:p2", c.Now())
		out := render(c, s)
		tuitest.NoSecrets(t, out, secret)
		tuitest.Golden(t, "home_"+itoa(w)+"_loading.golden", out)
		if !strings.Contains(out, "reading variables 1/2 projects") {
			t.Errorf("progress line missing:\n%s", out)
		}

		loaded(c)
		c.Store.FailLoad("inventory:p2", "Discover: connection refused")
		c.CS.Changes = append(c.CS.Changes, changeset.Change{ID: "c1"})
		c.Running["20261024-120000-ab12"] = tui.RunProgress{Done: 2, Total: 5, State: "running"}
		out = render(c, s)
		tuitest.NoSecrets(t, out, secret)
		tuitest.Golden(t, "home_"+itoa(w)+"_loaded.golden", out)
		if !strings.Contains(out, "✗ blog: Discover: connection refused · r retries") {
			t.Errorf("failed load missing:\n%s", out)
		}
	}
}

func itoa(n int) string {
	if n == 80 {
		return "80"
	}
	return "60"
}

func TestHomeNoProjects(t *testing.T) {
	c := ctx(80, 20)
	c.Settings.Projects = nil
	out := home.New().View(c, 76, 13)
	if !strings.Contains(out, "no projects selected · o settings") {
		t.Errorf("view:\n%s", out)
	}
}

func TestHomeKeys(t *testing.T) {
	c := ctx(80, 20)
	s := home.New()
	for k, want := range map[string]tea.Msg{
		"s": tui.OpenMsg{Name: "services"},
		"v": tui.OpenMsg{Name: "variables"},
		"h": tui.OpenMsg{Name: "history"},
		"R": tui.OpenMsg{Name: "rotate"},
		"c": tui.OpenMsg{Name: "changeset"},
		"o": tui.OpenMsg{Name: "settings"},
		"r": tui.RefreshMsg{},
	} {
		_, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		if cmd == nil {
			t.Errorf("%s: no command", k)
			continue
		}
		got := cmd()
		if g, ok := got.(tui.OpenMsg); ok {
			if w, ok := want.(tui.OpenMsg); !ok || g.Name != w.Name || len(g.Args) != 0 {
				t.Errorf("%s: %#v", k, got)
			}
		} else if _, ok := got.(tui.RefreshMsg); !ok || k != "r" {
			t.Errorf("%s: %#v", k, got)
		}
	}
}

// labels are the menu's items in order.
var labels = []string{"Services", "Variables", "Rotate secrets", "Change set", "History", "Settings"}

// menuLine is the view's line of the item labelled label; "" when none.
func menuLine(view, label string) string {
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(ansi.Strip(l), " "+label+" ") {
			return l
		}
	}
	return ""
}

// The items come in order, each with its key; the hints list the keys.
func TestHomeMenuOrder(t *testing.T) {
	c := ctx(80, 28)
	out := ansi.Strip(home.New().View(c, 76, 24))
	last := -1
	for _, label := range labels {
		i := strings.Index(out, " "+label+" ")
		if i < 0 || i < last {
			t.Errorf("%s missing or out of order:\n%s", label, out)
		}
		last = i
	}
	for label, key := range map[string]string{"Services": "s", "Rotate secrets": "R", "Settings": "o"} {
		if l := strings.TrimRight(ansi.Strip(menuLine(out, label)), " "); !strings.HasSuffix(l, " "+key) {
			t.Errorf("%s line %q does not end in its key %s", label, l, key)
		}
	}
	var keys []string
	for _, h := range home.New().Keys() {
		if len(h.Key) == 1 {
			keys = append(keys, h.Key)
		}
	}
	if got := strings.Join(keys, ""); got != "rsvRchoq" {
		t.Errorf("key hints = %q, want rsvRchoq", got)
	}
}

// Services, Variables, History and Rotate secrets are dimmed until a selected project's
// variables were read once; the keys still open them.
func TestHomeDimsUntilFirstRead(t *testing.T) {
	c := ctx(80, 28)
	c.Theme = tui.NewTheme(termenv.ANSI256)
	accent, _, _ := strings.Cut(c.Theme.Accent.Render("x"), "x")
	s := home.New()
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyEnd}) // the cursor off the items checked
	dim := []string{"Services", "Variables", "History", "Rotate secrets"}
	c.Store.BeginLoad("inventory:p1")
	for _, label := range dim {
		if l := menuLine(s.View(c, 76, 24), label); l == "" || strings.Contains(l, accent) {
			t.Errorf("%s before the first read is not dimmed: %q", label, l)
		}
	}
	if l := menuLine(s.View(c, 76, 24), "Change set"); !strings.Contains(l, accent) {
		t.Errorf("change set is dimmed: %q", l)
	}
	if _, cmd := home.New().Update(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")}); cmd == nil {
		t.Error("a dimmed key should still open its screen")
	}
	c.Store.FinishLoad("inventory:p1", c.Now())
	for _, label := range dim {
		if l := menuLine(s.View(c, 76, 24), label); !strings.Contains(l, accent) {
			t.Errorf("%s after the first read is still dimmed: %q", label, l)
		}
	}
}

// The arrows move the cursor over the menu and enter opens the item.
func TestHomeCursor(t *testing.T) {
	c := ctx(80, 28)
	s := home.New()
	selected := func() string {
		l := ""
		for _, x := range strings.Split(ansi.Strip(s.View(c, 76, 24)), "\n") {
			if strings.Contains(x, "▌") {
				l = x
			}
		}
		for _, label := range labels {
			if strings.Contains(l, " "+label+" ") {
				return label
			}
		}
		return ""
	}
	if got := selected(); got != "Services" {
		t.Fatalf("cursor starts on %q, want Services", got)
	}
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyDown})
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if got := selected(); got != "Rotate secrets" {
		t.Errorf("after down, j: %q, want Rotate secrets", got)
	}
	s, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	if m, ok := cmd().(tui.OpenMsg); !ok || m.Name != "rotate" {
		t.Errorf("enter on Rotate secrets: %v", cmd)
	}
	for range 3 {
		s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyUp}) // stays on the first
	}
	if got := selected(); got != "Services" {
		t.Errorf("up past the top: %q", got)
	}
	// a letter opens its item and moves the cursor there
	s, cmd = s.Update(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if m, ok := cmd().(tui.OpenMsg); !ok || m.Name != "settings" || selected() != "Settings" {
		t.Errorf("o: %v, cursor %q", cmd, selected())
	}
	// r refreshes; it is not an item
	if _, cmd = s.Update(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd == nil || cmd() != (tui.RefreshMsg{}) {
		t.Errorf("r: %v", cmd)
	}
}

// The logo shows where it fits; a narrow or short screen gets the name.
func TestHomeLogo(t *testing.T) {
	c := ctx(80, 28)
	if out := home.New().View(c, 76, 24); !strings.Contains(out, "█") {
		t.Errorf("no logo at 76×24:\n%s", out)
	}
	for _, size := range [][2]int{{56, 24}, {76, 12}} {
		out := ansi.Strip(home.New().View(c, size[0], size[1]))
		if strings.Contains(out, "█") || !strings.Contains(out, "railway-manager") {
			t.Errorf("%v:\n%s", size, out)
		}
		for _, l := range strings.Split(out, "\n") {
			if ansi.StringWidth(l) > size[0] {
				t.Errorf("%v: line %q is %d wide", size, l, ansi.StringWidth(l))
			}
		}
	}
}

// A variable read from an unselected environment is not counted.
func TestHomeVariableCountSelectedEnvsOnly(t *testing.T) {
	c := ctx(80, 20)
	c.Settings = &settings.Settings{Projects: []string{"p1"}, Envs: []string{"e1"}}
	c.Store.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "A", Unrendered: "1", Rendered: "1"},
		{EnvID: "e2", ServiceID: "s1", Name: "B", Unrendered: "2", Rendered: "2"},
		{EnvID: "e2", ServiceID: "s1", Name: "C", Unrendered: "3", Rendered: "3"},
	})
	out := ansi.Strip(home.New().View(c, 76, 13))
	if !strings.Contains(out, "1 variable") || strings.Contains(out, "3 variables") {
		t.Errorf("view:\n%s", out)
	}
}

// A full Home frame, as the README shows it.
func TestHomeFullGolden(t *testing.T) {
	c := ctx(100, 30)
	c.Token = "config · expires 14:05"
	loaded(c)
	c.Settings.Since = "2026-10-20T12:00:00Z"
	c.Settings.WriteEnvs = []string{"e1"}
	c.CS.Changes = append(c.CS.Changes, changeset.Change{ID: "c1", Place: domain.Place{EnvID: "e1", ServiceID: "s1"}})
	c.Running["20261024-120000-ab12"] = tui.RunProgress{Done: 2, Total: 5, State: "running"}
	out := render(c, home.New())
	tuitest.NoSecrets(t, out, secret)
	tuitest.Golden(t, "home_full_100.golden", out)
}

package auth_test

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/auth"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

const token = "fixture-token-value"

func ctx(w, h int) *tui.Ctx {
	return &tui.Ctx{API: &railway.Client{}, Theme: tui.NewTheme(tuitest.Profile()), Width: w, Height: h}
}

func render(c *tui.Ctx, s tui.Screen) string {
	return tuitest.Plain(tui.Frame(c, s.Title(), "", s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
}

func TestAuthGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 20)
		s := auth.New("no Railway token: set RAILWAY_API_TOKEN or run railway login")
		out := render(c, s)
		tuitest.NoSecrets(t, out, token)
		name := "auth_80.golden"
		if w == 60 {
			name = "auth_60.golden"
		}
		tuitest.Golden(t, name, out)
	}
}

func TestAuthEnterRetries(t *testing.T) {
	c := ctx(80, 20)
	s := auth.New("expired\x1b[2J")
	if out := s.View(c, 76, 13); strings.Contains(out, "\x1b[2J") {
		t.Errorf("reason not sanitized")
	}
	s, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter returned no command")
	}
	if !strings.Contains(s.View(c, 76, 13), "checking") {
		t.Errorf("no checking state")
	}
	// a failure (delivered by the root) shows the new reason and ends checking
	s.(*auth.Screen).SetReason("still rejected")
	if v := s.View(c, 76, 13); !strings.Contains(v, "still rejected") || strings.Contains(v, "checking") {
		t.Errorf("new reason not shown:\n%s", v)
	}
}

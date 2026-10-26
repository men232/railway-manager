// Package auth is the sign-in screen, pushed when Railway rejects or lacks a
// token.
package auth

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
)

// Screen shows why signing in failed and where the token comes from.
type Screen struct {
	reason   string
	checking bool
}

// New returns the screen with the reason the token was refused.
func New(reason string) tui.Screen { return &Screen{reason: reason} }

func (s *Screen) Init(*tui.Ctx) tea.Cmd { return nil }

func (s *Screen) Title() string { return "Sign in" }

func (s *Screen) Keys() []tui.KeyHint {
	return []tui.KeyHint{
		{Key: "enter", Label: "check again", Always: true},
		{Key: "q", Label: "back", Rank: 1},
	}
}

// Update re-runs WhoAmI on Enter. The root handles the answer wherever this
// screen is on the stack: success removes it and reads the catalog again,
// failure calls SetReason.
func (s *Screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.Type == tea.KeyEnter && !s.checking {
		s.checking = true
		return s, loaders.WhoAmI(ctx.API)
	}
	return s, nil
}

// SetReason shows why signing in failed and ends the "checking" state.
func (s *Screen) SetReason(reason string) { s.reason, s.checking = reason, false }

func (s *Screen) View(ctx *tui.Ctx, width, height int) string {
	th := ctx.Theme
	lines := []string{
		th.Accent.Render("Not signed in to Railway"),
		th.Danger.Render(text.Sanitize(s.reason)),
		"",
		"A token is read from, in order:",
		"  1. RAILWAY_API_TOKEN",
		"  2. RAILWAY_TOKEN",
		"  3. ~/.railway/config.json (railway login)",
		"",
	}
	if s.checking {
		lines = append(lines, th.Muted.Render(ctx.Icons.Run()+" checking…"))
	} else {
		lines = append(lines, "run "+th.Accent.Render("railway login")+" in another terminal, then press Enter")
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

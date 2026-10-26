// Package keys is the key list opened with ?: every key of the screen below
// and the global keys.
package keys

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
)

// Global are the keys the root model handles on every screen.
var Global = []tui.KeyHint{
	{Key: "C", Label: "change set"},
	{Key: "?", Label: "keys"},
	{Key: "q/esc", Label: "back (quit at Home)"},
	{Key: "ctrl+c", Label: "back (quit at Home)"},
}

type screen struct {
	title string
	hints []tui.KeyHint
}

// New builds the key list. args are the title of the screen below (string)
// and its hints ([]tui.KeyHint); either may be missing.
func New(args ...any) tui.Screen {
	s := &screen{}
	for _, a := range args {
		switch v := a.(type) {
		case string:
			s.title = v
		case []tui.KeyHint:
			s.hints = v
		}
	}
	return s
}

func (s *screen) Init(*tui.Ctx) tea.Cmd                          { return nil }
func (s *screen) Update(*tui.Ctx, tea.Msg) (tui.Screen, tea.Cmd) { return s, nil }
func (s *screen) Title() string                                  { return "Keys" }
func (s *screen) Keys() []tui.KeyHint {
	return []tui.KeyHint{{Key: "esc", Label: "back", Always: true}}
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	th := ctx.Theme
	var lines []string
	section := func(name string, hs []tui.KeyHint) {
		if len(hs) == 0 {
			return
		}
		lines = append(lines, th.Muted.Render(text.Sanitize(name)))
		for _, h := range hs {
			k := text.Sanitize(h.Key)
			pad := max(1, 8-ansi.StringWidth(k))
			lines = append(lines, "  "+th.Accent.Render(k)+strings.Repeat(" ", pad)+text.Sanitize(h.Label))
		}
		lines = append(lines, "")
	}
	name := s.title
	if name == "" {
		name = "this screen"
	}
	section(name, s.hints)
	section("everywhere", Global)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

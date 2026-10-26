// Package picker is the multi-select list: projects, environments and the
// like. It reports the chosen ids through the caller's onDone message.
package picker

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
)

// Item is one choice. ID is what onDone receives; Label and Note are shown.
type Item struct{ ID, Label, Note string }

type screen struct {
	title  string
	items  []Item
	chosen map[string]bool
	onDone func(selected []string) tea.Msg
	list   components.List
	filter *components.Filter
}

// New builds a picker. selected seeds the checked ids (it is copied). Enter
// pops the picker and then delivers onDone's message to the screen below.
func New(title string, items []Item, selected map[string]bool, onDone func(selected []string) tea.Msg) tui.Screen {
	c := make(map[string]bool, len(selected))
	for _, it := range items {
		if selected[it.ID] {
			c[it.ID] = true
		}
	}
	return &screen{title: title, items: items, chosen: c, onDone: onDone, filter: components.NewFilter("picker:" + title)}
}

// Build is the registry constructor: args are (title string, items
// []Item, selected map[string]bool, onDone func([]string) tea.Msg). It
// returns nil when an argument has another type.
func Build(args ...any) tui.Screen {
	var (
		title  string
		items  []Item
		sel    map[string]bool
		onDone func([]string) tea.Msg
	)
	for _, a := range args {
		switch v := a.(type) {
		case string:
			title = v
		case []Item:
			items = v
		case map[string]bool:
			sel = v
		case func([]string) tea.Msg:
			onDone = v
		default:
			return nil
		}
	}
	return New(title, items, sel, onDone)
}

func (s *screen) Init(ctx *tui.Ctx) tea.Cmd { s.filter.Load(ctx); return nil }

// Capturing reports whether the filter field takes every key.
func (s *screen) Capturing() bool { return s.filter.Editing() }

// visible is the items matching the filter.
func (s *screen) visible(ctx *tui.Ctx) []Item {
	s.filter.Load(ctx)
	if !s.filter.Active() {
		return s.items
	}
	var out []Item
	for _, it := range s.items {
		if s.filter.Match(it.Label + " " + it.Note) {
			out = append(out, it)
		}
	}
	return out
}

func (s *screen) Title() string {
	if s.title == "" {
		return "Pick"
	}
	return s.title
}

func (s *screen) Keys() []tui.KeyHint {
	if s.filter.Editing() {
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	return []tui.KeyHint{
		{Key: "space", Label: "toggle", Always: true},
		{Key: "enter", Label: "confirm", Always: true},
		{Key: "a", Label: "all", Rank: 1},
		{Key: "n", Label: "none", Rank: 2},
		{Key: "esc", Label: "cancel", Rank: 3},
		{Key: "/", Label: "filter", Rank: 4, Optional: true},
	}
}

func (s *screen) selected() []string {
	var out []string
	for _, it := range s.items {
		if s.chosen[it.ID] {
			out = append(out, it.ID)
		}
	}
	return out
}

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if h, cmd := s.filter.Update(ctx, k); h {
		return s, cmd
	}
	page := max(1, ctx.Height-tui.ChromeRows-1)
	vis := s.visible(ctx)
	n := len(vis)
	switch k.Type {
	case tea.KeyUp:
		s.list.Move(n, -1, page)
	case tea.KeyDown:
		s.list.Move(n, 1, page)
	case tea.KeyEnter:
		sel := s.selected()
		if s.onDone == nil {
			return s, tui.Emit(tui.PopMsg{})
		}
		done := s.onDone
		return s, tea.Sequence(tui.Emit(tui.PopMsg{}), func() tea.Msg { return done(sel) })
	case tea.KeyEsc:
		return s, tui.Emit(tui.PopMsg{})
	case tea.KeySpace:
		s.toggle(vis)
	case tea.KeyRunes:
		switch string(k.Runes) {
		case " ":
			s.toggle(vis)
		case "j":
			s.list.Move(n, 1, page)
		case "k":
			s.list.Move(n, -1, page)
		case "a":
			for _, it := range vis {
				s.chosen[it.ID] = true
			}
		case "n":
			for _, it := range vis {
				delete(s.chosen, it.ID)
			}
		case "q":
			return s, tui.Emit(tui.PopMsg{})
		}
	}
	return s, nil
}

func (s *screen) toggle(vis []Item) {
	if len(vis) == 0 {
		return
	}
	s.list.Cursor = max(0, min(s.list.Cursor, len(vis)-1))
	id := vis[s.list.Cursor].ID
	if s.chosen[id] {
		delete(s.chosen, id)
	} else {
		s.chosen[id] = true
	}
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	th := ctx.Theme
	if len(s.items) == 0 {
		return th.Muted.Render("nothing to pick")
	}
	vis := s.visible(ctx)
	var lines []string
	if f := s.filter.View(width, len(vis), len(s.items)); f != "" {
		lines = append(lines, f)
	}
	rows := max(0, height-1-len(lines))
	from, to := s.list.Window(len(vis), rows)
	if len(vis) == 0 {
		lines = append(lines, th.Muted.Render("nothing matches the filter"))
	}
	for i := from; i < to; i++ {
		it := vis[i]
		box := "[ ]"
		if s.chosen[it.ID] {
			box = "[x]"
		}
		line := box + " " + text.Sanitize(it.Label)
		if it.Note != "" {
			line += "  " + text.Sanitize(it.Note)
		}
		line = ansi.Truncate(line, width, "…")
		if i == s.list.Cursor {
			line = th.Selected.Render(line + strings.Repeat(" ", max(0, width-ansi.StringWidth(line))))
		}
		lines = append(lines, line)
	}
	lines = append(lines, th.Muted.Render(ansi.Truncate(
		strings.Join([]string{strconv.Itoa(len(s.selected())), "of", strconv.Itoa(len(s.items)), "selected"}, " "), width, "…")))
	if height < len(lines) {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

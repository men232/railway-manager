package components

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
)

// Filter is the shared "/" list filter. Its text is kept in Ctx.Filters under
// its key, so leaving and reopening a screen restores it.
type Filter struct {
	key     string
	ed      editor
	editing bool
	loaded  bool
}

// NewFilter returns an inactive filter that saves under key.
func NewFilter(key string) *Filter { return &Filter{key: key} }

// Load restores the text saved under the key. It acts once: later calls do
// nothing, so a screen may call it from Init and again defensively.
func (f *Filter) Load(ctx *tui.Ctx) {
	if f.loaded {
		return
	}
	f.loaded = true
	if t := ctx.Filters[f.key]; t != "" {
		f.ed.buf = []rune(text.Sanitize(t))
		f.ed.pos = len(f.ed.buf)
	}
}

func (f *Filter) save(ctx *tui.Ctx) {
	if ctx.Filters == nil {
		ctx.Filters = map[string]string{}
	}
	if f.Active() {
		ctx.Filters[f.key] = string(f.ed.buf)
		return
	}
	delete(ctx.Filters, f.key)
}

// Editing reports whether the field takes keys.
func (f *Filter) Editing() bool { return f.editing }

// Active reports whether the text is non-empty.
func (f *Filter) Active() bool { return len(f.ed.buf) > 0 }

// Update handles a key. While not editing only "/" is taken (it starts
// editing). While editing every key is taken except up, down, page up and
// page down: those leave editing and are reported unhandled so the screen
// moves its list with the same key press.
func (f *Filter) Update(ctx *tui.Ctx, msg tea.KeyMsg) (handled bool, cmd tea.Cmd) {
	f.Load(ctx)
	if !f.editing {
		if msg.Type == tea.KeyRunes && string(msg.Runes) == "/" {
			f.editing = true
			f.ed.pos = len(f.ed.buf)
			return true, nil
		}
		return false, nil
	}
	switch msg.Type {
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		f.editing = false
		return false, nil
	case tea.KeyEnter:
		f.editing = false
		return true, nil
	case tea.KeyEsc, tea.KeyCtrlC:
		f.ed.wipe()
		f.editing = false
	default:
		f.ed.edit(msg, false)
		if !f.Active() {
			f.editing = false
		}
	}
	f.save(ctx)
	return true, nil
}

// Match reports whether a row's text contains the filter text, ignoring case.
// It is true while the filter is inactive.
func (f *Filter) Match(s string) bool {
	if !f.Active() {
		return true
	}
	return strings.Contains(strings.ToLower(text.Sanitize(s)), strings.ToLower(string(f.ed.buf)))
}

// View draws "/ <text>▏  <shown>/<total>", the caret only while editing, cut
// to width. It is empty when the filter is neither active nor editing.
func (f *Filter) View(width, shown, total int) string {
	if !f.Active() && !f.editing {
		return ""
	}
	caret := ""
	if f.editing {
		caret = "▏"
	}
	line := "/ " + string(f.ed.buf) + caret + "  " + strconv.Itoa(shown) + "/" + strconv.Itoa(total)
	return ansi.Truncate(line, width, "…")
}

package components

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
)

// SubmitMsg carries the entered value when the user confirms an input.
type SubmitMsg struct{ Value string }

// CancelMsg is emitted when the user backs out of an input or prompt.
type CancelMsg struct{}

// editor is the shared line-editing core: runes with a cursor.
type editor struct {
	buf []rune
	pos int
}

// wipe zeroes the buffer, so a secret does not linger in memory, and resets.
func (e *editor) wipe() {
	clear(e.buf)
	e.buf, e.pos = nil, 0
}

func (e *editor) removeAt(i int) {
	copy(e.buf[i:], e.buf[i+1:])
	e.buf[len(e.buf)-1] = 0
	e.buf = e.buf[:len(e.buf)-1]
}

// edit applies an editing key and reports whether it was one. raw keeps
// typed or pasted text as it is, its line breaks as "\n" (a masked input:
// never drawn, and a pasted PEM key or JSON must reach Railway whole);
// otherwise it is sanitized for display.
func (e *editor) edit(k tea.KeyMsg, raw bool) bool {
	switch k.Type {
	case tea.KeyRunes, tea.KeySpace:
		var rs []rune
		if raw {
			// terminals send a paste's line breaks as "\r"
			rs = []rune(strings.ReplaceAll(strings.ReplaceAll(string(k.Runes), "\r\n", "\n"), "\r", "\n"))
		} else {
			rs = []rune(text.Sanitize(string(k.Runes)))
		}
		if k.Type == tea.KeySpace {
			rs = []rune{' '}
		}
		nb := make([]rune, 0, len(e.buf)+len(rs))
		nb = append(append(append(nb, e.buf[:e.pos]...), rs...), e.buf[e.pos:]...)
		clear(e.buf)
		e.buf = nb
		e.pos += len(rs)
	case tea.KeyBackspace:
		if e.pos > 0 {
			e.removeAt(e.pos - 1)
			e.pos--
		}
	case tea.KeyDelete:
		if e.pos < len(e.buf) {
			e.removeAt(e.pos)
		}
	case tea.KeyLeft:
		e.pos = max(0, e.pos-1)
	case tea.KeyRight:
		e.pos = min(len(e.buf), e.pos+1)
	case tea.KeyHome, tea.KeyCtrlA:
		e.pos = 0
	case tea.KeyEnd, tea.KeyCtrlE:
		e.pos = len(e.buf)
	case tea.KeyCtrlU:
		for e.pos > 0 {
			e.removeAt(0)
			e.pos--
		}
	default:
		return false
	}
	return true
}

// TextInput is a single-line text field.
type TextInput struct {
	Label string
	ed    editor
}

// Value returns the current text.
func (t *TextInput) Value() string { return string(t.ed.buf) }

// SetValue replaces the text and puts the cursor at its end.
func (t *TextInput) SetValue(s string) {
	t.ed.buf = []rune(text.Sanitize(s))
	t.ed.pos = len(t.ed.buf)
}

// Update handles a key: enter emits SubmitMsg, esc and ctrl+c emit CancelMsg.
func (t *TextInput) Update(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch k.Type {
	case tea.KeyEnter:
		return tui.Emit(SubmitMsg{Value: t.Value()})
	case tea.KeyEsc, tea.KeyCtrlC:
		return tui.Emit(CancelMsg{})
	}
	t.ed.edit(k, false)
	return nil
}

// View draws the label and the text with a cursor, cut to width.
func (t *TextInput) View(width int) string {
	label := ""
	if t.Label != "" {
		label = text.Sanitize(t.Label) + ": "
	}
	room := max(1, width-ansi.StringWidth(label)-1)
	before, after := string(t.ed.buf[:t.ed.pos]), string(t.ed.buf[t.ed.pos:])
	// keep the cursor visible: drop the oldest text before it when too long
	if w := ansi.StringWidth(before); w > room-1 {
		before = ansi.TruncateLeft(before, w-(room-1), "")
	}
	line := label + before + "█" + after
	return ansi.Truncate(line, width, "…")
}

// MaskedInput is a secret field: it shows • per rune and the length, never the
// value. With Repeat the value must be typed twice and the entries must match.
type MaskedInput struct {
	Label  string
	Repeat bool

	ed    editor
	first []rune
	stage int // 0 first entry, 1 repeat entry
}

// Update handles a key. Enter moves from the first to the repeat entry (with
// Repeat) or emits SubmitMsg. A mismatch emits a FlashMsg and starts over.
func (m *MaskedInput) Update(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch k.Type {
	case tea.KeyEnter:
		if !m.Repeat {
			val := string(m.ed.buf)
			m.reset()
			return tui.Emit(SubmitMsg{Value: val})
		}
		if m.stage == 0 {
			m.first, m.stage = m.ed.buf, 1
			m.ed = editor{}
			return nil
		}
		same := string(m.first) == string(m.ed.buf)
		val := string(m.ed.buf)
		m.reset()
		if !same {
			return tui.Emit(tui.FlashMsg{Kind: '✗', Text: "the two entries differ, nothing set"})
		}
		return tui.Emit(SubmitMsg{Value: val})
	case tea.KeyEsc, tea.KeyCtrlC:
		m.reset()
		return tui.Emit(CancelMsg{})
	}
	m.ed.edit(k, true)
	return nil
}

// reset zeroes both entries and returns to the first one.
func (m *MaskedInput) reset() {
	clear(m.first)
	m.first, m.stage = nil, 0
	m.ed.wipe()
}

// View draws the label, • per rune and the length, cut to width.
func (m *MaskedInput) View(width int) string {
	label := text.Sanitize(m.Label)
	if label == "" {
		label = "value"
	}
	if m.Repeat && m.stage == 1 {
		label = "repeat " + label
	}
	n := len(m.ed.buf)
	line := label + ": " + strings.Repeat("•", n) + "█ (" + strconv.Itoa(n) + ")"
	return ansi.Truncate(line, width, "…")
}

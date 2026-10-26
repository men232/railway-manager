package components

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
)

// ConfirmedMsg is emitted when the user confirms a Confirm prompt.
type ConfirmedMsg struct{}

// Confirm asks for confirmation. With Word empty, y confirms and any other key
// cancels. With a Word, the user must type it and press enter; anything else
// cancels.
type Confirm struct {
	Prompt string
	Word   string

	ed editor
}

// Update handles a key and emits ConfirmedMsg or CancelMsg when decided.
func (c *Confirm) Update(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	if c.Word == "" {
		if k.Type == tea.KeyRunes && string(k.Runes) == "y" {
			return tui.Emit(ConfirmedMsg{})
		}
		return tui.Emit(CancelMsg{})
	}
	switch k.Type {
	case tea.KeyEnter:
		if string(c.ed.buf) == c.Word {
			return tui.Emit(ConfirmedMsg{})
		}
		return tui.Emit(CancelMsg{})
	case tea.KeyEsc, tea.KeyCtrlC:
		return tui.Emit(CancelMsg{})
	}
	c.ed.edit(k, false)
	return nil
}

// View draws the prompt and what is typed so far, cut to width.
func (c *Confirm) View(width int) string {
	var line string
	if c.Word == "" {
		line = text.Sanitize(c.Prompt) + " (y/n)"
	} else {
		line = text.Sanitize(c.Prompt) + " type \"" + text.Sanitize(c.Word) + "\" and press enter: " + string(c.ed.buf) + "█"
	}
	return ansi.Truncate(line, width, "…")
}

package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
)

// Emit is a command that yields m.
func Emit(m tea.Msg) tea.Cmd { return func() tea.Msg { return m } }

// Flash is a command that yields a footer flash of kind, its text sanitized.
func Flash(kind rune, t string) tea.Cmd {
	return Emit(FlashMsg{Kind: kind, Text: text.Sanitize(t)})
}

// Pad fits str to w cells: padded on the right, or cut with "…".
func Pad(str string, w int) string {
	if n := ansi.StringWidth(str); n < w {
		return str + strings.Repeat(" ", w-n)
	}
	return ansi.Truncate(str, w, "…")
}

// Clock is the context's clock: Now, or time.Now when unset.
func (c *Ctx) Clock() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

// EnvName is the environment's own name: its store label without the
// project ("project/env" gives "env"). The store must be set.
func (c *Ctx) EnvName(envID string) string {
	l := c.Store.EnvLabel(envID)
	if _, after, ok := strings.Cut(l, "/"); ok {
		return after
	}
	return l
}

// DriftText is how a drifted change's reason (view's Drifted) is shown on
// every screen: the Drift icon, "drifted: " and the reason's first line,
// sanitized; "" for a change that has not drifted.
func DriftText(icons Icons, reason string) string {
	if reason == "" {
		return ""
	}
	reason, _, _ = strings.Cut(strings.ReplaceAll(reason, "\r\n", "\n"), "\n")
	return icons.Drift() + " drifted: " + text.Sanitize(reason)
}

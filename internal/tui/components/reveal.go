package components

import (
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/tui"
)

// RevealLines is how many lines of a revealed value the panel shows; the
// rest is counted, not drawn.
const RevealLines = 6

// RevealPanel draws a revealed value as a framed panel width wide: title on
// the top border, the value wrapped inside, "v hides" on the right. value
// must already be sanitized. It returns nil when width is too narrow.
func RevealPanel(th tui.Theme, title, value string, width int) []string {
	inner := width - 4
	if inner < 8 {
		return nil
	}
	var raw []string
	for _, l := range strings.Split(value, "\n") {
		raw = append(raw, strings.Split(ansi.Hardwrap(l, inner, true), "\n")...)
	}
	more := 0
	if len(raw) > RevealLines {
		for _, l := range raw[RevealLines-1:] {
			more += ansi.StringWidth(l)
		}
		raw = raw[:RevealLines-1]
	}
	lines := make([]string, 0, RevealLines)
	for _, l := range raw {
		lines = append(lines, th.Accent.Render(l))
	}
	if more > 0 {
		lines = append(lines, th.Muted.Render("… "+strconv.Itoa(more)+" more chars"))
	}

	b := th.Accent2
	hint := " v hides "
	head := "─ " + ansi.Truncate(title, max(0, width-6-len(hint)), "…") + " "
	fill := max(0, width-2-ansi.StringWidth(head)-len(hint)-1)
	out := []string{b.Render("╭" + head + strings.Repeat("─", fill) + hint + "─╮")}
	for _, l := range lines {
		out = append(out, b.Render("│ ")+tui.Pad(l, inner)+b.Render(" │"))
	}
	return append(out, b.Render("╰"+strings.Repeat("─", width-2)+"╯"))
}

// Overlay lays panel over the bottom of lines without moving them, or over
// the top when it would hide row cursor. lines keeps its length; a panel
// taller than lines is cut.
func Overlay(lines, panel []string, cursor int) []string {
	if len(panel) == 0 || len(lines) == 0 {
		return lines
	}
	panel = panel[:min(len(panel), len(lines))]
	out := slices.Clone(lines)
	at := len(out) - len(panel)
	if cursor >= at {
		at = 0
	}
	copy(out[at:], panel)
	return out
}

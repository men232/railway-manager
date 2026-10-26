package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
)

const (
	// ChromeRows is what the frame takes from the height: top border, header,
	// three footer lines, bottom border.
	ChromeRows = 6
	// ChromeCols is what the frame takes from the width: border and one column
	// of padding on each side.
	ChromeCols = 4
)

const badge = "railway-manager"

// Frame draws the whole screen: thick border, header, body padded to fill the
// height, and a four-line footer. Every line is cut to ctx.Width and the
// output has exactly ctx.Height lines. Title, note, account and flash text are
// sanitized here; the body is the screen's own responsibility.
func Frame(ctx *Ctx, title, note, body string, footer Footer) string {
	w, h := ctx.Width, ctx.Height
	if w < 1 || h < 1 {
		return ""
	}
	inner := w - ChromeCols
	bodyH := h - ChromeRows
	if inner < 10 || bodyH < 1 {
		return tooSmall(ctx, w, h)
	}
	th := ctx.Theme
	b := lipgloss.ThickBorder()
	edge := func(s string) string { return th.Border.Render(s) }

	row := func(content string) string {
		content = ansi.Truncate(content, inner, "…")
		pad := inner - ansi.StringWidth(content)
		return edge(b.Left) + " " + content + strings.Repeat(" ", pad) + " " + edge(b.Right)
	}

	out := make([]string, 0, h)
	out = append(out, edge(b.TopLeft+strings.Repeat(b.Top, w-2)+b.TopRight))

	left := th.Accent.Render(badge)
	if t := text.Sanitize(title); t != "" {
		left += " " + th.Muted.Render(t)
	}
	out = append(out, row(joinLR(left, th.Muted.Render(text.Sanitize(note)), inner)))

	bl := strings.Split(strings.ReplaceAll(body, "\t", " "), "\n")
	for i := 0; i < bodyH; i++ {
		if i < len(bl) {
			out = append(out, row(bl[i]))
		} else {
			out = append(out, row(""))
		}
	}

	out = append(out,
		row(th.Faint.Render(strings.Repeat("─", inner))),
		row(messageLine(ctx, footer, inner)),
		row(hintLine(ctx, footer.Hints, inner)),
	)
	out = append(out, edge(b.BottomLeft+strings.Repeat(b.Bottom, w-2)+b.BottomRight))
	return strings.Join(out, "\n")
}

// joinLR puts right at the right edge of a width-wide line. The left side wins
// when they collide; the right is cut to what remains, or dropped.
func joinLR(left, right string, width int) string {
	lw := ansi.StringWidth(left)
	if lw >= width {
		return ansi.Truncate(left, width, "…")
	}
	room := width - lw - 1
	if room <= 0 || ansi.StringWidth(right) == 0 {
		return left
	}
	right = ansi.Truncate(right, room, "…")
	return left + strings.Repeat(" ", width-lw-ansi.StringWidth(right)) + right
}

// tooSmall is the frame for a terminal too small to draw the chrome.
func tooSmall(ctx *Ctx, w, h int) string {
	lines := make([]string, h)
	lines[0] = ansi.Truncate("terminal too small", w, "")
	return strings.Join(lines, "\n")
}

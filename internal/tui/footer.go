package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
)

// Footer is what a screen contributes to the frame footer.
type Footer struct {
	// Flash is the message line text; the zero value shows nothing.
	Flash FlashMsg
	Hints []KeyHint
}

// Indicator is the change-set summary, empty for an empty set.
func Indicator(ctx *Ctx) string {
	if ctx.CS == nil || len(ctx.CS.Changes) == 0 {
		return ""
	}
	places := map[string]struct{}{}
	envs := map[string]struct{}{}
	for _, c := range ctx.CS.Changes {
		places[c.Place.Key()] = struct{}{}
		envs[c.Place.EnvID] = struct{}{}
	}
	sep := ctx.Icons.Sep()
	return fmt.Sprintf("%s %s%s%s%s%s", ctx.Icons.Pending(),
		Plural(len(ctx.CS.Changes), "change"), sep,
		Plural(len(places), "service"), sep,
		Plural(len(envs), "env"))
}

// Plural is "1 word" or "n words".
func Plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// messageLine is the flash on the left and the change-set indicator on the
// right, cut to width.
func messageLine(ctx *Ctx, f Footer, width int) string {
	right := Indicator(ctx)
	rw := ansi.StringWidth(right)
	left, lw := "", 0
	if f.Flash.Text != "" {
		msg := text.Sanitize(f.Flash.Text)
		st, plain := ctx.Theme.Muted, msg
		switch f.Flash.Kind {
		case '✓':
			st, plain = ctx.Theme.OK, "✓ "+msg
		case '✗':
			st, plain = ctx.Theme.Danger, "✗ "+msg
		case '!':
			st, plain = ctx.Theme.Warn, "! "+msg
		}
		room := width
		if rw > 0 {
			room = width - rw - 1
		}
		if room < 0 {
			room = 0
		}
		plain = ansi.Truncate(plain, room, "…")
		left, lw = st.Render(plain), ansi.StringWidth(plain)
	}
	if rw > width {
		return ctx.Theme.New.Render(ansi.Truncate(right, width, "…"))
	}
	pad := width - lw - rw
	if pad < 0 {
		pad = 0
	}
	return left + strings.Repeat(" ", pad) + ctx.Theme.New.Render(right)
}

// hintLine renders always-hints first, then the others by rank while they fit,
// and "? more" when any were left out.
func hintLine(ctx *Ctx, hints []KeyHint, width int) string {
	var always, rest, optional []KeyHint
	for _, h := range hints {
		if h.Optional {
			optional = append(optional, h)
		} else if h.Always {
			always = append(always, h)
		} else {
			rest = append(rest, h)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Rank < rest[j].Rank })
	sort.SliceStable(optional, func(i, j int) bool { return optional[i].Rank < optional[j].Rank })

	const gap = "  "
	more := "? more"
	used := 0
	var parts []string
	add := func(h KeyHint) {
		plain := h.Key + " " + h.Label
		used += ansi.StringWidth(plain)
		if len(parts) > 0 {
			used += len(gap)
		}
		parts = append(parts, ctx.Theme.Accent.Render(h.Key)+" "+ctx.Theme.Muted.Render(h.Label))
	}
	for _, h := range always {
		add(h)
	}
	dropped := false
	total := used
	for _, h := range rest {
		total += ansi.StringWidth(h.Key+" "+h.Label) + len(gap)
	}
	allFit := total <= width
	for _, h := range rest {
		need := ansi.StringWidth(h.Key+" "+h.Label) + len(gap)
		reserve := len(gap) + len(more)
		if allFit {
			reserve = 0
		}
		if used+need+reserve > width {
			dropped = true
			break
		}
		add(h)
	}
	// Optional hints use only the room left once everything else, including
	// "? more", is placed. Leaving one out never adds "? more".
	for _, h := range optional {
		need := ansi.StringWidth(h.Key+" "+h.Label) + len(gap)
		reserve := 0
		if dropped {
			reserve = len(gap) + len(more)
		}
		if used+need+reserve > width {
			continue
		}
		add(h)
	}
	line := strings.Join(parts, gap)
	if dropped {
		if len(parts) > 0 {
			line += gap
		}
		line += ctx.Theme.Faint.Render(more)
	}
	return line
}

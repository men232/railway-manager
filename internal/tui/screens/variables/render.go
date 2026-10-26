package variables

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

// seg is a run of text in one style (nil: plain).
type seg struct {
	text  string
	style *lipgloss.Style
}

// col is one column of a line. rank 0 is never dropped; when a line is too
// wide the highest rank goes first, a flex column is cut instead while at
// least minFlex of it is left. Once every droppable column is gone, a flex
// column of rank 0 (a revealed value) is cut, then the first column (the
// head); the last (the marks) is never cut before them.
type col struct {
	segs []seg
	rank int
	flex bool
}

const minFlex = 8

func (c col) width() int {
	w := 0
	for _, s := range c.segs {
		w += ansi.StringWidth(s.text)
	}
	return w
}

// cut fits the column to w cells, ending in "…".
func (c col) cut(w int) col {
	var out []seg
	left := w
	total := c.width()
	if total <= w {
		return c
	}
	for _, s := range c.segs {
		sw := ansi.StringWidth(s.text)
		if sw < left {
			out = append(out, s)
			left -= sw
			continue
		}
		out = append(out, seg{ansi.Truncate(s.text, max(left, 0), "…"), s.style})
		break
	}
	return col{segs: out, rank: c.rank, flex: c.flex}
}

// fit drops and cuts columns until the line fits width (see col). The marks
// stay whole as long as the head can keep minFlex cells.
func fit(cols []col, width int) []col {
	total := func() int {
		w := 0
		for _, c := range cols {
			w += c.width()
		}
		return w
	}
	for total() > width {
		i := -1
		for j, c := range cols {
			if c.rank > 0 && (i < 0 || c.rank > cols[i].rank) {
				i = j
			}
		}
		if i < 0 {
			break
		}
		over := total() - width
		if cols[i].flex && cols[i].width()-over >= minFlex {
			cols[i] = cols[i].cut(cols[i].width() - over)
			break
		}
		cols = append(cols[:i:i], cols[i+1:]...)
	}
	// a flex column that is never dropped (a revealed value) is cut next,
	// then the head, so the marks stay
	for i := range cols {
		if over := total() - width; over > 0 && i > 0 && cols[i].rank == 0 && cols[i].flex {
			cols[i] = cols[i].cut(max(cols[i].width()-over, min(3, cols[i].width())))
		}
	}
	if over := total() - width; over > 0 {
		cols[0] = cols[0].cut(max(cols[0].width()-over, min(minFlex, width)))
	}
	return cols
}

const maxRank = 6

// upTo keeps the columns of rank at most k.
func upTo(cols []col, k int) []col {
	out := make([]col, 0, len(cols))
	for _, c := range cols {
		if c.rank <= k {
			out = append(out, c)
		}
	}
	return out
}

// shared is the highest rank the lines of a kind (group lines, or every
// other) all keep, so their columns stay aligned: the most that lets each
// fit, a flex column counted at minFlex. A line that cannot fit even with
// rank 0 alone is left out; fit cuts it on its own.
func shared(rows [][]col, ls []line, kind lineKind, width int) int {
	need := func(cols []col, k int) int {
		w := 0
		for _, c := range cols {
			switch {
			case c.rank > k:
			case c.flex:
				w += min(c.width(), minFlex)
			default:
				w += c.width()
			}
		}
		return w
	}
	k := maxRank
	for i, cols := range rows {
		if (ls[i].kind == lineGroup) != (kind == lineGroup) || need(cols, 0) > width {
			continue
		}
		for k > 0 && need(cols, k) > width {
			k--
		}
	}
	return k
}

// render draws the columns: plain in the selection style when selected,
// cut to width either way.
func render(th tui.Theme, cols []col, selected bool, width int) string {
	var b strings.Builder
	for _, c := range cols {
		for _, s := range c.segs {
			if selected || s.style == nil {
				b.WriteString(s.text)
			} else {
				b.WriteString(s.style.Render(s.text))
			}
		}
	}
	line := ansi.Truncate(b.String(), width, "…")
	if selected {
		return th.Selected.Render(line)
	}
	return line
}

// changed formats a variable's last change as "changed MM-DD HH:MM"; "" when
// unknown.
func changed(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return "changed " + t.UTC().Format("01-02 15:04")
}

const changedW = len("changed 01-02 15:04")

// by is a place's author as shown: "?" when there is none, or when its
// environment's history read failed (nothing is on its way).
func (s *screen) by(r *view.PlaceRow) string {
	if r.By == "" || r.By == view.ByUnread && s.errs["h:"+r.Place.EnvID] != "" {
		return view.ByNone
	}
	return text.Sanitize(r.By)
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	if ctx.Store == nil {
		return ""
	}
	th := ctx.Theme
	// A change put through the root reaches no screen: a new tree hides a
	// revealed value here too.
	if gs := s.tree(ctx); s.revealed != "" && (len(gs) == 0 || &gs[0] != s.revealedTree) {
		s.hide()
	}
	shown, ls, total := s.lines(ctx)
	idx := s.locate(ls)
	var lines []string
	if s.scope != nil {
		header := th.Accent.Render(text.Sanitize(ctx.EnvName(s.scope.EnvID) + " · " + ctx.Store.ServiceName(s.scope.ServiceID)))
		lines = append(lines, ansi.Truncate(header, width, "…"))
	}
	if f := s.filter.View(width, shown, total); f != "" {
		lines = append(lines, f)
	}
	var tail string
	switch s.mode {
	case modeEdit, modeAddValue:
		tail = s.mask.View(width)
	case modeAddName:
		tail = s.text.View(width)
	case modeConfirm:
		tail = th.Warn.Render(s.confirm.View(width))
	}
	listH := s.listHeight(ctx, height)

	switch {
	case s.mode == modePick:
		lines = append(lines, s.pickView(ctx, width, listH)...)
	case len(ls) == 0:
		msg := "no variables in the selected environments"
		switch {
		case s.filter.Active():
			msg = "no variable matches the filter"
		case s.scope != nil:
			msg = "no variables here · a adds one"
		}
		lines = append(lines, th.Muted.Render(msg))
	default:
		from, to := s.list.Window(len(ls), listH)
		lay := s.layout(ctx, ls[from:to], width)
		rows := make([][]col, 0, to-from)
		for i := from; i < to; i++ {
			rows = append(rows, s.cols(ctx, lay, ls[i], i == idx))
		}
		keep := [2]int{shared(rows, ls[from:to], lineGroup, width), shared(rows, ls[from:to], linePlace, width)}
		list := make([]string, 0, listH)
		for i, cols := range rows {
			k := keep[1]
			if ls[from+i].kind == lineGroup {
				k = keep[0]
			}
			list = append(list, render(th, fit(upTo(cols, k), width), from+i == idx, width))
		}
		if panel := s.revealPanel(ctx, ls, idx, width); panel != nil {
			for len(list) < listH {
				list = append(list, "")
			}
			list = components.Overlay(list, panel, idx-from)
		}
		lines = append(lines, list...)
	}
	if tail != "" {
		lines = append(lines, tail)
	}
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

// revealPanel is the floating panel of the cursor line's revealed value;
// nil when none is revealed.
func (s *screen) revealPanel(ctx *tui.Ctx, ls []line, idx, width int) []string {
	if s.revealed == "" || idx < 0 || idx >= len(ls) || ls[idx].p == nil || ls[idx].p.LocationID != s.revealed {
		return nil
	}
	p := ls[idx].p
	v := ctx.Store.Variable(p.LocationID)
	if v == nil || v.Sealed {
		return nil
	}
	title := text.Sanitize(p.Name) + " · " + locText(shortEnvs(ctx), p) + " · " + strconv.Itoa(len([]rune(v.Rendered))) + " chars"
	return components.RevealPanel(ctx.Theme, title, text.Sanitize(v.Rendered), width)
}

func (s *screen) pickView(ctx *tui.Ctx, width, height int) []string {
	th := ctx.Theme
	out := []string{ansi.Truncate(th.Accent.Render(text.Sanitize("add "+s.name+" to which place?")), width, "…")}
	from, to := s.pick.Window(len(s.cands), height)
	for i := from; i < to; i++ {
		l := ansi.Truncate("  "+text.Sanitize(placeText(ctx, s.cands[i])), width, "…")
		if i == s.pick.Cursor {
			l = th.Selected.Render(l)
		}
		out = append(out, l)
	}
	return out
}

// layout is what the shown lines share: column widths and whether
// environments drop their project.
type layout struct {
	short       bool
	nameW, locW int
	byW         int
	// varW is the name column of place lines in groups of several names.
	varW int
}

func (s *screen) layout(ctx *tui.Ctx, ls []line, width int) layout {
	lay := layout{short: shortEnvs(ctx), nameW: 4}
	for _, l := range ls {
		label := l.g.Label
		if s.scope != nil {
			label = l.p.Name
		}
		if l.kind != linePlace {
			lay.nameW = max(lay.nameW, ansi.StringWidth(text.Sanitize(label)))
		}
		if l.p != nil {
			lay.locW = max(lay.locW, ansi.StringWidth(locText(lay.short, l.p)))
			lay.byW = max(lay.byW, ansi.StringWidth(s.by(l.p)))
			if l.kind == linePlace && len(l.g.Names) > 1 {
				lay.varW = max(lay.varW, ansi.StringWidth(text.Sanitize(l.p.Name)))
			}
		}
	}
	lay.nameW = min(lay.nameW, max(8, width/3))
	lay.locW = min(lay.locW, max(8, width/3))
	lay.byW = min(lay.byW, 16)
	lay.varW = min(lay.varW, max(8, width/4))
	return lay
}

// locText is a place's "env · service" as shown, sanitized.
func locText(short bool, p *view.PlaceRow) string {
	return text.Sanitize(envText(short, p.Env) + " · " + p.Service)
}

// cols is a line's columns, in order: head, the optional columns, marks.
func (s *screen) cols(ctx *tui.Ctx, lay layout, l line, selected bool) []col {
	th := ctx.Theme
	st := func(x lipgloss.Style) *lipgloss.Style { return &x }
	if l.kind == lineGroup {
		g := l.g
		arrow := "▸ "
		if s.unfolded[g.Key] {
			arrow = "▾ "
		}
		values := strconv.Itoa(g.Values) + " values"
		if g.Values == 1 {
			values = "1 value"
		}
		vstyle := st(th.Muted)
		if g.Values > 1 {
			vstyle = st(th.Warn)
		}
		envs := make([]string, len(g.Envs))
		for i, e := range g.Envs {
			envs[i] = text.Sanitize(envText(lay.short, e))
		}
		cols := []col{
			{segs: []seg{{arrow + tui.Pad(text.Sanitize(g.Label), lay.nameW), nil}}},
			{segs: []seg{{"  " + tui.Pad("×"+strconv.Itoa(len(g.Places)), 4), st(th.Muted)}}, rank: 3},
			{segs: []seg{{"  ", nil}, {tui.Pad(values, 8), vstyle}}, rank: 2},
			{segs: []seg{{"  " + strings.Join(envs, ", "), st(th.Muted)}}, rank: 6, flex: true},
		}
		if g.Pending > 0 {
			cols = append(cols, col{segs: []seg{{"  " + ctx.Icons.Pending() + " " + strconv.Itoa(g.Pending), st(th.New)}}})
		}
		return cols
	}

	p := l.p
	var head col
	switch {
	case s.scope != nil:
		val := "••••••• (" + strconv.Itoa(p.Length) + ")"
		switch {
		case p.Sealed:
			val = "sealed"
		case p.Pending != nil && p.Pending.Base.Absent && p.Pending.Kind == "variable.set":
			val = "" // an added variable: the value is only in the change
		}
		if ansi.StringWidth(val) < 14 {
			val = tui.Pad(val, 14)
		}
		head = col{segs: []seg{{tui.Pad(text.Sanitize(p.Name), lay.nameW) + "  ", nil}, {val, st(th.Muted)}}}
	case l.kind == lineSingle:
		head = col{segs: []seg{{"  " + tui.Pad(text.Sanitize(p.Name), lay.nameW) + "  " + tui.Pad(locText(lay.short, p), lay.locW), nil}}}
	default:
		// the letter first, as on every screen that shows one
		letter := seg{" ", nil}
		if p.Letter >= 0 {
			letter = seg{string(rune('a' + p.Letter%26)), st(th.Letters[p.Letter%len(th.Letters)])}
		}
		lead := "  " + tui.Pad(locText(lay.short, p), lay.locW)
		if len(l.g.Names) > 1 { // which variable of the group this is
			lead += "  " + tui.Pad(text.Sanitize(p.Name), lay.varW)
		}
		head = col{segs: []seg{{"    ", nil}, letter, {lead, nil}}}
	}
	cols := []col{head}
	cols = append(cols,
		// who goes before when on a narrow screen
		col{segs: []seg{{"  " + tui.Pad(changed(p.UpdatedAt), changedW), st(th.Muted)}}, rank: 5},
		col{segs: []seg{{"  by " + tui.Pad(s.by(p), lay.byW), st(th.Muted)}}, rank: 6},
	)
	if p.Sealed && s.scope == nil {
		cols = append(cols, col{segs: []seg{{"  sealed", st(th.Muted)}}, rank: 2})
	}
	if p.Ref != "" {
		cols = append(cols, col{segs: []seg{{"  → " + text.Sanitize(p.Ref), st(th.Muted)}}, rank: 4, flex: true})
	}
	if m := marks(ctx, p); len(m) > 0 {
		cols = append(cols, col{segs: m})
	}
	return cols
}

// marks is what the change set does to a place: the overlay (or the run
// holding it, or that it is gone from Railway), then why it is invalid or
// drifted. Empty without a change.
func marks(ctx *tui.Ctx, p *view.PlaceRow) []seg {
	th := ctx.Theme
	st := func(x lipgloss.Style) *lipgloss.Style { return &x }
	var out []seg
	switch {
	case p.Pending != nil && p.Pending.RunID != "":
		out = append(out, seg{"  " + ctx.Icons.Run() + " " + text.Sanitize(p.Pending.RunID), st(th.Warn)})
	case p.Missing:
		out = append(out, seg{"  " + strings.TrimSpace("! gone from Railway "+text.Sanitize(p.Overlay.Short)), st(th.Warn)})
	case p.Pending != nil:
		tone := th.New
		switch p.Overlay.Tone {
		case "danger":
			tone = th.Danger
		case "warn":
			tone = th.Warn
		}
		out = append(out, seg{"  " + text.Sanitize(p.Overlay.Short), st(tone)})
	}
	switch {
	case p.Invalid != "":
		reason, _, _ := strings.Cut(strings.ReplaceAll(p.Invalid, "\r\n", "\n"), "\n")
		out = append(out, seg{"  " + ctx.Icons.Fail() + " invalid: " + text.Sanitize(reason), st(th.Danger)})
	case p.Drifted != "" && !p.Missing:
		// A drifted change is marked, never dropped: it still applies.
		// A place gone from Railway already says so.
		out = append(out, seg{"  " + tui.DriftText(ctx.Icons, p.Drifted), st(th.Warn)})
	}
	return out
}

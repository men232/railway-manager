package rotate

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/view"
)

func sectionTitle(sec rotation.Section) string {
	switch sec {
	case rotation.SectionGenerated:
		return "generated"
	case rotation.SectionPaste:
		return "values you paste"
	case rotation.SectionAdded:
		return "added"
	default:
		return "database"
	}
}

// tag is a row's icon and mode tag, in the tag's colour.
func tag(ctx *tui.Ctx, r view.RotateRow) string {
	text, style := tagOf(ctx, r)
	return style.Render(text)
}

// tagOf is a row's icon and mode tag and its style.
func tagOf(ctx *tui.Ctx, r view.RotateRow) (string, lipgloss.Style) {
	tg := ctx.Theme.Tags
	switch {
	case r.Section == rotation.SectionAdded:
		return ctx.Icons.Added() + " NEW", tg.New
	case pwRow(r):
		return pwTag(r), pwTagStyle(ctx, r)
	case r.Class == rotation.ClassGen:
		return "◆ GEN", tg.Gen
	default:
		return ctx.Icons.Edit() + " ASK", tg.Ask
	}
}

// state is a row's state column and its style.
func state(ctx *tui.Ctx, r view.RotateRow) (string, lipgloss.Style) {
	if pwRow(r) {
		return pwState(ctx, r)
	}
	th := ctx.Theme
	// Lines of a group rotated after the leak need nothing: they count as
	// done, so a row whose other lines are decided is complete.
	left := openCount(r)
	frac := strconv.Itoa(r.Decidable-left) + "/" + strconv.Itoa(r.Decidable)
	switch {
	case r.Problem != "":
		return ctx.Icons.Fail() + " cannot rotate", th.Danger
	case r.Decided == 0 && rotated(r):
		return ctx.Icons.Ok() + " rotated", th.OK
	case r.Decided == 0:
		return "", th.Muted
	case left == 0:
		return ctx.Icons.Ok() + " " + frac, th.OK
	default:
		return ctx.Icons.Status("◐") + " " + frac, th.Warn
	}
}

// rotated reports whether every place of the row changed after the leak.
func rotated(r view.RotateRow) bool {
	if len(r.Groups) == 0 {
		return false
	}
	for _, g := range r.Groups {
		if !g.AllFresh {
			return false
		}
	}
	return true
}

// shortEnvs reports whether environments are shown without their project:
// one project is selected.
func shortEnvs(ctx *tui.Ctx) bool {
	return len(loaders.Targets(ctx.Store.Projects(), ctx.Settings)) <= 1
}

// envText is an environment label as shown, sanitized.
func envText(short bool, label string) string {
	if short {
		if _, after, ok := strings.Cut(label, "/"); ok {
			label = after
		}
	}
	return text.Sanitize(label)
}

// line is one list line: a section header (row nil) or a row.
type line struct {
	sec   rotation.Section
	count int
	row   int // index in the shown rows; -1 for a header
}

func listLines(rows []view.RotateRow) []line {
	var out []line
	for i, r := range rows {
		if i == 0 || rows[i-1].Section != r.Section {
			out = append(out, line{sec: r.Section, row: -1})
		}
		out[headerOf(out, len(out)-1)].count++
		out = append(out, line{sec: r.Section, row: i})
	}
	return out
}

// headerOf is the index of the header of the section line i is in.
func headerOf(ls []line, i int) int {
	for ; i > 0 && ls[i].row >= 0; i-- {
	}
	return i
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	if ctx.Store == nil || ctx.Views == nil {
		return ""
	}
	if s.add != nil {
		return s.addView(ctx, width, height)
	}
	th := ctx.Theme
	vis := s.visible(ctx)
	rows := s.shown(ctx)
	var top, tail []string
	if partialRead(ctx) {
		top = append(top, th.Warn.Render(ansi.Truncate("! the last read missed some places: a secret there is not counted · r refreshes", width, "…")))
	}
	if f := s.filter.View(width, len(rows), len(vis)); f != "" {
		top = append(top, f)
	}
	if s.confirm != nil {
		tail = append(tail, th.Warn.Render(s.confirm.View(width)))
	}
	room := height - len(top) - len(tail)
	out := top
	s.seen(rows)
	idx := s.locate(rows)
	s.stalled = idx >= 0 && pwRow(rows[idx]) && rows[idx].Stalled
	s.revertable = s.stalled && rows[idx].Revert
	if idx < 0 {
		msg := "no secrets in the selected environments"
		switch {
		case s.filter.Active():
			msg = "no row matches the filter"
		case !s.hidden && len(s.rows(ctx)) > 0:
			msg = "nothing to rotate · h shows " + strconv.Itoa(len(s.rows(ctx))) + " hidden rows"
		}
		out = append(out, th.Muted.Render(msg))
	} else {
		var panel []string
		if pb := room/3 - 1; pb > 0 {
			panel = s.panel(ctx, rows[idx], width, pb)
		}
		listH := room
		if len(panel) > 0 {
			listH -= len(panel) + 1
		}
		list := s.list(ctx, rows, idx, width, listH)
		for len(list) < listH {
			list = append(list, "")
		}
		out = append(out, list...)
		if len(panel) > 0 {
			out = append(out, th.Faint.Render(strings.Repeat("─", width)))
			out = append(out, panel...)
		}
	}
	out = append(out, tail...)
	if len(out) > height {
		out = out[:max(height, 0)]
	}
	return strings.Join(out, "\n")
}

// partialRead reports whether the last read of a selected project failed
// for some places (a load warning): their secrets are missing from the rows,
// so a group can look rotated everywhere while one of them keeps the value.
func partialRead(ctx *tui.Ctx) bool {
	if ctx.Settings == nil {
		return false
	}
	for _, p := range ctx.Settings.Projects {
		if strings.HasPrefix(ctx.Store.Load(store.InventoryKey(p)).Err, "! ") {
			return true
		}
	}
	return false
}

// list draws the rows by section in height lines, the cursor's row and its
// section header on screen.
func (s *screen) list(ctx *tui.Ctx, rows []view.RotateRow, idx, width, height int) []string {
	if height < 1 {
		return nil
	}
	th := ctx.Theme
	ls := listLines(rows)
	cl := 0
	for i, l := range ls {
		if l.row == idx {
			cl = i
		}
	}
	hl := headerOf(ls, cl)
	if cl < s.offset {
		s.offset = cl
	}
	if cl >= s.offset+height {
		s.offset = cl - height + 1
	}
	if s.offset > hl && cl == s.offset && height > 1 {
		s.offset-- // room for the pinned header
	}
	s.offset = max(0, min(s.offset, max(0, len(ls)-height)))

	short := shortEnvs(ctx)
	stateW, labelW, countW := 0, 4, 2
	for _, r := range rows {
		st, _ := state(ctx, r)
		stateW = max(stateW, ansi.StringWidth(st))
		labelW = max(labelW, ansi.StringWidth(text.Sanitize(r.Label)))
		countW = max(countW, len(count(r)))
	}
	labelW = min(labelW, max(10, width/3))

	var out []string
	for i := s.offset; i < min(len(ls), s.offset+height); i++ {
		l := ls[i]
		if i == s.offset && s.offset > hl && height > 1 {
			l = ls[hl]
		}
		if l.row < 0 {
			h := "── " + sectionTitle(l.sec) + " (" + strconv.Itoa(l.count) + ") "
			if n := width - ansi.StringWidth(h); n > 0 {
				h += strings.Repeat("─", n)
			}
			out = append(out, th.Faint.Render(ansi.Truncate(h, width, "")))
			continue
		}
		out = append(out, s.rowLine(ctx, rows[l.row], l.row == idx, short, stateW, labelW, countW, width))
	}
	return out
}

func (s *screen) rowLine(ctx *tui.Ctx, r view.RotateRow, selected, short bool, stateW, labelW, countW, width int) string {
	th := ctx.Theme
	type seg struct {
		text  string
		style *lipgloss.Style
	}
	st := func(x lipgloss.Style) *lipgloss.Style { return &x }
	var segs []seg
	if stateW > 0 {
		txt, style := state(ctx, r)
		segs = append(segs, seg{tui.Pad(txt, stateW), st(style)}, seg{"  ", nil})
	}
	tagText, tagStyle := tagOf(ctx, r)
	mark := " "
	var markStyle *lipgloss.Style
	switch {
	case r.Problem != "" || r.Stalled || r.Blocked != "":
		mark, markStyle = "!", st(th.Danger)
	case r.Warning != "":
		mark, markStyle = "!", st(th.Warn)
	}
	segs = append(segs,
		seg{tagText, st(tagStyle)},
		seg{"  " + tui.Pad(text.Sanitize(r.Label), labelW) + " ", nil},
		seg{mark, markStyle},
		seg{" " + tui.Pad(count(r), countW), nil},
	)
	if pwRow(r) && !r.Password.KeyFile {
		n := countURLs(r).total
		urls := strconv.Itoa(n) + " URLs"
		if n == 1 {
			urls = "1 URL"
		}
		segs = append(segs, seg{"  " + urls, nil})
	}
	if len(r.Groups) > 1 {
		segs = append(segs, seg{"  " + strconv.Itoa(len(r.Groups)) + " values", st(th.Warn)})
	}
	envs := make([]string, len(r.Envs))
	for i, e := range r.Envs {
		envs[i] = envText(short, e)
	}
	segs = append(segs, seg{"  " + strings.Join(envs, ", "), st(th.Muted)})

	var b strings.Builder
	dim := r.Hidden
	for _, sg := range segs {
		switch {
		case selected || (sg.style == nil && !dim):
			b.WriteString(sg.text)
		case dim:
			b.WriteString(th.Muted.Render(sg.text))
		default:
			b.WriteString(sg.style.Render(sg.text))
		}
	}
	l := ansi.Truncate(b.String(), width, "…")
	if selected {
		return th.Selected.Render(l)
	}
	return l
}

// count is a row's place count column: "×N"; a password row counts its
// own variables only (none for a user row).
func count(r view.RotateRow) string {
	n := len(r.Places)
	if pwRow(r) {
		if n = ownCount(r); n == 0 {
			return ""
		}
	}
	return "×" + strconv.Itoa(n)
}

// action is the panel's first line after the tag and label (value rotation
// wording).
func action(ctx *tui.Ctx, r view.RotateRow) string {
	switch {
	case r.Section == rotation.SectionAdded:
		return "added variable · enter: its places"
	case pwRow(r):
		return pwAction(ctx, r)
	case r.Class == rotation.ClassGen:
		for _, g := range r.Groups {
			if g.Sealed {
				continue
			}
			kind := "letters+digits"
			if g.Hex {
				kind = "hex"
			}
			return strconv.Itoa(max(g.OldLen, 24)) + " " + kind + ", like now"
		}
		return "24 letters+digits (sealed: length unknown)"
	case r.Public:
		return "public value, usually not a secret"
	case r.Class == rotation.ClassHidden:
		return "not recognised as a secret · enter: decide per place"
	default:
		return "enter: paste, file or copy per place · issue new keys first, revoke old ones after apply"
	}
}

// panel is the detail panel of row r in at most max lines: action, stats,
// problem, warning, note, coverage, the first places, references.
func (s *screen) panel(ctx *tui.Ctx, r view.RotateRow, width, limit int) []string {
	if pwRow(r) {
		return s.pwPanel(ctx, r, width, limit)
	}
	th := ctx.Theme
	cut := func(l string) string { return ansi.Truncate(l, width, "…") }
	var head []string
	head = append(head, cut(tag(ctx, r)+"  "+text.Sanitize(r.Label)+" · "+action(ctx, r)))
	head = append(head, cut(th.Muted.Render(stats(ctx, r))))
	if r.Problem != "" {
		head = append(head, cut(th.Danger.Render(ctx.Icons.Fail()+" "+text.Sanitize(r.Problem))))
	}
	if r.Warning != "" {
		head = append(head, cut(th.Warn.Render(ctx.Icons.Warn()+" "+text.Sanitize(r.Warning))))
	}
	if r.Note != "" {
		head = append(head, cut(th.Muted.Render(text.Sanitize(r.Note))))
	}
	for _, c := range coverage(ctx, r) {
		head = append(head, cut(th.Warn.Render(c)))
	}
	if len(head) >= limit {
		return head[:limit]
	}

	var refs []string
	if len(r.Refs) > 0 {
		refs = append(refs, cut(th.Muted.Render("↳ through references: "+strconv.Itoa(len(r.Refs))+" · redeployed with the new value")))
		for _, rc := range r.Refs {
			l := "    " + text.Sanitize(rc.Label)
			if rc.DB {
				l += " (DB: no restart)"
			}
			refs = append(refs, cut(th.Muted.Render(l)))
		}
	}
	left := limit - len(head)
	refRoom := min(len(refs), 1) // the references line is kept
	placeRoom := min(len(r.Places), left-refRoom)
	places := s.places(ctx, r, width, placeRoom)
	out := append(head, places...)
	return append(out, refs[:min(len(refs), limit-len(out))]...)
}

// stats is "places N  values K (a b)  names N  rotated F/N (after …)".
func stats(ctx *tui.Ctx, r view.RotateRow) string {
	parts := []string{"places " + strconv.Itoa(len(r.Places))}
	if r.Section != rotation.SectionAdded {
		v := "values " + strconv.Itoa(len(r.Groups))
		if len(r.Groups) > 1 {
			var ls []string
			for i, g := range r.Groups {
				if !g.Sealed {
					ls = append(ls, rotation.Letter(i))
				}
			}
			if len(ls) > 0 {
				v += " (" + strings.Join(ls, " ") + ")"
			}
		}
		parts = append(parts, v)
	}
	names := map[string]bool{}
	for _, p := range r.Places {
		names[p.Loc.Name] = true
	}
	if len(names) > 1 {
		parts = append(parts, "names "+strconv.Itoa(len(names)))
	}
	if t := since(ctx); !t.IsZero() && r.Section != rotation.SectionAdded {
		fresh := 0
		for _, p := range r.Places {
			if p.Loc.Fresh {
				fresh++
			}
		}
		parts = append(parts, "rotated "+strconv.Itoa(fresh)+"/"+strconv.Itoa(len(r.Places))+" (after "+stamp(t)+")")
	}
	return strings.Join(parts, "  ")
}

// coverage is a line per group of the row with a change: "<label> · C of T
// places · K keep the old value", lettered when the
// row has several groups.
func coverage(ctx *tui.Ctx, r view.RotateRow) []string {
	var out []string
	for i, g := range r.Groups {
		cov, ok := rotation.CoverageOf(ctx.Store, pending(ctx), rotation.GroupKey(g.Key), envIDs(ctx))
		if !ok || cov.Changed == 0 {
			continue
		}
		l := text.Sanitize(cov.Label) + " · " + strconv.Itoa(cov.Changed) + " of " + strconv.Itoa(cov.Total) + " places"
		switch keep := cov.Total - cov.Changed; {
		case keep == 1:
			l += " · 1 keeps the old value"
		case keep > 1:
			l += " · " + strconv.Itoa(keep) + " keep the old value"
		}
		if len(r.Groups) > 1 && !g.Sealed {
			l = rotation.Letter(i) + " " + l
		}
		out = append(out, l)
	}
	return out
}

// places draws the row's first lines in n lines; when some are left out
// the last says how many.
func (s *screen) places(ctx *tui.Ctx, r view.RotateRow, width, n int) []string {
	if n <= 0 {
		return nil
	}
	th := ctx.Theme
	show := r.Places
	more := 0
	if len(show) > n {
		show, more = show[:n-1], len(show)-(n-1)
	}
	cols := placeColsOf(ctx, r, show, width)
	out := make([]string, 0, n)
	for _, p := range show {
		out = append(out, ansi.Truncate("  "+placeLine(ctx, r, p, cols), width, "…"))
	}
	if more > 0 {
		out = append(out, th.Muted.Render(ansi.Truncate("  … "+strconv.Itoa(more)+" more · enter shows every place", width, "…")))
	}
	return out
}

// locCols are a line's place columns: the environment, then the service.
type locCols struct {
	short      bool
	envW, svcW int
}

// fit widens the columns to line p.
func (c *locCols) fit(ctx *tui.Ctx, p view.RotatePlace) {
	c.envW = max(c.envW, ansi.StringWidth(envOf(ctx, c.short, p)))
	c.svcW = max(c.svcW, ansi.StringWidth(serviceOf(ctx, p)))
}

// limit caps the columns to their share of width.
func (c *locCols) limit(width int) {
	c.envW, c.svcW = min(c.envW, max(6, width/5)), min(c.svcW, max(8, width/4))
}

// width is the columns' width with the gap between them.
func (c locCols) width() int { return c.envW + 2 + c.svcW }

// shrink narrows the columns to w, the service last: it tells more.
func (c *locCols) shrink(w int) {
	if c.width() <= w {
		return
	}
	c.envW = max(4, min(c.envW, w-2-c.svcW))
	c.svcW = max(6, w-2-c.envW)
}

// cells are line p's place columns.
func (c locCols) cells(ctx *tui.Ctx, p view.RotatePlace) string {
	return tui.Pad(envOf(ctx, c.short, p), c.envW) + "  " + tui.Pad(serviceOf(ctx, p), c.svcW)
}

func envOf(ctx *tui.Ctx, short bool, p view.RotatePlace) string {
	return envText(short, ctx.Store.EnvLabel(p.Loc.Place.EnvID))
}

func serviceOf(ctx *tui.Ctx, p view.RotatePlace) string {
	return text.Sanitize(ctx.Store.ServiceName(p.Loc.Place.ServiceID))
}

// placeCols are the column widths of place lines, and the copy brackets
// drawn beside them.
type placeCols struct {
	locCols
	names bool
	nameW int
	// links are each line's bracket cells by location id, linksW their
	// width; linked marks the copies whose source a bracket reaches. decW
	// is the decision column's width while brackets are drawn.
	links  map[string]string
	linked map[string]bool
	linksW int
	decW   int
	now    time.Time
	// age shows the age column; a narrow screen keeps the decision instead.
	age bool
}

// placeColsOf fits the columns to the lines shown; the name column shows
// when the row has several names.
func placeColsOf(ctx *tui.Ctx, r view.RotateRow, show []view.RotatePlace, width int) placeCols {
	c := placeCols{locCols: locCols{short: shortEnvs(ctx)}}
	names := map[string]bool{}
	for _, p := range r.Places {
		names[p.Loc.Name] = true
	}
	c.names = len(names) > 1
	for _, p := range show {
		c.fit(ctx, p)
		c.nameW = max(c.nameW, ansi.StringWidth(text.Sanitize(p.Loc.Name)))
	}
	c.limit(width)
	c.nameW = min(c.nameW, max(8, width/4))
	c.now = ctx.Clock()()
	c.age = true
	const minDec = 14 // what a decision needs to stay readable
	if c.lineW()+2+minDec > width {
		c.age = false
	}
	c.links, c.linked, c.linksW = links(ctx, show)
	if c.linksW > 0 {
		for _, p := range show {
			c.decW = max(c.decW, ansi.StringWidth(decision(ctx, r, p, c.linked[p.Loc.ID])))
		}
		// Brackets only where they fit; else a copy names its source.
		if c.lineW()+2+c.decW+2+c.linksW > width {
			c.links, c.linked, c.linksW, c.decW = nil, nil, 0, 0
		}
	}
	return c
}

// lineW is a place line's width up to its decision.
func (c placeCols) lineW() int {
	w := 3 + c.width() + 2 + dateW
	if c.age {
		w += 2 + agoW
	}
	if c.names {
		w += c.nameW + 2
	}
	return w
}

// Widths of the date and age columns.
const (
	dateW = len("01-02 15:04")
	agoW  = len("999 days ago")
)

// ago is how long before now t was, in the largest whole unit: "now",
// "5 min ago", "3h ago", "1 day ago", "212 days ago".
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	case d < 48*time.Hour:
		return "1 day ago"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + " days ago"
}

// links draws the copy brackets of lines: one per source whose copies are
// among lines, joining them, each in its own lane so none cross. It returns
// every line's cells by location id (blank where no bracket passes), the
// copies a bracket reaches, and the lanes' width; zero width when no copy's
// source is among lines.
func links(ctx *tui.Ctx, lines []view.RotatePlace) (map[string]string, map[string]bool, int) {
	at := map[string]int{}
	for i, p := range lines {
		at[p.Loc.ID] = i
	}
	type span struct {
		member map[int]bool
		lo, hi int
	}
	bySrc := map[int]*span{}
	linked := map[string]bool{}
	for i, p := range lines {
		loc, ok := copySourceLoc(ctx, p)
		if !ok {
			continue
		}
		j, ok := at[loc]
		if !ok || j == i {
			continue
		}
		sp := bySrc[j]
		if sp == nil {
			sp = &span{member: map[int]bool{j: true}, lo: j, hi: j}
			bySrc[j] = sp
		}
		sp.member[i], sp.lo, sp.hi = true, min(sp.lo, i), max(sp.hi, i)
		linked[p.Loc.ID] = true
	}
	if len(bySrc) == 0 {
		return nil, nil, 0
	}
	spans := make([]*span, 0, len(bySrc))
	for _, sp := range bySrc {
		spans = append(spans, sp)
	}
	slices.SortFunc(spans, func(a, b *span) int { return a.lo - b.lo })
	var ends []int // the last row of each lane
	lane := make([]int, len(spans))
	for k, sp := range spans {
		lane[k] = len(ends)
		for l, end := range ends {
			if end < sp.lo {
				lane[k] = l
				break
			}
		}
		if lane[k] == len(ends) {
			ends = append(ends, sp.hi)
		} else {
			ends[lane[k]] = sp.hi
		}
	}
	grid := make([][]string, len(lines))
	for i := range grid {
		grid[i] = slices.Repeat([]string{" "}, len(ends))
	}
	for k, sp := range spans {
		for i := sp.lo; i <= sp.hi; i++ {
			switch {
			case !sp.member[i]:
				grid[i][lane[k]] = "│"
			case i == sp.lo:
				grid[i][lane[k]] = "┐"
			case i == sp.hi:
				grid[i][lane[k]] = "┘"
			default:
				grid[i][lane[k]] = "┤"
			}
		}
	}
	cells := make(map[string]string, len(lines))
	for i, p := range lines {
		cells[p.Loc.ID] = ctx.Theme.Accent2.Render(strings.Join(grid[i], " "))
	}
	return cells, linked, 2*len(ends) - 1
}

// placeLine is a line's letter, place, name, last change and decision. The
// letter comes first, so a value's lines line up on every screen.
func placeLine(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace, c placeCols) string {
	th := ctx.Theme
	var b strings.Builder
	b.WriteString(letterCell(th, p.Letter) + " ")
	b.WriteString(c.cells(ctx, p) + "  ")
	if c.names {
		b.WriteString(tui.Pad(text.Sanitize(p.Loc.Name), c.nameW) + "  ")
	}
	date, age := "", ""
	if !p.Loc.Updated.IsZero() {
		date, age = stamp(p.Loc.Updated), ago(c.now, p.Loc.Updated)
	}
	cells := tui.Pad(date, dateW)
	if c.age {
		cells += "  " + tui.Pad(age, agoW)
	}
	b.WriteString(th.Muted.Render(cells))
	d := decision(ctx, r, p, c.linked[p.Loc.ID])
	if c.linksW > 0 {
		d = tui.Pad(d, c.decW) + "  " + c.links[p.Loc.ID]
	}
	b.WriteString("  " + d)
	return b.String()
}

// letterCell is a value letter in its colour, two cells wide; blank for a
// line without one (sealed).
func letterCell(th tui.Theme, letter int) string {
	if letter < 0 {
		return "  "
	}
	return th.Letters[letter%len(th.Letters)].Render(tui.Pad(rotation.Letter(letter), 2))
}

// decision is a line's decision column: its change's overlay (a copy
// names its source) and drift, else why it cannot take one, else
// "✓ rotated" when its group changed after the leak everywhere.
func decision(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace, linked bool) string {
	th := ctx.Theme
	switch {
	case p.Pending != nil:
		tone := th.New
		switch p.Overlay.Tone {
		case "danger":
			tone = th.Danger
		case "warn":
			tone = th.Warn
		}
		out := tone.Render(text.Sanitize(p.Overlay.Short))
		if o := p.Pending.Origin; o != nil && o.Source == changeset.SourceGenerate && p.Pending.Kind == "variable.set" {
			out = th.Letters[2].Render("◆ GEN " + strings.TrimPrefix(text.Sanitize(p.Overlay.Short), "→ ")) // yellow
		}
		if src, ok := copyFrom(ctx, p); ok {
			// A bracket joins it to its source when that is shown.
			// blue, as the brackets
			out = th.Accent2.Render("⇉ from " + placeLabel(ctx, src))
			if linked {
				out = th.Accent2.Render("⇉ COPY")
			}
		}
		if p.Pending.RunID != "" {
			out = th.Warn.Render(ctx.Icons.Run() + " " + text.Sanitize(p.Pending.RunID))
		}
		if d := tui.DriftText(ctx.Icons, p.Drifted); d != "" {
			out += "  " + th.Warn.Render(d)
		}
		return out
	case p.ReadOnly:
		return th.Muted.Render("read only")
	case p.DB:
		return th.Muted.Render("database")
	case p.Loc.Fresh && rotatedGroup(r, p):
		return th.OK.Render(ctx.Icons.Ok() + " rotated")
	case p.Loc.Fresh && lineGroup(r, p) != nil:
		return th.Warn.Render("↻ still leaked")
	}
	return ""
}

// stillLeaked explains a line changed after the leak whose value is older
// elsewhere: the place of its value unchanged since before the leak, and
// when. false when p is not such a line.
func stillLeaked(r view.RotateRow, p view.RotatePlace) (view.RotatePlace, bool) {
	if !p.Loc.Fresh || p.Pending != nil || p.ReadOnly || p.DB || rotatedGroup(r, p) {
		return view.RotatePlace{}, false
	}
	if lineGroup(r, p) == nil {
		return view.RotatePlace{}, false
	}
	gi := groupIndex(r)
	for _, q := range r.Places {
		if gi[q.Loc.ID] == gi[p.Loc.ID] && !q.Loc.Fresh {
			return q, true
		}
	}
	return view.RotatePlace{}, false
}

// rotatedGroup reports whether line p's group changed everywhere after the
// leak: a fresh line of another group may still hold the leaked value.
func rotatedGroup(r view.RotateRow, p view.RotatePlace) bool {
	g := lineGroup(r, p)
	return g != nil && g.AllFresh
}

// pwPanel is a password row's detail panel in at most limit lines: the
// decision, where the password lives, the URL counts, the standing notes,
// then the first places (other users' and unrecognised URLs left out).
func (s *screen) pwPanel(ctx *tui.Ctx, r view.RotateRow, width, limit int) []string {
	th := ctx.Theme
	cut := func(l string) string { return ansi.Truncate(l, width, "…") }
	head := []string{
		cut(tag(ctx, r) + "  " + text.Sanitize(r.Label) + " · " + pwAction(ctx, r)),
		cut(th.Muted.Render(pwWhere(ctx, r))),
	}
	if !r.Password.KeyFile {
		head = append(head, cut(urlsText(countURLs(r))))
	}
	for _, n := range pwNotes(ctx, r, "c continue") {
		head = append(head, cut(n))
	}
	if len(head) >= limit {
		return head[:limit]
	}
	var show []view.RotatePlace
	for _, p := range r.Places {
		if !p.Own && !shownOnly(r, p) {
			show = append(show, p)
		}
	}
	n := limit - len(head)
	more := 0
	if len(show) > n {
		show, more = show[:n-1], len(show)-(n-1)
	}
	cols := pwColsOf(ctx, r, show, width-2)
	for _, p := range show {
		head = append(head, cut("  "+pwPlaceLine(ctx, r, p, cols)))
	}
	if more > 0 {
		head = append(head, th.Muted.Render(cut("  … "+strconv.Itoa(more)+" more · enter shows every place")))
	}
	return head
}

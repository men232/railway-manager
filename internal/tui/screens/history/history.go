// Package history is the History screen: the merged history feed of the
// selected environments (view.Feed), newest first, with the entries
// railway-manager's own runs made marked ◆ (the RunMark icon). It never
// shows a value: of an entry's payload only the variable names are read.
//
// It reads the first page of each environment's history, its patches and
// its project's members on open and on return (Resume), when not read yet;
// m reads the next page of every environment that has one and r reads the
// first pages again. Enter on a run made here opens it. A failed read
// stands in the footer and the entries already shown stay.
package history

import (
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

// filterKey is the shared filter's key of the screen.
const filterKey = "history"

// pageMsg is a history page this screen asked for, tagged with the read's
// sequence number. The root stores it like any loaders.HistoryMsg (ApplyTo
// is promoted); only the read still awaited by that number updates the
// screen, so a page asked before a refresh, or before the screen was
// covered, is ignored on arrival.
type pageMsg struct {
	loaders.HistoryMsg
	seq int64
}

// pageSeq numbers every history read of every History screen, so a page
// one screen asked for never answers another's read.
var pageSeq atomic.Int64

type screen struct {
	preset string
	filter *components.Filter
	list   components.List

	// The screen's own read tracking (the store's load status is not used:
	// a stale answer updates it too). first and next: the sequence number
	// of the first-page and next-page history read in flight by env; r
	// and Suspend forget them, so an older page is ignored when it lands.
	// patches and members: in flight by env and project.
	first, next      map[string]int64
	patches, members map[string]bool
	// since is the start of each env's reads (the first page's), so next
	// pages page the same query.
	since map[string]time.Time
	// errs is the last failed read by key ("h:"/"p:" env, "m:" project);
	// a later read that succeeds clears it.
	errs map[string]string
}

// Build is the registry constructor. No argument gives the whole feed; one
// string presets the filter (Variables' d passes the cursor's name). It
// returns nil for any other arguments.
func Build(args ...any) tui.Screen {
	s := &screen{
		filter: components.NewFilter(filterKey),
		first:  map[string]int64{}, next: map[string]int64{},
		patches: map[string]bool{}, members: map[string]bool{},
		since: map[string]time.Time{}, errs: map[string]string{},
	}
	switch len(args) {
	case 0:
		return s
	case 1:
		if name, ok := args[0].(string); ok {
			s.preset = name
			return s
		}
	}
	return nil
}

func (s *screen) Title() string { return "History" }

// Init sets the preset filter, then reads what is not read yet.
func (s *screen) Init(ctx *tui.Ctx) tea.Cmd {
	if s.preset != "" {
		if ctx.Filters == nil {
			ctx.Filters = map[string]string{}
		}
		ctx.Filters[filterKey] = s.preset
	}
	s.filter.Load(ctx)
	return tea.Batch(s.reads(ctx, false), tui.Emit(tui.RefreshRunsMsg{}))
}

// Resume reads what is not read yet (a failed read is asked again) and the
// runs index, which runs made meanwhile change.
func (s *screen) Resume(ctx *tui.Ctx) tea.Cmd {
	return tea.Batch(s.reads(ctx, false), tui.Emit(tui.RefreshRunsMsg{}))
}

// Suspend forgets the reads in flight: their answers reach the screen on
// top instead (the root still stores them), so Resume asks again for what
// is not read by then, and m is not left waiting.
func (s *screen) Suspend(*tui.Ctx) {
	clear(s.first)
	clear(s.next)
	clear(s.patches)
	clear(s.members)
}

// Capturing reports whether the filter takes every key.
func (s *screen) Capturing() bool { return s.filter.Editing() }

func (s *screen) Keys() []tui.KeyHint {
	if s.filter.Editing() {
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	return []tui.KeyHint{
		{Key: "enter", Label: "open run", Rank: 1},
		{Key: "m", Label: "more", Rank: 2},
		{Key: "r", Label: "refresh", Rank: 3},
		{Key: "/", Label: "filter", Rank: 4, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
}

// envIDs is the selected environments (all of a selected project's when
// none of it is chosen, as everywhere).
func envIDs(ctx *tui.Ctx) []string {
	var out []string
	for _, t := range loaders.Targets(ctx.Store.Projects(), ctx.Settings) {
		for _, e := range t.Environments {
			out = append(out, e.ID)
		}
	}
	return out
}

// projectIDs is the projects of the selected environments.
func projectIDs(ctx *tui.Ctx) []string {
	var out []string
	for _, t := range loaders.Targets(ctx.Store.Projects(), ctx.Settings) {
		if len(t.Environments) > 0 {
			out = append(out, t.ProjectID)
		}
	}
	return out
}

// reads asks for the first history page and the patches of each selected
// environment, and the members of their projects: those not read yet, or
// every first page and patches when again (r). Nothing already in flight
// is asked twice.
func (s *screen) reads(ctx *tui.Ctx, again bool) tea.Cmd {
	if ctx.API == nil || ctx.Store == nil {
		return nil
	}
	clock := ctx.Clock()
	var cmds []tea.Cmd
	for _, env := range envIDs(ctx) {
		if _, _, _, read := ctx.Store.History(env); (again || !read) && s.first[env] == 0 {
			s.since[env] = clock().Add(-loaders.HistorySince)
			s.first[env] = s.page(&cmds, ctx, env, "")
		}
		if (again || ctx.Store.Load(store.PatchesKey(env)).ReadAt.IsZero()) && !s.patches[env] {
			s.patches[env] = true
			cmds = append(cmds, loaders.Patches(ctx.API, env, clock().Add(-loaders.HistorySince), clock))
		}
	}
	for _, p := range projectIDs(ctx) {
		if ctx.Store.Load(store.MembersKey(p)).ReadAt.IsZero() && !s.members[p] {
			s.members[p] = true
			cmds = append(cmds, loaders.Members(ctx.API, p, clock))
		}
	}
	return tea.Batch(cmds...)
}

// more asks for the next page of every environment that has one and no
// read in flight.
func (s *screen) more(ctx *tui.Ctx) tea.Cmd {
	if ctx.API == nil {
		return nil
	}
	clock := ctx.Clock()
	var cmds []tea.Cmd
	waiting := false
	for _, env := range envIDs(ctx) {
		_, cursor, hasNext, read := ctx.Store.History(env)
		if s.first[env] != 0 || s.next[env] != 0 {
			waiting = true
			continue
		}
		if !read || !hasNext {
			continue
		}
		if _, ok := s.since[env]; !ok { // first page read by another screen
			s.since[env] = clock().Add(-loaders.HistorySince)
		}
		s.next[env] = s.page(&cmds, ctx, env, cursor)
	}
	if len(cmds) == 0 && !waiting {
		return tui.Flash('!', "no older entries to read")
	}
	return tea.Batch(cmds...)
}

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if ctx.Store == nil {
		return s, nil
	}
	switch m := msg.(type) {
	case pageMsg:
		awaited := s.next
		if m.After == "" {
			awaited = s.first
		}
		if awaited[m.EnvID] != m.seq {
			return s, nil // asked before a refresh or a Suspend: ignored
		}
		delete(awaited, m.EnvID)
		s.record("h:"+m.EnvID, m.Err)
	case loaders.PatchesMsg:
		delete(s.patches, m.EnvID)
		s.record("p:"+m.EnvID, m.Err)
	case loaders.MembersMsg:
		delete(s.members, m.ProjectID)
		s.record("m:"+m.ProjectID, m.Err)
	case tea.KeyMsg:
		return s, s.key(ctx, m)
	}
	return s, nil
}

// record keeps a read's failure under key; success (or a partial answer)
// clears it.
func (s *screen) record(key string, err error) {
	var partial *railway.PartialError
	if err == nil || errors.As(err, &partial) {
		delete(s.errs, key)
		return
	}
	s.errs[key] = err.Error()
}

func (s *screen) key(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	if h, cmd := s.filter.Update(ctx, k); h {
		return cmd
	}
	rows, _ := s.rows(ctx)
	h := s.listHeight(ctx, ctx.Height-tui.ChromeRows)
	switch k.Type {
	case tea.KeyUp:
		s.list.Move(len(rows), -1, h)
	case tea.KeyDown:
		s.list.Move(len(rows), 1, h)
	case tea.KeyPgUp:
		s.list.Move(len(rows), -h, h)
	case tea.KeyPgDown:
		s.list.Move(len(rows), h, h)
	case tea.KeyEnter:
		return s.open(rows)
	case tea.KeyRunes:
		switch string(k.Runes) {
		case "j":
			s.list.Move(len(rows), 1, h)
		case "k":
			s.list.Move(len(rows), -1, h)
		case "m":
			return s.more(ctx)
		case "r":
			clear(s.next)
			return tea.Batch(s.reads(ctx, true), tui.Emit(tui.RefreshRunsMsg{}))
		}
	}
	return nil
}

// open opens the cursor's run when it was made here; a run made elsewhere
// is only named.
func (s *screen) open(rows []view.FeedRow) tea.Cmd {
	if len(rows) == 0 {
		return nil
	}
	row := rows[max(0, min(s.list.Cursor, len(rows)-1))]
	switch {
	case row.RunID == "":
		return nil
	case row.Local:
		return tui.Emit(tui.OpenMsg{Name: "run", Args: []any{row.RunID}})
	}
	return tui.Flash('!', "run "+row.RunID+" was made on another machine")
}

// rows is the feed rows matching the filter and the count before
// filtering.
func (s *screen) rows(ctx *tui.Ctx) ([]view.FeedRow, int) {
	if ctx.Views == nil {
		return nil, 0
	}
	s.filter.Load(ctx)
	all := ctx.Views.Feed(envIDs(ctx), ctx.RunIndex.Runs)
	if !s.filter.Active() {
		return all, len(all)
	}
	var out []view.FeedRow
	for _, r := range all {
		t := shownOf(r, false)
		if s.filter.Match(t.who + " " + t.action + " " + t.service + " " + t.names) {
			out = append(out, r)
		}
	}
	return out, len(all)
}

// hasMore reports whether an environment has a next page to read.
func hasMore(ctx *tui.Ctx) bool {
	for _, env := range envIDs(ctx) {
		if _, _, hasNext, _ := ctx.Store.History(env); hasNext {
			return true
		}
	}
	return false
}

// Note is the entry count, and that m reads more when there is more.
func (s *screen) Note(ctx *tui.Ctx) string {
	if ctx.Store == nil || ctx.Views == nil {
		return ""
	}
	_, total := s.rows(ctx)
	note := strconv.Itoa(total) + " entries"
	if total == 1 {
		note = "1 entry"
	}
	if hasMore(ctx) {
		note += " · m older"
	}
	return note
}

// Notice is the first failed read, in environment order.
func (s *screen) Notice(ctx *tui.Ctx) tui.FlashMsg {
	if ctx.Store == nil || len(s.errs) == 0 {
		return tui.FlashMsg{}
	}
	fail := func(what, err string) tui.FlashMsg {
		return tui.FlashMsg{Kind: '✗', Text: text.Sanitize("cannot read " + what + ": " + err + " · r retries")}
	}
	for _, env := range envIDs(ctx) {
		if e, ok := s.errs["h:"+env]; ok {
			return fail("history of "+ctx.Store.EnvLabel(env), e)
		}
		if e, ok := s.errs["p:"+env]; ok {
			return fail("patches of "+ctx.Store.EnvLabel(env), e)
		}
	}
	for _, p := range ctx.Store.Projects() {
		if e, ok := s.errs["m:"+p.ID]; ok {
			return fail("members of "+p.Name, e)
		}
	}
	return tui.FlashMsg{}
}

// reading reports whether a first page or a next page is in flight.
func (s *screen) reading() (first, next bool) { return len(s.first) > 0, len(s.next) > 0 }

// page adds the read of an env's history page from cursor after (""= the
// first page) to cmds, and returns its sequence number.
func (s *screen) page(cmds *[]tea.Cmd, ctx *tui.Ctx, env, after string) int64 {
	seq, read := pageSeq.Add(1), loaders.History(ctx.API, env, after, s.since[env], ctx.Clock())
	*cmds = append(*cmds, func() tea.Msg {
		m, _ := read().(loaders.HistoryMsg)
		return pageMsg{HistoryMsg: m, seq: seq}
	})
	return seq
}

// listHeight is the rows left for the list in a body of height.
func (s *screen) listHeight(ctx *tui.Ctx, height int) int {
	if s.filter.Active() || s.filter.Editing() {
		height--
	}
	if _, next := s.reading(); next {
		height--
	}
	return max(height, 1)
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	if ctx.Store == nil {
		return ""
	}
	th := ctx.Theme
	rows, total := s.rows(ctx)
	var lines []string
	if f := s.filter.View(width, len(rows), total); f != "" {
		lines = append(lines, f)
	}
	first, next := s.reading()
	switch {
	case len(rows) > 0:
		listH := s.listHeight(ctx, height)
		from, to := s.list.Window(len(rows), listH)
		cells := make([][]cell, 0, to-from)
		sh := short(ctx)
		for i := from; i < to; i++ {
			cells = append(cells, s.cells(ctx, rows[i], sh))
		}
		lay := fit(cells, width)
		for i, cs := range cells {
			lines = append(lines, render(th, cs, lay, from+i == s.list.Cursor, width))
		}
	case len(envIDs(ctx)) == 0:
		lines = append(lines, th.Muted.Render("no environments selected · Settings chooses them"))
	case first:
		lines = append(lines, th.Muted.Render("reading history…"))
	case s.filter.Active():
		lines = append(lines, th.Muted.Render("no entry matches the filter"))
	default:
		lines = append(lines, th.Muted.Render("no history in the last 30 days"))
	}
	if next {
		lines = append(lines, th.Muted.Render(ansi.Truncate("reading older entries…", width, "…")))
	}
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

// shown is a row's text as shown, each part sanitized on its own.
type shown struct {
	who, action, object, loc, service, names string
}

// shownOf is a row's text; short shows the environment without its
// project.
func shownOf(r view.FeedRow, short bool) shown {
	who := text.Sanitize(r.Who)
	if who == "" {
		who = view.ByNone
	}
	env := r.Env
	if short {
		env = r.EnvName
	}
	t := shown{
		who: who, action: text.Sanitize(r.Action), object: objectName(r.Object),
		loc: text.Sanitize(env), service: text.Sanitize(r.Service),
	}
	if t.service != "" {
		t.loc += " · " + t.service
	}
	names := make([]string, len(r.Names))
	for i, n := range r.Names {
		names[i] = text.Sanitize(n)
	}
	t.names = strings.Join(names, ", ")
	return t
}

// objectNames are the short names of Railway's history objects; any other
// object shows as Railway names it.
var objectNames = map[string]string{
	"EnvironmentPatch": "patch",
	"Variable":         "variable",
	"Deployment":       "deploy",
	"SSHSession":       "ssh",
}

func objectName(o string) string {
	if n, ok := objectNames[o]; ok {
		return n
	}
	return text.Sanitize(o)
}

// The columns of a row, in order. The tail is last and unpadded: the
// variable names, or on a run's patch entry the mark (patch entries carry
// no names).
const (
	colTime = iota
	colWho
	colAction
	colObject
	colLoc
	colTail
	colCount
)

// rank is when a column goes on a narrow screen, once the marks are short:
// the highest first (env · service, then who, then the object); 0 never
// goes. The env · service column is cut instead while minFlex of it is
// left. The tail's names are cut to the room left, kept at minNames while
// a column can still give way; a
// mark is never cut: the action, then the time give way first.
var (
	rank = [colCount]int{colLoc: 4, colWho: 3, colObject: 2}
	// limit caps a column's width; 0 is none.
	limit = [colCount]int{colWho: 16, colAction: 12, colObject: 18}
)

const (
	minFlex = 8
	// minNames is how much of the names a narrow screen keeps before
	// env · service and who give way: they matter most on Variable rows.
	minNames = 16
	gap      = 2
)

// cell is one column of a row: sanitized text in a style (nil: plain).
// A tail with a mark (the RunMark icon, plain "◆") has its short form too
// ("◆ <id tail>"), used unless every column fits beside the full one
// ("◆ run <id>").
type cell struct {
	text, short string
	style       *lipgloss.Style
	mark        bool
}

// short reports whether environments are shown without their project: one
// project is selected.
func short(ctx *tui.Ctx) bool {
	return len(projectIDs(ctx)) <= 1
}

// idTail is the part of a run id after its last "-" (the random part of
// "20261024-115900-ab12"), the whole id without one.
func idTail(id string) string {
	if i := strings.LastIndex(id, "-"); i >= 0 && i < len(id)-1 {
		return id[i+1:]
	}
	return id
}

func (s *screen) cells(ctx *tui.Ctx, r view.FeedRow, short bool) []cell {
	th := ctx.Theme
	st := func(x lipgloss.Style) *lipgloss.Style { return &x }
	t := shownOf(r, short)
	cs := make([]cell, colCount)
	cs[colTime] = cell{text: r.At.UTC().Format("01-02 15:04"), style: st(th.Muted)}
	cs[colWho] = cell{text: t.who}
	cs[colAction] = cell{text: t.action}
	cs[colObject] = cell{text: t.object, style: st(th.Muted)}
	cs[colLoc] = cell{text: t.loc, style: st(th.Muted)}
	switch {
	case r.RunID != "":
		tone := th.Muted
		if r.Local {
			tone = th.Accent
		}
		id := text.Sanitize(r.RunID)
		m := ctx.Icons.RunMark()
		cs[colTail] = cell{text: m + " run " + id, short: m + " " + idTail(id), style: st(tone), mark: true}
	default:
		cs[colTail] = cell{text: t.names}
	}
	return cs
}

// layout is how the rows are drawn: the width of each padded column, 0 for
// a column not shown, and whether marks take their short form.
type layout struct {
	w     [colCount]int
	short bool
}

// fit lays the rows out in width: each column at its widest cell (capped)
// with the full marks when every row fits so; else the marks are short and
// columns go or are cut by rank until every row, with its tail, fits. The
// tail's own width is not shared: each row's tail takes the room left.
func fit(rows [][]cell, width int) layout {
	var lay layout
	w := &lay.w
	for _, cs := range rows {
		for i := range colTail {
			w[i] = max(w[i], ansi.StringWidth(cs[i].text))
		}
	}
	for i := range w {
		if limit[i] > 0 {
			w[i] = min(w[i], limit[i])
		}
	}
	fixed := func() int {
		t, n := 0, 0
		for _, x := range w[:colTail] {
			if x > 0 {
				t += x
				n++
			}
		}
		return t + gap*max(n-1, 0)
	}
	// need is the widest row: the padded columns and its tail (names
	// counted at minNames at most, or whole when whole is asked).
	need := func(whole bool) int {
		most := fixed()
		for _, cs := range rows {
			t := cs[colTail]
			tw := ansi.StringWidth(t.text)
			switch {
			case t.mark && lay.short:
				tw = ansi.StringWidth(t.short)
			case !t.mark && !whole:
				tw = min(tw, minNames)
			}
			if tw > 0 {
				most = max(most, fixed()+gap+tw)
			}
		}
		return most
	}
	if need(true) <= width {
		return lay
	}
	lay.short = true
	for need(false) > width {
		i := -1
		for j := range colTail {
			if w[j] > 0 && rank[j] > 0 && (i < 0 || rank[j] > rank[i]) {
				i = j
			}
		}
		if i < 0 {
			break
		}
		if over := need(false) - width; i == colLoc && w[i]-over >= minFlex {
			w[i] -= over
			break
		}
		w[i] = 0
	}
	// then the action and the time are cut, so a mark stays whole
	for _, i := range []int{colAction, colTime} {
		if over := need(false) - width; over > 0 && w[i] > 0 {
			w[i] = max(w[i]-over, min(3, w[i]))
		}
	}
	return lay
}

// render draws a row: the padded columns at the widths, then its tail in
// the room left; plain in the selection style when selected, cut to width
// either way.
func render(th tui.Theme, cs []cell, lay layout, selected bool, width int) string {
	var b strings.Builder
	used := 0
	put := func(t string, style *lipgloss.Style) {
		if used > 0 {
			b.WriteString(strings.Repeat(" ", gap))
			used += gap
		}
		used += ansi.StringWidth(t)
		if style != nil && !selected {
			t = style.Render(t)
		}
		b.WriteString(t)
	}
	for i := range colTail {
		if lay.w[i] > 0 {
			put(tui.Pad(cs[i].text, lay.w[i]), cs[i].style)
		}
	}
	switch t := cs[colTail]; {
	case t.mark && lay.short:
		put(t.short, t.style)
	case t.mark:
		put(t.text, t.style)
	case t.text != "":
		if room := width - used - gap; room > 0 {
			put(ansi.Truncate(t.text, room, "…"), t.style)
		}
	}
	line := ansi.Truncate(strings.TrimRight(b.String(), " "), width, "…")
	if selected {
		return th.Selected.Render(line)
	}
	return line
}

// Package changeset is the Change set screen: the pending changes grouped by
// service, the derived redeploys, and the unfinished runs. It selects what
// to apply, applies, removes changes, toggles redeploys and opens runs.
package changeset

import (
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

type rowKind int

const (
	kindRun rowKind = iota
	kindChange
	kindRedeploy
)

// entry is one drawn line. row is the index into the cursor rows, or -1 for
// a header or note.
type entry struct {
	row    int
	kind   rowKind
	id     string // run id, change id or place key
	locked bool
	// invalid: the change cannot be applied as it stands (view's
	// ChangeRow.Invalid); it can only be removed.
	invalid bool
	segs    []seg
}

type seg struct {
	text  string
	style *lipgloss.Style // nil: plain
}

type screen struct {
	filter      *components.Filter
	cursor, off int
	selected    map[string]bool
	confirm     *components.Confirm
	ids         []string // what the open confirm applies; nil for all
	// partly is the warning above the open confirm: the partly rotated
	// groups the applied changes belong to; "" for none.
	partly string
	// stalled is the stalled-password banner; enter off a run row
	// continues its run.
	stalled components.Stalled
}

// New returns the Change set screen.
func New() tui.Screen {
	return &screen{selected: map[string]bool{}, filter: components.NewFilter("changeset")}
}

// Build is the registry constructor: no arguments, or "select" and the ids
// ([]string) to select (Rotate's apply). Ids that are gone, locked or
// invalid are dropped on Init. It returns nil for other arguments.
func Build(args ...any) tui.Screen {
	switch len(args) {
	case 0:
		return New()
	case 2:
		if ids, ok := args[1].([]string); ok && args[0] == "select" {
			s := &screen{selected: make(map[string]bool, len(ids)), filter: components.NewFilter("changeset")}
			for _, id := range ids {
				s.selected[id] = true
			}
			return s
		}
	}
	return nil
}

func (s *screen) Title() string { return "Change set" }

func (s *screen) Init(ctx *tui.Ctx) tea.Cmd {
	s.filter.Load(ctx)
	if len(s.selected) > 0 {
		_, allRows, _ := s.layout(ctx, true)
		s.prune(allRows)
	}
	return tui.Emit(tui.RefreshRunsMsg{})
}

// Resume asks for the runs index again when the screen is on top again.
func (s *screen) Resume(ctx *tui.Ctx) tea.Cmd { return tui.Emit(tui.RefreshRunsMsg{}) }

// Capturing reports whether the apply confirmation takes every key.
func (s *screen) Capturing() bool {
	return s.confirm != nil || s.stalled.Capturing() || s.filter.Editing()
}

func (s *screen) Keys() []tui.KeyHint {
	if k := s.stalled.Keys(); k != nil {
		return k
	}
	if s.confirm != nil {
		return []tui.KeyHint{
			{Key: "enter", Label: "confirm", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	}
	if s.filter.Editing() {
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	return []tui.KeyHint{
		{Key: "a", Label: "apply", Rank: 1},
		{Key: "space", Label: "select", Rank: 2},
		{Key: "x", Label: "remove", Rank: 3},
		{Key: "t", Label: "redeploy", Rank: 4},
		{Key: "enter", Label: "open run", Rank: 5},
		{Key: "/", Label: "filter", Rank: 6, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
}

// shownRuns are the unfinished runs and the newest finished one.
func (s *screen) shownRuns(ctx *tui.Ctx) []apply.RunInfo {
	var out []apply.RunInfo
	finished := false
	for _, r := range ctx.RunIndex.Runs { // newest first
		st := r.State
		if p, ok := ctx.Running[r.ID]; ok && p.State != "" {
			st = p.State
		}
		switch st {
		case apply.RunRunning, apply.RunFailed:
			r.State = st
			out = append(out, r)
		case apply.RunFinished:
			if !finished {
				finished = true
				r.State = st
				out = append(out, r)
			}
		}
	}
	return out
}

// progress is done/total for a run: live when it runs here, else journal.
func progress(ctx *tui.Ctx, r apply.RunInfo) (done, total int) {
	if p, ok := ctx.Running[r.ID]; ok && p.Total > 0 {
		return p.Done, p.Total
	}
	for _, st := range r.Steps {
		total++
		if st.Status == apply.StatusDone || st.Status == apply.StatusSkipped {
			done++
		}
	}
	return done, total
}

// layout builds the drawn lines and the cursor rows, and counts the rows
// before filtering.
func (s *screen) layout(ctx *tui.Ctx, unfiltered bool) (lines []entry, rows []entry, total int) {
	th := ctx.Theme
	s.filter.Load(ctx)
	match := func(t string) bool { return unfiltered || s.filter.Match(t) }
	add := func(e entry) {
		if e.kind >= 0 && e.row >= 0 {
			e.row = len(rows)
			rows = append(rows, e)
		}
		lines = append(lines, e)
	}
	head := func(t string) { add(entry{row: -1, segs: []seg{{t, &th.Accent2}}}) }
	note := func(t string, st *lipgloss.Style) { add(entry{row: -1, segs: []seg{{"  " + t, st}}}) }

	head("runs")
	runs := s.shownRuns(ctx)
	switch {
	case !ctx.RunIndex.Loaded:
		note("reading runs…", &th.Muted)
	case len(runs) == 0:
		note("no runs", &th.Muted)
	}
	runsAt, keptRuns := len(lines)-1, 0 // the heading is the line before
	for _, r := range runs {
		total++
		if !match(r.ID + " " + r.State) {
			continue
		}
		keptRuns++
		done, steps := progress(ctx, r)
		glyph, st := ctx.Icons.Run(), &th.Warn
		switch r.State {
		case apply.RunFailed:
			glyph, st = ctx.Icons.Fail(), &th.Danger
		case apply.RunFinished:
			glyph, st = ctx.Icons.Ok(), &th.OK
		}
		t := glyph + " " + text.Sanitize(r.ID) + " " + r.State + " " + strconv.Itoa(done) + "/" + strconv.Itoa(steps)
		add(entry{kind: kindRun, id: r.ID, segs: []seg{{"  " + t, st}}})
	}

	if !unfiltered && len(runs) > 0 && keptRuns == 0 {
		lines = lines[:runsAt]
	}
	var v view.ChangeSetView
	if ctx.Views != nil {
		v = ctx.Views.ChangeSet(ctx.CS, selectedEnvIDs(ctx))
	}
	head("changes")
	if len(v.Groups) == 0 {
		note("no pending changes", &th.Muted)
	}
	for _, c := range v.Coverage {
		note(coverageText(c), &th.Warn)
	}
	for _, g := range v.Groups {
		groupAt := len(lines)
		add(entry{row: -1, segs: []seg{{"  " + groupLabel(g.Label), &th.Muted}}})
		kept := 0
		for _, c := range g.Changes {
			total++
			invalid := c.Invalid != ""
			if !match(g.Label + " " + c.Name + " " + c.Overlay.Short + " " + invalidText(c.Invalid) + " " + tui.DriftText(ctx.Icons, c.Drifted)) {
				continue
			}
			kept++
			locked := c.Change.RunID != ""
			mark := "  "
			if s.selected[c.Change.ID] {
				mark = "* "
			}
			segs := []seg{{"    " + mark + text.Sanitize(c.Name), nil}}
			switch {
			case locked:
				segs = append(segs, seg{"  " + ctx.Icons.Run() + " " + text.Sanitize(c.Change.RunID), &th.Warn})
			case invalid:
				segs = append(segs, seg{"  " + ctx.Icons.Fail() + " " + invalidText(c.Invalid), &th.Danger})
			default:
				if o := text.Sanitize(c.Overlay.Short); o != "" {
					segs = append(segs, seg{"  " + o, toneOf(ctx, c.Overlay.Tone)})
				}
			}
			if from := copyText(ctx, c.Change); from != "" {
				segs = append(segs, seg{"  " + from, &th.Muted})
			}
			if d := tui.DriftText(ctx.Icons, c.Drifted); d != "" {
				// a mark only: the change is kept and still applies
				segs = append(segs, seg{"  " + d, &th.Warn})
			}
			if f := text.Sanitize(c.Change.Flag); f != "" {
				segs = append(segs, seg{"  ! " + f, &th.Warn})
			}
			add(entry{kind: kindChange, id: c.Change.ID, locked: locked, invalid: invalid, segs: segs})
		}
		if kept == 0 && len(g.Changes) > 0 { // no change of this group matches: drop its label
			lines = lines[:groupAt]
		}
	}

	if len(v.Redeploys) > 0 || v.Err != "" {
		head("redeploys")
	}
	redeployAt, keptRedeploys := len(lines)-1, 0
	for _, rd := range v.Redeploys {
		total++
		if !match(rd.Label) {
			continue
		}
		keptRedeploys++
		box := "[ ] "
		if rd.On {
			box = "[x] "
		}
		add(entry{kind: kindRedeploy, id: rd.PlaceKey, segs: []seg{{"  " + box + text.Sanitize(rd.Label), nil}}})
	}
	if !unfiltered && len(v.Redeploys) > 0 && keptRedeploys == 0 && v.Err == "" {
		lines = lines[:redeployAt]
	}
	if v.Err != "" {
		note("! "+firstLine(v.Err), &th.Danger)
	}
	if s.filter.Active() && len(rows) == 0 {
		note("nothing matches the filter", &th.Muted)
	}
	return lines, rows, total
}

// coverageText is "<label> · C of T places · K keep the old value"; the
// label sanitized.
func coverageText(c rotation.Coverage) string {
	l := text.Sanitize(c.Label) + " · " + strconv.Itoa(c.Changed) + " of " + strconv.Itoa(c.Total) + " places"
	switch keep := c.Total - c.Changed; {
	case keep == 1:
		l += " · 1 keeps the old value"
	case keep > 1:
		l += " · " + strconv.Itoa(keep) + " keep the old value"
	}
	return l
}

// partlyText is the warning above the apply confirm naming the partly
// rotated groups, "" with none.
func partlyText(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "! 1 group partly rotated"
	}
	return "! " + strconv.Itoa(n) + " groups partly rotated"
}

func toneOf(ctx *tui.Ctx, tone string) *lipgloss.Style {
	switch tone {
	case "danger":
		return &ctx.Theme.Danger
	case "warn":
		return &ctx.Theme.Warn
	case "new":
		return &ctx.Theme.New
	}
	return &ctx.Theme.Muted
}

// groupLabel turns "project/env/service" into "project/env · service".
func groupLabel(l string) string {
	l = text.Sanitize(l)
	if i := strings.LastIndex(l, "/"); i >= 0 {
		return l[:i] + " · " + l[i+1:]
	}
	return l
}

// invalidText is how an invalid change's reason is shown: "invalid: " and its
// first line, sanitized; "" for a valid change.
func invalidText(reason string) string {
	if reason == "" {
		return ""
	}
	return "invalid: " + firstLine(reason)
}

// applicable reports whether a change row can go into a run: unlocked and
// valid.
func (e entry) applicable() bool { return e.kind == kindChange && !e.locked && !e.invalid }

// firstLine is the first line of an error, sanitized.
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	return text.Sanitize(s)
}

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if handled, cmd := s.stalled.Update(ctx, msg); handled {
		return s, cmd
	}
	switch m := msg.(type) {
	case components.CancelMsg:
		if s.confirm == nil {
			return s, nil
		}
		s.confirm, s.ids, s.partly = nil, nil, ""
		return s, nil
	case components.ConfirmedMsg:
		if s.confirm == nil {
			return s, nil
		}
		return s.confirmed(ctx)
	case tea.KeyMsg:
		if s.confirm != nil {
			return s, s.confirm.Update(m)
		}
		if h, cmd := s.filter.Update(ctx, m); h {
			return s, cmd
		}
		return s.key(ctx, m)
	}
	return s, nil
}

func (s *screen) key(ctx *tui.Ctx, k tea.KeyMsg) (tui.Screen, tea.Cmd) {
	_, rows, _ := s.layout(ctx, false)
	// Forget selections of changes that are gone or now locked. The filter
	// only hides rows, so liveness is read from the unfiltered set.
	_, allRows, _ := s.layout(ctx, true)
	s.prune(allRows)
	move := func(d int) {
		s.cursor = max(0, min(s.cursor+d, len(rows)-1))
	}
	h := max(1, ctx.Height-tui.ChromeRows)
	switch k.Type {
	case tea.KeyUp:
		move(-1)
		return s, nil
	case tea.KeyDown:
		move(1)
		return s, nil
	case tea.KeyPgUp:
		move(-h)
		return s, nil
	case tea.KeyPgDown:
		move(h)
		return s, nil
	case tea.KeySpace:
		return s.toggleSelect(rows)
	case tea.KeyEnter:
		if r, ok := s.at(rows); ok && r.kind == kindRun {
			return s, tui.Emit(tui.OpenMsg{Name: "run", Args: []any{r.id}})
		}
		s.stalled.Enter(ctx) // off a run row, enter continues a stalled run
		return s, nil
	case tea.KeyRunes:
	default:
		return s, nil
	}
	switch string(k.Runes) {
	case "j":
		move(1)
	case "k":
		move(-1)
	case " ":
		return s.toggleSelect(rows)
	case "a":
		return s.startApply(ctx, rows, allRows)
	case "x":
		r, ok := s.at(rows)
		if !ok || r.kind != kindChange {
			return s, nil
		}
		if r.locked {
			return s, tui.Flash('✗', "in a run")
		}
		return s, tui.Emit(tui.RemoveChangeMsg{ID: r.id})
	case "t":
		if r, ok := s.at(rows); ok && r.kind == kindRedeploy {
			return s, tui.Emit(tui.ToggleRedeployMsg{PlaceKey: r.id})
		}
	}
	return s, nil
}

// confirmed applies what the confirmation was opened for, re-checked against
// the current change set: ids that were removed or locked since are dropped.
func (s *screen) confirmed(ctx *tui.Ctx) (tui.Screen, tea.Cmd) {
	want := s.ids
	s.confirm, s.ids, s.partly = nil, nil, ""
	_, rows, _ := s.layout(ctx, true)
	live := map[string]bool{}
	var liveIDs []string
	for _, r := range rows {
		if r.applicable() {
			live[r.id] = true
			liveIDs = append(liveIDs, r.id)
		}
	}
	if want == nil { // apply all
		if len(liveIDs) == 0 {
			return s, tui.Flash('!', "nothing to apply")
		}
		s.selected = map[string]bool{}
		return s, tui.Emit(tui.ApplyMsg{})
	}
	var ids []string
	for _, id := range want {
		if live[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		s.selected = map[string]bool{}
		return s, tui.Flash('!', "nothing left to apply: the selected changes moved into a run or were removed")
	}
	s.selected = map[string]bool{}
	return s, tui.Emit(tui.ApplyMsg{IDs: ids})
}

// prune forgets the selected changes that are not among the applicable rows
// (rows unfiltered).
func (s *screen) prune(rows []entry) {
	live := map[string]bool{}
	for _, r := range rows {
		if r.applicable() {
			live[r.id] = true
		}
	}
	for id := range s.selected {
		if !live[id] {
			delete(s.selected, id)
		}
	}
}

func (s *screen) at(rows []entry) (entry, bool) {
	if len(rows) == 0 {
		return entry{}, false
	}
	// a filter may have left the cursor past the last shown row
	s.cursor = max(0, min(s.cursor, len(rows)-1))
	return rows[s.cursor], true
}

func (s *screen) toggleSelect(rows []entry) (tui.Screen, tea.Cmd) {
	r, ok := s.at(rows)
	if !ok || r.kind != kindChange {
		return s, nil
	}
	if r.locked {
		return s, tui.Flash('!', "locked: in a run")
	}
	if r.invalid {
		return s, tui.Flash('✗', "invalid: it cannot be applied · x removes it")
	}
	if s.selected[r.id] {
		delete(s.selected, r.id)
	} else {
		s.selected[r.id] = true
	}
	return s, nil
}

// partial is the number of partly rotated groups (the view's coverage) that
// changes among ids (nil: every applicable row of all) belong to.
func partial(ctx *tui.Ctx, ids []string, all []entry) int {
	if ctx.Views == nil || ctx.CS == nil {
		return 0
	}
	cov := ctx.Views.ChangeSet(ctx.CS, selectedEnvIDs(ctx)).Coverage
	if len(cov) == 0 {
		return 0
	}
	if ids == nil {
		for _, r := range all {
			if r.applicable() {
				ids = append(ids, r.id)
			}
		}
	}
	applied := make(map[string]bool, len(ids))
	for _, id := range ids {
		applied[id] = true
	}
	groups := map[string]bool{}
	for _, c := range ctx.CS.Changes {
		if applied[c.ID] {
			groups[c.Group] = true
		}
	}
	n := 0
	for _, c := range cov {
		if groups[c.Group] {
			n++
		}
	}
	return n
}

// startApply opens the apply confirmation. Selected changes count even when
// the filter hides them; with none selected, the shown changes are applied.
// Invalid changes are never applied: the prompt says how many are left out.
func (s *screen) startApply(ctx *tui.Ctx, shown, all []entry) (tui.Screen, tea.Cmd) {
	var chosen, visible []string
	unlocked, invalidAll, invalidShown := 0, 0, 0
	for _, r := range all {
		if r.kind != kindChange || r.locked {
			continue
		}
		if r.invalid {
			invalidAll++
			continue
		}
		unlocked++
		if s.selected[r.id] {
			chosen = append(chosen, r.id)
		}
	}
	for _, r := range shown {
		switch {
		case r.applicable():
			visible = append(visible, r.id)
		case r.kind == kindChange && !r.locked && r.invalid:
			invalidShown++
		}
	}
	if unlocked == 0 && invalidAll > 0 {
		return s, tui.Flash('!', "nothing to apply: every change left is invalid · x removes it")
	}
	if unlocked == 0 || (len(chosen) == 0 && len(visible) == 0) {
		return s, tui.Flash('!', "nothing to apply")
	}
	leftOut := func(n int) string {
		if n == 0 {
			return ""
		}
		return " · " + strconv.Itoa(n) + " invalid left out"
	}
	var prompt string
	switch {
	case len(chosen) > 0:
		prompt = "apply " + strconv.Itoa(len(chosen)) + " selected"
		s.ids = slices.Clone(chosen)
	case s.filter.Active():
		prompt = "apply " + plural(len(visible)) + " shown" + leftOut(invalidShown)
		s.ids = slices.Clone(visible)
	default:
		prompt = "apply all " + plural(unlocked) + leftOut(invalidAll)
		s.ids = nil
	}
	s.confirm = &components.Confirm{Prompt: prompt, Word: "apply"}
	s.partly = partlyText(partial(ctx, s.ids, all))
	return s, nil
}

func plural(n int) string {
	if n == 1 {
		return "1 change"
	}
	return strconv.Itoa(n) + " changes"
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	lines, rows, total := s.layout(ctx, false)
	s.cursor = max(0, min(s.cursor, len(rows)-1))
	var top, bottom []string
	if f := s.filter.View(width, len(rows), total); f != "" {
		top = append(top, f)
	}
	// the stalled-password banner; its question
	// shows below it
	top = append(top, s.stalled.Lines(ctx, width)...)
	if s.confirm != nil {
		if s.partly != "" {
			bottom = append(bottom, ctx.Theme.Warn.Render(ansi.Truncate(s.partly, width, "…")))
		}
		bottom = append(bottom, s.confirm.View(width))
	}
	listH := max(height-len(top)-len(bottom), 0)

	cur := -1
	for i, l := range lines {
		if l.row >= 0 && l.row == s.cursor {
			cur = i
		}
	}
	if cur >= 0 {
		if cur < s.off {
			s.off = cur
		}
		if cur >= s.off+listH {
			s.off = cur - listH + 1
		}
	}
	s.off = max(0, min(s.off, max(0, len(lines)-listH)))

	out := top
	for i := s.off; i < min(len(lines), s.off+listH); i++ {
		out = append(out, s.draw(ctx, lines[i], i == cur, width))
	}
	out = append(out, bottom...)
	if len(out) > height { // keep the confirm line: it is the last one
		out = append(out[:max(height-len(bottom), 0)], bottom...)
		out = out[max(len(out)-max(height, 0), 0):]
	}
	return strings.Join(out, "\n")
}

func (s *screen) draw(ctx *tui.Ctx, e entry, selected bool, width int) string {
	var plain, styled strings.Builder
	for _, sg := range e.segs {
		plain.WriteString(sg.text)
		if sg.style != nil {
			styled.WriteString(sg.style.Render(sg.text))
		} else {
			styled.WriteString(sg.text)
		}
	}
	if selected {
		return ctx.Theme.Selected.Render(ansi.Truncate(plain.String(), width, "…"))
	}
	return ansi.Truncate(styled.String(), width, "…")
}

// selectedEnvIDs are the selected environments: what coverage counts.
func selectedEnvIDs(ctx *tui.Ctx) []string {
	if ctx.Store == nil {
		return nil
	}
	return loaders.EnvIDs(loaders.Targets(ctx.Store.Projects(), ctx.Settings))
}

// copyText is "⇉ from <env> · <service>" for a change that copies another
// place's value, sanitized; "" for any other.
func copyText(ctx *tui.Ctx, c changeset.Change) string {
	loc, ok := kinds.CopySource(c)
	if !ok || ctx.Store == nil {
		return ""
	}
	parts := strings.SplitN(loc, "/", 3)
	if len(parts) != 3 {
		return ""
	}
	svc := parts[1]
	if svc == "shared" {
		svc = ""
	}
	return text.Sanitize("⇉ from " + ctx.EnvName(parts[0]) + " · " + ctx.Store.ServiceName(svc))
}

// Package rotate is the Rotate secrets screen: the rotation rows of the
// selected environments (view.Rotate) by section, each with its decision
// state, and a detail panel for the cursor's row.
//
// Space generates new values for a GEN row's undecided places, one value
// per old value, reusing the value a pending change of the group already
// carries; on a fully decided row it undoes the row's Rotate decisions.
// Places in read-only environments and on databases are never decided
// here. A database password row (password.go) takes
// space like a GEN row: a new password of 32 letters and digits, or undo;
// c on a stalled one continues its run after a question; its places are
// read only. Values never render.
//
// The places screen (BuildPlaces, "rotate.places") decides a row per place
// and name; a on the list adds a variable to places of the write
// environments.
package rotate

import (
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

// producer is the Origin.Producer of every change Rotate makes.
const producer = "rotate"

type screen struct {
	// want is the fingerprint Build was given, resolved once in Init.
	want   string
	hidden bool
	// cur is the cursor's row by key; curIdx the index it last had, used
	// when that row is gone.
	cur    string
	curIdx int
	// offset is the first list line drawn.
	offset int
	filter *components.Filter
	// confirm asks before the first decision on an off-by-default row
	// (confirmRow).
	confirm    *components.Confirm
	confirmRow string
	// resume: the question asks to continue confirmRow's stalled run;
	// revert: to revert its database password.
	resume, revert bool
	// planner holds the values this screen generated (see planKey). Undo
	// of a row forgets the row's keys.
	planner
	// add is the add flow while it is up; nil otherwise. addSeq tells its
	// file read in flight from older ones, across adds.
	add    *adding
	addSeq int
	// stalled: the cursor's row, when last drawn, is a stalled password
	// row, whose run c continues; revertable: R reverts it.
	stalled, revertable bool
}

// New returns the Rotate secrets screen.
func New() tui.Screen { return newScreen() }

func newScreen() *screen {
	return &screen{filter: components.NewFilter("rotate"), planner: newPlanner()}
}

// Build is the registry constructor: no argument, or a fingerprint (string)
// to start on the row whose groups hold it. When that row is hidden, or no
// row holds it, hidden rows are shown (no row: Init flashes so). It returns
// nil for other arguments.
func Build(args ...any) tui.Screen {
	switch len(args) {
	case 0:
		return newScreen()
	case 1:
		if fp, ok := args[0].(string); ok {
			s := newScreen()
			s.want = fp
			return s
		}
	}
	return nil
}

func (s *screen) Title() string { return "Rotate secrets" }

func (s *screen) Init(ctx *tui.Ctx) tea.Cmd {
	s.filter.Load(ctx)
	if s.want == "" {
		return nil
	}
	fp := s.want
	s.want = ""
	for _, r := range s.rows(ctx) {
		for _, g := range r.Groups {
			if g.Key == fp {
				s.cur, s.hidden = r.Key, r.Hidden
				return nil
			}
		}
	}
	s.hidden = true
	return tui.Flash('!', "this value is not in a rotation row")
}

// Capturing reports whether the filter or the question takes every key.
func (s *screen) Capturing() bool { return s.add != nil || s.confirm != nil || s.filter.Editing() }

// Note is the leak time and the row count.
func (s *screen) Note(ctx *tui.Ctx) string {
	rows := " · " + strconv.Itoa(len(s.visible(ctx))) + " rows"
	notes := []string{"no leak time · o settings" + rows, "no leak time" + rows, "no leak time"}
	if t := since(ctx); !t.IsZero() {
		notes = []string{"leak " + stamp(t) + rows, "leak " + stamp(t)}
	}
	// the frame header holds "railway-manager <title>  <note>"
	room := ctx.Width - tui.ChromeCols - ansi.StringWidth("railway-manager "+s.Title()) - 1
	for _, n := range notes {
		if ansi.StringWidth(n) <= room {
			return n
		}
	}
	return notes[len(notes)-1]
}

// Notice has no standing footer message.
func (s *screen) Notice(*tui.Ctx) tui.FlashMsg { return tui.FlashMsg{} }

func (s *screen) Keys() []tui.KeyHint {
	switch {
	case s.add != nil:
		return s.add.keys()
	case s.confirm != nil:
		yes := "generate"
		switch {
		case s.resume:
			yes = "continue"
		case s.revert:
			yes = "revert"
		}
		return []tui.KeyHint{
			{Key: "y", Label: yes, Always: true},
			{Key: "n", Label: "cancel", Always: true},
		}
	case s.filter.Editing():
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	h := "hidden"
	if s.hidden {
		h = "hide hidden"
	}
	hints := []tui.KeyHint{
		{Key: "enter", Label: "places", Rank: 1},
		{Key: "space", Label: "generate", Rank: 2},
		{Key: "A", Label: "apply", Rank: 3},
		{Key: "h", Label: h, Rank: 4},
		{Key: "a", Label: "add", Rank: 5},
		{Key: "/", Label: "filter", Rank: 6, Optional: true},
		{Key: "r", Label: "refresh", Rank: 7, Optional: true},
		{Key: "j/k", Label: "move", Rank: 8, Optional: true},
		{Key: "pgup/pgdn", Label: "page", Rank: 9, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
	if s.stalled {
		// Continue comes first on a stalled row.
		for i := range hints {
			if hints[i].Rank > 0 {
				hints[i].Rank++
			}
		}
		lead := []tui.KeyHint{{Key: "c", Label: "continue", Rank: 1}}
		if s.revertable {
			for i := range hints {
				if hints[i].Rank > 0 {
					hints[i].Rank++
				}
			}
			lead = append(lead, tui.KeyHint{Key: "R", Label: "revert", Rank: 2})
		}
		hints = append(lead, hints...)
	}
	return hints
}

// since is the leak time of the settings; zero when unset or unreadable.
func since(ctx *tui.Ctx) time.Time {
	if ctx.Settings == nil || ctx.Settings.Since == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, ctx.Settings.Since)
	if err != nil {
		return time.Time{}
	}
	return t
}

// stamp is a time as shown here: "MM-DD HH:MM", local, as the leak time is
// typed in Settings.
func stamp(t time.Time) string { return t.In(time.Local).Format("01-02 15:04") }

// envIDs is the environments the inventory reads (all of a selected
// project's when none of it is chosen), as Variables lists them.
func envIDs(ctx *tui.Ctx) []string {
	var out []string
	for _, t := range loaders.Targets(ctx.Store.Projects(), ctx.Settings) {
		for _, e := range t.Environments {
			out = append(out, e.ID)
		}
	}
	return out
}

// allEnvIDs is every environment of the selected projects: what database
// detection reads. They are never shown.
func allEnvIDs(ctx *tui.Ctx) []string {
	return loaders.EnvIDs(loaders.LoadTargets(ctx.Store.Projects(), ctx.Settings))
}

// rows is every rotation row, hidden ones included.
func (s *screen) rows(ctx *tui.Ctx) []view.RotateRow { return rowsOf(ctx) }

// visible is the rows listed before the filter: hidden ones only with h.
func (s *screen) visible(ctx *tui.Ctx) []view.RotateRow {
	all := s.rows(ctx)
	if s.hidden {
		return all
	}
	out := make([]view.RotateRow, 0, len(all))
	for _, r := range all {
		if !r.Hidden {
			out = append(out, r)
		}
	}
	return out
}

// shown is the visible rows the filter keeps.
func (s *screen) shown(ctx *tui.Ctx) []view.RotateRow {
	vis := s.visible(ctx)
	if !s.filter.Active() {
		return vis
	}
	out := make([]view.RotateRow, 0, len(vis))
	for _, r := range vis {
		if s.filter.Match(r.Label) {
			out = append(out, r)
		}
	}
	return out
}

// locate finds the cursor's row in rows by key, else at its last index
// (clamped), and follows it; -1 when rows is empty.
func (s *screen) locate(rows []view.RotateRow) int {
	if len(rows) == 0 {
		return -1
	}
	idx := -1
	for i, r := range rows {
		if r.Key == s.cur {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = max(0, min(s.curIdx, len(rows)-1))
	}
	s.cur, s.curIdx = rows[idx].Key, idx
	return idx
}

func (s *screen) move(rows []view.RotateRow, d int) {
	i := s.locate(rows)
	if i < 0 {
		return
	}
	i = max(0, min(i+d, len(rows)-1))
	s.cur, s.curIdx = rows[i].Key, i
}

// page is how far PgUp and PgDn move: about the list's height.
func page(ctx *tui.Ctx) int { return max(1, (ctx.Height-tui.ChromeRows)*2/3-2) }

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if ctx.Store == nil || ctx.Views == nil {
		return s, nil
	}
	if s.add != nil {
		return s, s.addUpdate(ctx, msg)
	}
	switch m := msg.(type) {
	case components.ConfirmedMsg:
		key, resume, revert := s.confirmRow, s.resume, s.revert
		s.closeConfirm()
		for _, r := range s.rows(ctx) {
			if r.Key != key {
				continue
			}
			if revert {
				if !r.Revert || r.Run == nil {
					return s, tui.Flash('!', "the row changed · R again")
				}
				return s, tui.Emit(tui.RevertMsg{RunID: r.Run.ID, Row: *r.Password})
			}
			if resume {
				if !r.Stalled || r.Run == nil {
					return s, tui.Flash('!', "the row changed · c again")
				}
				return s, tui.Emit(tui.ResumeMsg{RunID: r.Run.ID})
			}
			return s, s.space(ctx, r, true)
		}
		switch {
		case resume:
			return s, tui.Flash('!', "the row changed · c again")
		case revert:
			return s, tui.Flash('!', "the row changed · R again")
		}
		return s, tui.Flash('!', "the row changed since space · space again")
	case components.CancelMsg:
		s.closeConfirm()
	case tea.KeyMsg:
		if s.confirm != nil {
			return s, s.confirm.Update(m)
		}
		if h, cmd := s.filter.Update(ctx, m); h {
			return s, cmd
		}
		return s, s.key(ctx, m)
	}
	return s, nil
}

func (s *screen) closeConfirm() {
	s.confirm, s.confirmRow, s.resume, s.revert = nil, "", false, false
}

func (s *screen) key(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	rows := s.shown(ctx)
	switch k.Type {
	case tea.KeyUp:
		s.move(rows, -1)
		return nil
	case tea.KeyDown:
		s.move(rows, 1)
		return nil
	case tea.KeyPgUp:
		s.move(rows, -page(ctx))
		return nil
	case tea.KeyPgDown:
		s.move(rows, page(ctx))
		return nil
	}
	key := k.String()
	switch key {
	case "j":
		s.move(rows, 1)
		return nil
	case "k":
		s.move(rows, -1)
		return nil
	case "h":
		s.hidden = !s.hidden
		return nil
	case "r":
		return tui.Emit(tui.RefreshMsg{})
	case "a":
		s.add = newAdding()
		return nil
	case "A":
		return s.applyPreset(ctx)
	}
	idx := s.locate(rows)
	if idx < 0 {
		return nil
	}
	switch key {
	case "enter":
		return openPlaces(rows[idx])
	case "c":
		r := rows[idx]
		if pwRow(r) && r.Run != nil && r.Run.Elsewhere {
			return tui.Flash('✗', elsewhereText)
		}
		if !pwRow(r) || !r.Stalled || r.Run == nil {
			return nil
		}
		s.confirm = &components.Confirm{Prompt: "continue the stalled rotation of " + r.Label + "?"}
		s.confirmRow, s.resume = r.Key, true
		return nil
	case "R":
		return s.askRevert(rows[idx])
	case " ":
		return s.space(ctx, rows[idx], false)
	}
	return nil
}

// askRevert asks before reverting the database password of a stalled root
// row whose new password no place holds; once a
// place holds it only continue is left.
func (s *screen) askRevert(r view.RotateRow) tea.Cmd {
	switch {
	case !pwRow(r) || r.Run == nil:
		return nil
	case r.Run.Elsewhere:
		return tui.Flash('✗', elsewhereText)
	case !r.Stalled:
		return nil
	case !r.Password.Root || r.Password.KeyFile:
		return tui.Flash('✗', "only a root password is reverted · c continues")
	case !r.Revert:
		return tui.Flash('✗', "a place holds the new password · c continues")
	}
	s.confirm = &components.Confirm{Prompt: "set " + r.Label + " back to the password in Railway now?"}
	s.confirmRow, s.revert = r.Key, true
	return nil
}

// openPlaces opens the row's places screen.
func openPlaces(r view.RotateRow) tea.Cmd {
	return tui.Emit(tui.OpenMsg{Name: "rotate.places", Args: []any{r.Key}})
}

// applyPreset opens the Change set with every Rotate change selected.
func (s *screen) applyPreset(ctx *tui.Ctx) tea.Cmd {
	var ids []string
	for _, c := range pending(ctx) {
		if strings.HasPrefix(c.Group, "rotate:") || strings.HasPrefix(c.Group, changeset.PasswordGroupPrefix) {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return tui.Flash('!', "no rotate changes yet · space generates")
	}
	return tui.Emit(tui.OpenMsg{Name: "changeset", Args: []any{"select", ids}})
}

// space decides a GEN row: it generates for the undecided decidable lines
// outside groups rotated everywhere after the leak, or undoes the row's
// Rotate decisions when none is left undecided. ASK
// and added rows open their places; DB, URL and problem rows are refused.
// asked: the off-by-default question was answered yes.
func (s *screen) space(ctx *tui.Ctx, r view.RotateRow, asked bool) tea.Cmd {
	switch {
	case r.Section == rotation.SectionAdded:
		return openPlaces(r)
	case pwRow(r):
		return s.spacePassword(ctx, r, asked)
	case r.Problem != "":
		return tui.Flash('!', text.Sanitize(r.Problem))
	case r.Class != rotation.ClassGen:
		return openPlaces(r)
	case r.Decidable == 0:
		return whyNot(ctx, r)
	}
	// Lines of a group rotated everywhere after the leak already hold a new
	// value: generating would split it from what the other places of that
	// group run with, so space leaves them (c on the place screen copies it).
	var todo []view.RotatePlace
	fresh := false
	for _, p := range r.Places {
		if open(r, p) {
			todo = append(todo, p)
		} else if decidable(p) && p.Pending == nil {
			fresh = true // a line of a rotated group
		}
	}
	if len(todo) == 0 {
		if fresh && !slices.ContainsFunc(r.Places, func(p view.RotatePlace) bool { return isRotate(p.Pending) }) {
			return tui.Flash('!', "already rotated after the leak · enter shows the places")
		}
		return s.undo(r)
	}
	if r.OffByDefault && r.Decided == 0 && !asked {
		prompt := "generate new values?"
		if r.Warning != "" {
			prompt = "! " + r.Warning + " · generate?"
		}
		s.confirm, s.confirmRow = &components.Confirm{Prompt: prompt}, r.Key
		return nil
	}
	if fresh {
		return s.generateDone(ctx, r, todo, '!', "some places are already rotated · c in the row copies from them")
	}
	return s.generate(ctx, r, todo)
}

// spacePassword decides a password row like a GEN row: a new password of
// 32 letters and digits, or, when one is decided, undoes it (the password
// change alone; the root removes its automatic rewrites). A KEYFILE asks
// first. asked: the question was answered yes.
func (s *screen) spacePassword(ctx *tui.Ctx, r view.RotateRow, asked bool) tea.Cmd {
	if cmd := pwRowRefusal(r); cmd != nil {
		return cmd
	}
	if r.Change != nil {
		return s.undoPassword(r)
	}
	if cmd := pwLineRefusal(ctx, r, pwLines(r)[0], "g"); cmd != nil {
		return cmd
	}
	if r.Password.KeyFile && !asked {
		// The panel above shows the row's warning in full.
		s.confirm, s.confirmRow = &components.Confirm{Prompt: keyFilePrompt}, r.Key
		return nil
	}
	v, err := s.passwordValue(ctx, r)
	if err != nil {
		return tui.Flash('✗', "cannot generate: "+err.Error())
	}
	return decidePassword(ctx, r, kinds.SourceGenerate, v)
}

// undo removes the row's Rotate changes; a change held by a run stops it.
func (s *screen) undo(r view.RotateRow) tea.Cmd {
	var removes []string
	for _, p := range r.Places {
		if !isRotate(p.Pending) {
			continue
		}
		if p.Pending.RunID != "" {
			return tui.Flash('✗', "in run "+p.Pending.RunID)
		}
		removes = append(removes, p.Pending.ID)
	}
	if len(removes) == 0 {
		return tui.Flash('!', "every place has a change from elsewhere · enter shows them")
	}
	for _, p := range r.Places {
		delete(s.planned, planKey(r, p))
	}
	return changes(removes, nil, '✓', placesText(len(removes))+": decision undone")
}

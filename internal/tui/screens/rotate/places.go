package rotate

import (
	"os"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

// placesMode is what takes the keys on the places screen.
type placesMode int

const (
	modeList    placesMode = iota
	modeMask               // a masked paste
	modePick               // the file picker
	modeRead               // a file read in flight
	modeChoose             // choosing a copy source
	modeConfirm            // the off-by-default question
)

// places is the places screen of a rotation row: a
// line per place and name with its decision. Targets are the selected
// lines, else the cursor's. Every action resolves its targets again
// against the rows as they are when it is submitted. Values never render,
// except the cursor line's current value on v.
type places struct {
	rowKey, label string
	// cur is the cursor's line by location id; curIdx the index it last
	// had, used when that line is gone.
	cur    string
	curIdx int
	list   components.List
	// sel is the selected lines by location id; lines gone from the row
	// leave it.
	sel map[string]bool

	mode placesMode
	// action is the key that opened the input, targets its lines.
	action  string
	targets []string
	mask    *components.MaskedInput
	picker  *components.FilePicker
	confirm *components.Confirm
	// seq tells the file read in flight from older ones.
	seq int
	// revealed is the location id whose value shows; "" for none. revealedAt
	// is the data it was revealed in: a refresh or a change set update hides
	// it, as a key does.
	revealed   string
	revealedAt [2]uint64
	// cursorRow is the cursor's row in the list as last drawn.
	cursorRow int
	// pw, stalled: the row, when last read, is a password row, and a
	// stalled one (for the key hints).
	pw, stalled bool
	planner
}

// BuildPlaces is the registry constructor of "rotate.places": one
// argument, the row key. It returns nil for other arguments.
func BuildPlaces(args ...any) tui.Screen {
	if len(args) != 1 {
		return nil
	}
	k, ok := args[0].(string)
	if !ok || k == "" {
		return nil
	}
	return &places{rowKey: k, sel: map[string]bool{}, planner: newPlanner()}
}

func (s *places) Init(ctx *tui.Ctx) tea.Cmd {
	s.row(ctx)
	return nil
}

// row is the screen's row now; it keeps the label for the title.
func (s *places) row(ctx *tui.Ctx) (view.RotateRow, bool) {
	r, ok := rowByKey(ctx, s.rowKey)
	if ok {
		s.label = r.Label
		s.pw, s.stalled = pwRow(r), r.Stalled
	}
	return r, ok
}

func (s *places) Title() string {
	if s.label == "" {
		return "Rotate"
	}
	return "Rotate · " + text.Sanitize(s.label)
}

// Note is "N places · K values".
func (s *places) Note(ctx *tui.Ctx) string {
	r, ok := s.row(ctx)
	if !ok {
		return ""
	}
	n := placesText(len(r.Places))
	values := "added"
	switch {
	case pwRow(r):
		places := 0
		for _, p := range r.Places {
			if !shownOnly(r, p) {
				places++
			}
		}
		n, values = placesText(places), "1 database"
	case r.Section == rotation.SectionAdded:
	case len(r.Groups) == 1:
		values = "1 value"
	default:
		values = strconv.Itoa(len(r.Groups)) + " values"
	}
	room := ctx.Width - tui.ChromeCols - ansi.StringWidth("railway-manager "+s.Title()) - 1
	if full := n + " · " + values; ansi.StringWidth(full) <= room {
		return full
	}
	return n
}

// Notice has no standing footer message.
func (s *places) Notice(*tui.Ctx) tui.FlashMsg { return tui.FlashMsg{} }

// Capturing reports whether an input, the picker, choose-mode or the
// question takes every key.
func (s *places) Capturing() bool { return s.mode != modeList }

// Claims takes the uppercase letters, which select a letter's lines here
// (C would open the Change set otherwise).
func (s *places) Claims(key string) bool {
	return s.mode == modeList && len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z'
}

// Suspend hides a revealed value when another screen covers this one.
func (s *places) Suspend(*tui.Ctx) { s.revealed = "" }

func (s *places) Keys() []tui.KeyHint {
	switch s.mode {
	case modeConfirm:
		return []tui.KeyHint{{Key: "y", Label: "continue", Always: true}, {Key: "n", Label: "cancel", Always: true}}
	case modeMask:
		return []tui.KeyHint{{Key: "enter", Label: "confirm", Always: true}, {Key: "esc", Label: "cancel", Always: true}}
	case modePick:
		return []tui.KeyHint{{Key: "enter", Label: "pick", Always: true}, {Key: "esc", Label: "cancel", Always: true}}
	case modeRead:
		return []tui.KeyHint{{Key: "esc", Label: "cancel", Always: true}}
	case modeChoose:
		return []tui.KeyHint{
			{Key: "enter", Label: "copy from here", Always: true},
			{Key: "↑/↓", Label: "move", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	}
	switch {
	case s.pw && s.stalled:
		return []tui.KeyHint{
			{Key: "↑/↓", Label: "move", Rank: 1},
			{Key: "v", Label: "reveal", Rank: 2, Optional: true},
			{Key: "esc", Label: "back", Always: true},
		}
	case s.pw:
		return []tui.KeyHint{
			{Key: "g", Label: "generate", Rank: 1},
			{Key: "p", Label: "paste", Rank: 2},
			{Key: "o", Label: "by hand", Rank: 3},
			{Key: "k", Label: "keep", Rank: 4},
			{Key: "f", Label: "file", Rank: 5},
			{Key: "c", Label: "copy", Rank: 6},
			{Key: "x", Label: "delete", Rank: 7},
			{Key: "u", Label: "undo", Rank: 8},
			{Key: "space", Label: "select", Rank: 9},
			{Key: "v", Label: "reveal", Rank: 10, Optional: true},
			{Key: "↑/↓", Label: "move", Rank: 11, Optional: true},
			{Key: "esc", Label: "back", Always: true},
		}
	}
	return []tui.KeyHint{
		{Key: "g", Label: "generate", Rank: 1},
		{Key: "p", Label: "paste", Rank: 2},
		{Key: "f", Label: "file", Rank: 3},
		{Key: "c", Label: "copy", Rank: 4},
		{Key: "x", Label: "delete", Rank: 5},
		{Key: "u", Label: "undo", Rank: 6},
		{Key: "space", Label: "select", Rank: 7},
		{Key: "A–Z", Label: "select letter", Rank: 8, Optional: true},
		{Key: "v", Label: "reveal", Rank: 9, Optional: true},
		{Key: "↑/↓", Label: "move", Rank: 10, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
}

// locate finds the cursor's line by id, else at its last index (clamped),
// and follows it; -1 when the row has no line.
func (s *places) locate(r view.RotateRow) int {
	ls := items(r)
	if len(ls) == 0 {
		return -1
	}
	idx := slices.IndexFunc(ls, func(p view.RotatePlace) bool { return p.Loc.ID == s.cur })
	if idx < 0 {
		idx = max(0, min(s.curIdx, len(ls)-1))
	}
	// A line shown only never takes the cursor: the nearest one above
	// does (a password row's first line never is one).
	for idx > 0 && shownOnly(r, ls[idx]) {
		idx--
	}
	s.cur, s.curIdx = ls[idx].Loc.ID, idx
	return idx
}

// move moves the cursor by d lines, over the lines shown only.
func (s *places) move(r view.RotateRow, d int) {
	i := s.locate(r)
	if i < 0 {
		return
	}
	ls := items(r)
	to := max(0, min(i+d, len(ls)-1))
	step := 1
	if to < i {
		step = -1
	}
	for to != i && shownOnly(r, ls[to]) {
		to -= step
	}
	if to == i && d != 0 {
		// Nothing to stop on that way: past the lines shown only.
		for j := i + d; j >= 0 && j < len(ls); j += step {
			if !shownOnly(r, ls[j]) {
				to = j
				break
			}
		}
	}
	s.cur, s.curIdx = ls[to].Loc.ID, to
}

// sync drops selected lines that are gone from the row.
func (s *places) sync(r view.RotateRow) {
	for id := range s.sel {
		if !slices.ContainsFunc(items(r), func(p view.RotatePlace) bool { return p.Loc.ID == id }) {
			delete(s.sel, id)
		}
	}
}

// chosen is the target lines: the selected ones in row order, else the
// cursor's.
func (s *places) chosen(r view.RotateRow) []view.RotatePlace {
	var out []view.RotatePlace
	ls := items(r)
	for _, p := range ls {
		if s.sel[p.Loc.ID] {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		if i := s.locate(r); i >= 0 {
			out = append(out, ls[i])
		}
	}
	return out
}

func idsOf(lines []view.RotatePlace) []string {
	out := make([]string, len(lines))
	for i, p := range lines {
		out[i] = p.Loc.ID
	}
	return out
}

// resolve finds the lines ids name in the row as it is now; when the row
// or one of them is gone it flashes the "places changed" text for action.
func (s *places) resolve(ctx *tui.Ctx, ids []string, action string) (view.RotateRow, []view.RotatePlace, tea.Cmd) {
	changed := tui.Flash('!', "the places changed since "+action+" · "+action+" again")
	r, ok := s.row(ctx)
	if !ok {
		return r, nil, changed
	}
	lines := make([]view.RotatePlace, 0, len(ids))
	ls := items(r)
	for _, id := range ids {
		i := slices.IndexFunc(ls, func(p view.RotatePlace) bool { return p.Loc.ID == id })
		if i < 0 {
			return r, nil, changed
		}
		lines = append(lines, ls[i])
	}
	return r, lines, nil
}

// refuse is the first line's refusal; nil when every line can be decided.
func refuse(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace, action string) tea.Cmd {
	if pwRow(r) {
		if cmd := pwRowRefusal(r); cmd != nil {
			return cmd
		}
		if len(lines) > 1 && slices.ContainsFunc(lines, func(p view.RotatePlace) bool { return p.Own }) {
			return tui.Flash('!', "the password line is decided alone · clear the selection")
		}
	}
	for _, p := range lines {
		cmd := lineRefusal(ctx, r, p)
		if pwRow(r) {
			cmd = pwLineRefusal(ctx, r, p, action)
		}
		if cmd != nil {
			return cmd
		}
	}
	return nil
}

func (s *places) close() {
	s.mode, s.action, s.targets = modeList, "", nil
	s.mask, s.picker, s.confirm = nil, nil, nil
}

func (s *places) clearSel() { clear(s.sel) }

func (s *places) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if ctx.Store == nil || ctx.Views == nil {
		return s, nil
	}
	if r, ok := s.row(ctx); ok {
		s.sync(r)
	}
	switch m := msg.(type) {
	case components.SubmitMsg:
		if s.mode != modeMask {
			return s, nil
		}
		action, ids := s.action, s.targets
		s.close()
		return s, s.setValue(ctx, action, ids, m.Value)
	case components.CancelMsg:
		s.close()
	case components.ConfirmedMsg:
		if s.mode != modeConfirm {
			return s, nil
		}
		action, ids := s.action, s.targets
		s.close()
		if action == "u" {
			return s, s.undoConfirmed(ctx, ids)
		}
		return s, s.begin(ctx, action, ids)
	case components.FilePickedMsg:
		if s.mode != modePick {
			return s, nil
		}
		s.picker, s.mode = nil, modeRead
		s.seq++
		return s, readFile(m.Path, s.seq)
	case fileReadMsg:
		if s.mode != modeRead || m.seq != s.seq {
			return s, nil
		}
		action, ids := s.action, s.targets
		s.close()
		if m.err != "" {
			return s, tui.Flash('✗', m.err)
		}
		return s, s.setValue(ctx, action, ids, string(m.value))
	case tea.KeyMsg:
		switch s.mode {
		case modeMask:
			return s, s.mask.Update(m)
		case modePick:
			return s, s.picker.Update(m)
		case modeConfirm:
			return s, s.confirm.Update(m)
		case modeRead:
			if m.Type == tea.KeyEsc || m.Type == tea.KeyCtrlC {
				s.close()
			}
			return s, nil
		case modeChoose:
			return s, s.chooseKey(ctx, m)
		}
		return s, s.key(ctx, m)
	}
	return s, nil
}

func (s *places) key(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	r, ok := s.row(ctx)
	if !ok {
		return nil
	}
	key := k.String()
	if key != "v" {
		s.revealed = "" // any other key hides a revealed value
	}
	if s.moveKey(ctx, r, k) {
		return nil
	}
	if len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z' {
		return s.selectLetter(ctx, r, int(key[0]-'A'))
	}
	if pwRow(r) && r.Stalled {
		switch key {
		case "v":
		case "c":
			return tui.Flash('!', "read-only · c in the list continues")
		default:
			return tui.Flash('✗', stalledText)
		}
	}
	switch key {
	case " ":
		if i := s.locate(r); i >= 0 {
			id := items(r)[i].Loc.ID
			if s.sel[id] {
				delete(s.sel, id)
			} else {
				s.sel[id] = true
			}
		}
	case "g", "p", "f", "c", "x":
		return s.begin(ctx, key, nil)
	case "o", "k":
		if pwRow(r) {
			return s.begin(ctx, key, nil)
		}
	case "u":
		return s.undo(ctx, r)
	case "v":
		return s.reveal(ctx, r)
	}
	return nil
}

// moveKey moves the cursor on the arrows and pages (letters are actions
// here).
func (s *places) moveKey(ctx *tui.Ctx, r view.RotateRow, k tea.KeyMsg) bool {
	switch k.Type {
	case tea.KeyUp:
		s.move(r, -1)
	case tea.KeyDown:
		s.move(r, 1)
	case tea.KeyPgUp:
		s.move(r, -page(ctx))
	case tea.KeyPgDown:
		s.move(r, page(ctx))
	default:
		return false
	}
	return true
}

// selectLetter selects the decidable lines of the letter, or clears them
// when all are selected already.
func (s *places) selectLetter(ctx *tui.Ctx, r view.RotateRow, letter int) tea.Cmd {
	var ids []string
	var blocked *view.RotatePlace
	for _, p := range r.Places {
		if p.Letter != letter {
			continue
		}
		if lineRefusal(ctx, r, p) != nil {
			if blocked == nil {
				blocked = &p
			}
			continue
		}
		ids = append(ids, p.Loc.ID)
	}
	if len(ids) == 0 {
		if blocked != nil {
			return lineRefusal(ctx, r, *blocked)
		}
		return nil
	}
	all := true
	for _, id := range ids {
		all = all && s.sel[id]
	}
	for _, id := range ids {
		if all {
			delete(s.sel, id)
		} else {
			s.sel[id] = true
		}
	}
	return nil
}

// begin starts g, p, f, c or x on the targets: the chosen lines, or ids
// (resolved again) after the off-by-default question.
func (s *places) begin(ctx *tui.Ctx, action string, ids []string) tea.Cmd {
	var r view.RotateRow
	var lines []view.RotatePlace
	if ids == nil {
		var ok bool
		if r, ok = s.row(ctx); !ok {
			return nil
		}
		lines = s.chosen(r)
	} else {
		var cmd tea.Cmd
		if r, lines, cmd = s.resolve(ctx, ids, action); cmd != nil {
			return cmd
		}
	}
	if len(lines) == 0 {
		return nil
	}
	if cmd := refuse(ctx, r, lines, action); cmd != nil {
		return cmd
	}
	if pwRow(r) {
		return s.pwBegin(ctx, r, lines, action, ids != nil)
	}
	if ids == nil && r.OffByDefault && r.Decided == 0 {
		s.mode, s.action, s.targets = modeConfirm, action, idsOf(lines)
		s.confirm = &components.Confirm{Prompt: "off by default · decide anyway?"}
		return nil
	}
	switch action {
	case "g":
		cmd := s.generate(ctx, r, lines)
		s.clearSel()
		return cmd
	case "p":
		s.mode, s.action, s.targets = modeMask, action, idsOf(lines)
		s.mask = &components.MaskedInput{Label: r.Label + " · " + placesText(len(lines)), Repeat: true}
	case "f":
		home, _ := os.UserHomeDir()
		s.picker = components.NewFilePicker(components.OSDirReader{}, home)
		s.picker.Theme = ctx.Theme
		s.mode, s.action, s.targets = modePick, action, idsOf(lines)
	case "c":
		if since(ctx).IsZero() {
			// Without a leak time no line is known to hold a new value:
			// refuse before a source is chosen (copy checks again).
			return tui.Flash('!', "copy needs a leak time · Settings l")
		}
		s.mode, s.action, s.targets = modeChoose, action, idsOf(lines)
	case "x":
		return s.remove(ctx, r, lines)
	}
	return nil
}

// setValue puts a variable.set of a pasted or read value at the lines ids
// name, resolved again: refused when empty or an old value of the row.
func (s *places) setValue(ctx *tui.Ctx, action string, ids []string, value string) tea.Cmd {
	r, lines, cmd := s.resolve(ctx, ids, action)
	if cmd != nil {
		return cmd
	}
	if cmd := refuse(ctx, r, lines, action); cmd != nil {
		return cmd
	}
	switch {
	case value == "":
		return tui.Flash('✗', "the value is empty")
	case pwRow(r) && lines[0].Own:
		source := kinds.SourcePaste
		if action == "o" {
			source = kinds.SourceHand
		}
		s.clearSel()
		return decidePassword(ctx, r, source, value)
	case pwRow(r) && rotation.HoldsPassword(value, current(ctx, r)):
		return tui.Flash('✗', leakedValue)
	case leaked(r, lines, value):
		return tui.Flash('✗', "that is the current, leaked value")
	}
	now := ctx.Clock()()
	puts := make([]changeset.Change, 0, len(lines))
	for _, p := range lines {
		c, err := kinds.SetVariable(ctx.Store, pending(ctx), p.Loc.Place, p.Loc.Name, value, now)
		if err != nil {
			return tui.Flash('✗', err.Error())
		}
		mark(&c, r, p)
		tie(r, &c)
		puts = append(puts, c)
	}
	s.clearSel()
	return changes(nil, puts, '✓', placesText(len(lines))+": new value")
}

func (s *places) chooseKey(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	r, ok := s.row(ctx)
	switch {
	case k.Type == tea.KeyEsc || k.Type == tea.KeyCtrlC:
		s.close()
	case k.Type == tea.KeyEnter:
		ids := s.targets
		src := s.cur
		s.close()
		return s.copy(ctx, ids, src)
	case ok:
		s.moveKey(ctx, r, k)
	}
	return nil
}

// copy puts at the lines ids name a copy of the source line's current
// value. Refused: no leak time (no line is known to
// hold a new value), a source that is a target, sealed, empty or gone (the
// kind's errors), holding a reference (it would resolve again at the
// target), not changed after the leak, or an old value of the row.
func (s *places) copy(ctx *tui.Ctx, ids []string, srcID string) tea.Cmd {
	r, lines, cmd := s.resolve(ctx, ids, "c")
	if cmd != nil {
		return cmd
	}
	ls := items(r)
	i := slices.IndexFunc(ls, func(p view.RotatePlace) bool { return p.Loc.ID == srcID })
	if i < 0 {
		return tui.Flash('!', "the places changed since c · c again")
	}
	src := ls[i]
	if pwRow(r) && src.Own {
		return tui.Flash('✗', "the password line is not a URL · choose a URL place")
	}
	if cmd := refuse(ctx, r, lines, "c"); cmd != nil {
		return cmd
	}
	if since(ctx).IsZero() {
		// Without a leak time no line is known to hold a new value.
		return tui.Flash('!', "copy needs a leak time · Settings l")
	}
	if slices.Contains(ids, srcID) {
		return tui.Flash('✗', "copy source is one of the chosen places")
	}
	now := ctx.Clock()()
	if planned, ok := plannedAt(ctx, src); ok {
		return s.copyPlanned(ctx, r, lines, src, planned)
	}
	puts := make([]changeset.Change, 0, len(lines))
	for _, p := range lines {
		c, err := kinds.CopyVariable(ctx.Store, pending(ctx), p.Loc.Place, p.Loc.Name, src.Loc.Place, src.Loc.Name, now)
		if err != nil {
			return tui.Flash('✗', err.Error())
		}
		mark(&c, r, p)
		tie(r, &c)
		puts = append(puts, c)
	}
	v := ctx.Store.Variable(srcID)
	switch {
	case v == nil:
		return tui.Flash('✗', "copy source not found")
	case !v.Literal:
		return tui.Flash('✗', "copy source holds a reference")
	case !src.Loc.Fresh:
		return tui.Flash('✗', "likely holds the leaked value")
	case pwRow(r) && rotation.HoldsPassword(v.Unrendered, current(ctx, r)):
		return tui.Flash('✗', leakedValue)
	case leaked(r, lines, v.Unrendered):
		return tui.Flash('✗', "that is the current, leaked value")
	}
	s.clearSel()
	return changes(nil, puts, '✓', placesText(len(lines))+": copied from "+placeLabel(ctx, src.Loc.Place))
}

// plannedAt is the pending variable.set at line p, whose planned value a
// copy takes; false when p has none (a delete, or no change).
func plannedAt(ctx *tui.Ctx, p view.RotatePlace) (changeset.Change, bool) {
	if p.Pending == nil || ctx.CS == nil {
		return changeset.Change{}, false
	}
	c, ok := ctx.CS.Get(p.Pending.ID)
	if !ok || c.Kind != "variable.set" {
		return changeset.Change{}, false
	}
	return c, true
}

// copyPlanned copies the value src's pending change plans to the lines,
// linked to it: they follow it to a new value and go when it is undone.
// Refused: a value of the row's old ones, and a source that already copies
// one of the lines (the link would go round).
func (s *places) copyPlanned(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace, src view.RotatePlace, planned changeset.Change) tea.Cmd {
	value, _ := kinds.SetValue(planned)
	if leaked(r, lines, value) {
		return tui.Flash('✗', "that is the current, leaked value")
	}
	var targets []string
	for _, p := range lines {
		if p.Pending != nil {
			targets = append(targets, p.Pending.ID)
		}
	}
	if slices.ContainsFunc(kinds.CopiesOf(pending(ctx), targets...), func(c changeset.Change) bool { return c.ID == planned.ID }) {
		return tui.Flash('✗', "the source copies one of the chosen places")
	}
	now := ctx.Clock()()
	puts := make([]changeset.Change, 0, len(lines))
	for _, p := range lines {
		c, err := kinds.CopyPlanned(ctx.Store, pending(ctx), p.Loc.Place, p.Loc.Name, planned, now)
		if err != nil {
			return tui.Flash('✗', err.Error())
		}
		mark(&c, r, p)
		tie(r, &c)
		puts = append(puts, c)
	}
	s.clearSel()
	return changes(nil, puts, '✓', placesText(len(lines))+": copied from "+placeLabel(ctx, src.Loc.Place)+" · follows its new value")
}

// remove puts a variable.delete at the lines (begin has refused what it
// must); a pending add is dropped instead.
func (s *places) remove(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace) tea.Cmd {
	now := ctx.Clock()()
	var removes []string
	var puts []changeset.Change
	for _, p := range lines {
		if p.Pending != nil && p.Pending.Kind == "variable.set" && p.Pending.Base.Absent {
			removes = append(removes, p.Pending.ID)
			continue
		}
		c, err := kinds.DeleteVariable(ctx.Store, p.Loc.Place, p.Loc.Name, now)
		if err != nil {
			return tui.Flash('✗', err.Error())
		}
		mark(&c, r, p)
		puts = append(puts, c)
	}
	s.clearSel()
	return changes(removes, puts, '✓', placesText(len(lines))+": delete")
}

// undo removes the targets' changes, whoever made them, and forgets the
// planned values no change carries any more.
func (s *places) undo(ctx *tui.Ctx, r view.RotateRow) tea.Cmd {
	if pwRow(r) {
		return s.pwUndo(ctx, r)
	}
	return s.undoLines(ctx, r, s.chosen(r), false)
}

// undoConfirmed undoes the lines ids name once the copies question was
// answered yes.
func (s *places) undoConfirmed(ctx *tui.Ctx, ids []string) tea.Cmd {
	r, lines, cmd := s.resolve(ctx, ids, "u")
	if cmd != nil {
		return cmd
	}
	return s.undoLines(ctx, r, lines, true)
}

// undoLines removes the lines' changes; when copies of their planned values
// would go too, it asks first unless confirmed.
func (s *places) undoLines(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace, confirmed bool) tea.Cmd {
	gone := map[string]bool{}
	var removes []string
	for _, p := range lines {
		if p.Pending == nil {
			continue
		}
		if p.Pending.RunID != "" {
			return tui.Flash('✗', "in run "+p.Pending.RunID)
		}
		gone[p.Pending.ID] = true
		removes = append(removes, p.Pending.ID)
	}
	if len(removes) == 0 {
		return tui.Flash('!', "nothing to undo here")
	}
	if copies := kinds.CopiesOf(pending(ctx), removes...); len(copies) > 0 && !confirmed {
		s.mode, s.action, s.targets = modeConfirm, "u", idsOf(lines)
		s.confirm = &components.Confirm{Prompt: copiesPrompt(ctx, copies)}
		return nil
	}
	s.forget(ctx, r, lines, gone)
	s.clearSel()
	return changes(removes, nil, '✓', placesText(len(removes))+": decision undone")
}

// copiesPrompt asks whether the copies go with the undone places.
func copiesPrompt(ctx *tui.Ctx, copies []changeset.Change) string {
	at := make([]string, 0, len(copies))
	for _, c := range copies {
		at = append(at, placeLabel(ctx, c.Place))
	}
	what := strconv.Itoa(len(copies)) + " copies of it go too: "
	if len(copies) == 1 {
		what = "1 copy of it goes too: "
	}
	return "undo? " + what + strings.Join(at, ", ")
}

// reveal shows the cursor line's current value, or hides it.
func (s *places) reveal(ctx *tui.Ctx, r view.RotateRow) tea.Cmd {
	if s.revealed != "" {
		s.revealed = ""
		return nil
	}
	if !ctx.IsTTY {
		return tui.Flash('!', "reveal needs a terminal")
	}
	i := s.locate(r)
	if i < 0 {
		return nil
	}
	id := items(r)[i].Loc.ID
	if id == pwLineID {
		return tui.Flash('!', "the password lives in the database · v works on URL lines")
	}
	v := ctx.Store.Variable(id)
	switch {
	case v == nil:
		return tui.Flash('!', "nothing to reveal: not in Railway yet")
	case v.Sealed:
		return tui.Flash('!', "sealed values cannot be read")
	}
	s.revealed, s.revealedAt = id, dataRev(ctx)
	return nil
}

// dataRev is the revision of the variables and the change set.
func dataRev(ctx *tui.Ctx) [2]uint64 {
	var cs uint64
	if ctx.CS != nil {
		cs = ctx.CS.Rev
	}
	return [2]uint64{ctx.Store.Rev(store.Variables), cs}
}

func (s *places) View(ctx *tui.Ctx, width, height int) string {
	if ctx.Store == nil || ctx.Views == nil {
		return ""
	}
	th := ctx.Theme
	r, ok := s.row(ctx)
	if !ok {
		return th.Muted.Render(ansi.Truncate("this row is gone · esc back", width, "…"))
	}
	s.sync(r)
	s.seen([]view.RotateRow{r})
	tail := []string{th.Faint.Render(strings.Repeat("─", width)), s.coverage(ctx, r, width)}
	if s.mode == modeList {
		tail = append(tail, s.leakNote(ctx, r, width)...)
	}
	if pwRow(r) {
		for _, n := range pwNotes(ctx, r, "read only · c in the list continues") {
			tail = append(tail, ansi.Truncate(n, width, "…"))
		}
	}
	switch s.mode {
	case modeMask:
		if s.action == "o" {
			tail = append(tail, th.Danger.Render(ansi.Truncate(ctx.Icons.Warn()+" "+handWarning, width, "…")))
		}
		tail = append(tail, th.Warn.Render(s.mask.View(width)))
	case modePick:
		tail = append(tail, strings.Split(s.picker.View(width), "\n")...)
	case modeRead:
		tail = append(tail, th.Muted.Render("reading the file…"))
	case modeChoose:
		tail = append(tail, th.Warn.Render(ansi.Truncate("copy to "+placesText(len(s.targets))+" · choose the source · enter copies · esc cancels", width, "…")))
	case modeConfirm:
		if r.Warning != "" {
			tail = append(tail, th.Warn.Render(ansi.Truncate(ctx.Icons.Warn()+" "+text.Sanitize(r.Warning), width, "…")))
		}
		tail = append(tail, th.Warn.Render(s.confirm.View(width)))
	}
	listH := max(0, height-len(tail))
	out := s.lines(ctx, r, width, listH)
	for len(out) < listH {
		out = append(out, "")
	}
	if i := s.locate(r); i >= 0 {
		if panel := s.revealPanel(ctx, r, items(r)[i], width); panel != nil {
			out = components.Overlay(out, panel, s.cursorRow)
		}
	}
	out = append(out, tail...)
	if len(out) > height {
		out = out[:max(height, 0)]
	}
	return strings.Join(out, "\n")
}

// lines draws the row's lines in height lines around the cursor.
func (s *places) lines(ctx *tui.Ctx, r view.RotateRow, width, height int) []string {
	idx := s.locate(r)
	if idx < 0 || height < 1 {
		return nil
	}
	th := ctx.Theme
	ls := items(r)
	s.list.Cursor = idx
	from, to := s.list.Window(len(ls), height)
	cols := placeColsOf(ctx, r, r.Places, width-3)
	pcols := pwCols{}
	if pwRow(r) {
		pcols = pwColsOf(ctx, r, ls, width-3)
	}
	out := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		p := ls[i]
		cur, sel := " ", " "
		if i == idx {
			cur = th.Accent.Render("▸")
		}
		if s.sel[p.Loc.ID] {
			sel = th.Accent.Render("*")
		}
		var l string
		if pwRow(r) {
			l = cur + sel + " " + pwPlaceLine(ctx, r, p, pcols)
		} else {
			l = cur + sel + " " + placeLine(ctx, r, p, cols)
		}
		out = append(out, ansi.Truncate(l, width, "…"))
	}
	s.cursorRow = idx - from
	return out
}

// revealPanel is the floating panel of the cursor line's revealed value;
// nil when none is revealed.
func (s *places) revealPanel(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace, width int) []string {
	if s.revealed != "" && s.revealedAt != dataRev(ctx) {
		s.revealed = "" // the data changed since: hide it
	}
	if s.revealed == "" || s.revealed != p.Loc.ID {
		return nil
	}
	v := ctx.Store.Variable(p.Loc.ID)
	if v == nil || v.Sealed {
		return nil
	}
	value := text.Sanitize(v.Rendered)
	title := text.Sanitize(p.Loc.Name) + " · " + placeLabel(ctx, p.Loc.Place) + " · " + strconv.Itoa(len([]rune(v.Rendered))) + " chars"
	return components.RevealPanel(ctx.Theme, title, value, width)
}

// leakNote explains the cursor line when it changed after the leak but its
// value is older elsewhere, in two lines; nil otherwise.
func (s *places) leakNote(ctx *tui.Ctx, r view.RotateRow, width int) []string {
	i := s.locate(r)
	if i < 0 || pwRow(r) {
		return nil
	}
	p := items(r)[i]
	old, ok := stillLeaked(r, p)
	if !ok {
		return nil
	}
	th := ctx.Theme
	letter := rotation.Letter(p.Letter)
	return []string{
		th.Warn.Render(ansi.Truncate("↻ "+placeLabel(ctx, p.Loc.Place)+" changed after the leak, but it is still the leaked value", width, "…")),
		th.Muted.Render(ansi.Truncate("  same value "+letter+" at "+placeLabel(ctx, old.Loc.Place)+", unchanged since "+stamp(old.Loc.Updated)+" · rotate all "+letter, width, "…")),
	}
}

// coverage is "X of Y places change · Z keep the old value".
func (s *places) coverage(ctx *tui.Ctx, r view.RotateRow, width int) string {
	th := ctx.Theme
	if pwRow(r) {
		l, style := pwFooter(ctx, r)
		return style.Render(ansi.Truncate(l, width, "…"))
	}
	changed := 0
	for _, p := range r.Places {
		if p.Pending != nil {
			changed++
		}
	}
	l := strconv.Itoa(changed) + " of " + strconv.Itoa(len(r.Places)) + " places change"
	style := th.OK
	switch keep := len(r.Places) - changed; {
	case keep == 1:
		l += " · 1 keeps the old value"
		style = th.Warn
	case keep > 1:
		l += " · " + strconv.Itoa(keep) + " keep the old value"
		style = th.Warn
	}
	if changed == 0 {
		style = th.Muted
	}
	return style.Render(ansi.Truncate(l, width, "…"))
}

// pwBegin starts action on lines of password row r (refuse has passed):
// the password line takes g, p and o, a URL line p, f, c, x and k. asked:
// a KEYFILE's question was answered yes.
func (s *places) pwBegin(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace, action string, asked bool) tea.Cmd {
	own := lines[0].Own
	if own && r.Password.KeyFile && (action == "g" || action == "p") && r.Change == nil && !asked {
		s.mode, s.action, s.targets = modeConfirm, action, idsOf(lines)
		s.confirm = &components.Confirm{Prompt: keyFilePrompt}
		return nil
	}
	switch action {
	case "g":
		v, err := s.passwordValue(ctx, r)
		if err != nil {
			return tui.Flash('✗', "cannot generate: "+err.Error())
		}
		s.clearSel()
		return decidePassword(ctx, r, kinds.SourceGenerate, v)
	case "p", "o":
		label := r.Label + " · " + placesText(len(lines))
		switch {
		case own && action == "o":
			label = r.Label + " · the password you already set inside the database"
		case own:
			label = r.Label + " · new password"
		}
		s.mode, s.action, s.targets = modeMask, action, idsOf(lines)
		s.mask = &components.MaskedInput{Label: label, Repeat: true}
	case "f":
		home, _ := os.UserHomeDir()
		s.picker = components.NewFilePicker(components.OSDirReader{}, home)
		s.picker.Theme = ctx.Theme
		s.mode, s.action, s.targets = modePick, action, idsOf(lines)
	case "c":
		if since(ctx).IsZero() {
			return tui.Flash('!', "copy needs a leak time · Settings l")
		}
		s.mode, s.action, s.targets = modeChoose, action, idsOf(lines)
	case "x":
		return s.remove(ctx, r, lines)
	case "k":
		return s.keep(ctx, r, lines)
	}
	return nil
}

// keep puts a variable.keep of the row's group at each URL line: the place
// keeps its value, and so the old password, which stops working.
func (s *places) keep(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace) tea.Cmd {
	now := ctx.Clock()()
	puts := make([]changeset.Change, 0, len(lines))
	for _, p := range lines {
		c := kinds.Keep(p.Loc.Place, p.Loc.Name, r.Key, now)
		c.Origin = &changeset.Origin{Producer: producer, Row: r.Key}
		puts = append(puts, c)
	}
	s.clearSel()
	return changes(nil, puts, '!', placesText(len(lines))+": kept · old password stops working")
}

// pwUndo undoes the targets of a password row: the password line removes
// the password change alone (the root removes its automatic rewrites); a
// URL line's own decision is removed and, while a password is decided, the
// place is rewritten with it again. An automatic rewrite is undone with the
// password.
func (s *places) pwUndo(ctx *tui.Ctx, r view.RotateRow) tea.Cmd {
	lines := s.chosen(r)
	if cmd := refuse(ctx, r, lines, "u"); cmd != nil {
		return cmd
	}
	if lines[0].Own {
		s.clearSel()
		return s.undoPassword(r)
	}
	// gone are the removed changes the rebuild rewrites; an Unclear or
	// unrecognised place's is removed without a rewrite (it cannot have
	// one), so the rebuild keeps it and its put is dropped.
	gone := map[string]bool{}
	unclear := map[string]bool{}
	var removes []string
	for _, p := range lines {
		switch {
		case p.Pending == nil:
			continue
		case p.Auto:
			return tui.Flash('!', "rewritten with the password · u on the password line undoes it")
		case p.Unclear || p.URLMode == rotation.URLUnrecognised:
			unclear[p.Pending.Key()] = true
		default:
			gone[p.Pending.ID] = true
		}
		removes = append(removes, p.Pending.ID)
	}
	if len(removes) == 0 {
		return tui.Flash('!', "nothing to undo here")
	}
	puts, err := rebuild(ctx, r, gone)
	if err != nil {
		return tui.Flash('✗', "cannot undo: "+err.Error())
	}
	puts = slices.DeleteFunc(puts, func(c changeset.Change) bool { return unclear[c.Key()] })
	s.clearSel()
	kind, done := '✓', placesText(len(removes))+": decision undone"
	switch {
	case len(unclear) > 0:
		kind, done = '!', done+" · set this place by hand before running"
	case len(puts) > 0:
		done += " · rewritten with the password"
	}
	return changes(removes, puts, kind, done)
}

// Package variables is the Variables screen: one tree of the variables of
// the selected environments, grouped by name or by value (view.Tree), with
// what the change set does to each place. Opened with a place (Services
// enter) it lists that place's variables only, one line each.
//
// It edits, adds, deletes and undoes variables as changes, at one place or
// at every place of a group, and reveals the cursor place's value on
// request. A write to an environment outside Settings' write environments is
// refused before any change is put. The authors ("by") come from each
// environment's history, read the first time one of its places is shown; a
// failed read stands in the notice until r asks it again.
package variables

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

type mode int

const (
	modeNone mode = iota
	modeEdit
	modeAddName
	modePick
	modeAddValue
	modeConfirm
)

type screen struct {
	// scope is the place of the scoped view; nil for the tree.
	scope    *domain.Place
	grouping view.Grouping
	// unfolded holds the unfolded groups by GroupRow.Key; by-name and
	// by-value keys never clash, so each grouping keeps its own.
	unfolded map[string]bool
	// cur is the cursor's line by identity; list.Cursor follows it.
	cur    ident
	filter *components.Filter
	list   components.List

	mode    mode
	name    string          // the variable being added
	targets []view.PlaceRow // the places an edit or a group delete writes
	text    *components.TextInput
	mask    *components.MaskedInput
	confirm *components.Confirm
	cands   []domain.Place // the places an add may go to
	pick    components.List

	// revealed is the location id of the place whose value is shown; ""
	// for none. revealedTree is the tree it was revealed in: a store or
	// change set update that gives another tree hides it.
	revealed     string
	revealedTree *view.GroupRow

	// History and members reads: asked this round (until Resume), and the
	// first-page history reads still in flight while on top (Suspend
	// forgets them). The store's load status is not used: a stale answer
	// updates it too.
	askedHist, flightHist map[string]bool
	askedMem, flightMem   map[string]bool
	// errs is the last failed read by key ("h:" env, "m:" project); a
	// later answer that succeeds clears it.
	errs map[string]string
}

func newScreen(scope *domain.Place) *screen {
	key := "variables"
	if scope != nil {
		key += ":" + scope.Key()
	}
	return &screen{
		scope: scope, unfolded: map[string]bool{}, filter: components.NewFilter(key),
		askedHist: map[string]bool{}, flightHist: map[string]bool{},
		askedMem: map[string]bool{}, flightMem: map[string]bool{},
		errs: map[string]string{},
	}
}

// New returns the variables screen of a place: its variables, one line
// each.
func New(p domain.Place) tui.Screen { return newScreen(&p) }

// Build is the registry constructor: no argument gives the tree of the
// selected environments, a domain.Place the scoped view of that place. It
// returns nil for any other arguments.
func Build(args ...any) tui.Screen {
	switch len(args) {
	case 0:
		return newScreen(nil)
	case 1:
		if p, ok := args[0].(domain.Place); ok {
			return New(p)
		}
	}
	return nil
}

func (s *screen) Init(ctx *tui.Ctx) tea.Cmd { s.filter.Load(ctx); return s.reads(ctx) }
func (s *screen) Title() string             { return "Variables" }

// Resume forgets the reads that answered, so a failed one is asked again.
func (s *screen) Resume(ctx *tui.Ctx) tea.Cmd { return s.retry(ctx) }

// retry forgets the reads that answered and asks again those the store does
// not hold (a failed one).
func (s *screen) retry(ctx *tui.Ctx) tea.Cmd {
	for env := range s.askedHist {
		if !s.flightHist[env] {
			delete(s.askedHist, env)
		}
	}
	for p := range s.askedMem {
		if !s.flightMem[p] {
			delete(s.askedMem, p)
		}
	}
	return s.reads(ctx)
}

// Suspend hides a revealed value when another screen covers this one, and
// forgets the reads in flight: the root forwards their answers to the
// screen on top, so Resume asks again what the store does not hold then.
func (s *screen) Suspend(*tui.Ctx) {
	s.hide()
	clear(s.flightHist)
	clear(s.flightMem)
}

func (s *screen) hide() { s.revealed, s.revealedTree = "", nil }

// Capturing reports whether an input takes every key.
func (s *screen) Capturing() bool { return s.mode != modeNone || s.filter.Editing() }

// Note is the grouping and the group counts; the scoped view has none.
func (s *screen) Note(ctx *tui.Ctx) string {
	if s.scope != nil || ctx.Store == nil {
		return ""
	}
	gs := s.tree(ctx)
	diverged := 0
	for _, g := range gs {
		if g.Values > 1 {
			diverged++
		}
	}
	by := "by name"
	if s.grouping == view.ByValue {
		by = "by value"
	}
	note := by + " · " + strconv.Itoa(len(gs)) + " groups · " + strconv.Itoa(diverged) + " diverged"
	// the frame header holds "railway-manager <title>  <note>"
	if room := ctx.Width - tui.ChromeCols - len("railway-manager ") - len(s.Title()) - 1; ansi.StringWidth(note) > room {
		note = by + " · " + strconv.Itoa(len(gs)) + " · " + strconv.Itoa(diverged) + " diverged"
	}
	return note
}

// Notice is the first failed history or members read, in environment
// order; actions flash instead.
func (s *screen) Notice(ctx *tui.Ctx) tui.FlashMsg {
	if ctx.Store == nil || len(s.errs) == 0 {
		return tui.FlashMsg{}
	}
	fail := func(what, err string) tui.FlashMsg {
		return tui.FlashMsg{Kind: '✗', Text: text.Sanitize("cannot read " + what + ": " + err + " · r retries")}
	}
	envs := envIDs(ctx)
	if s.scope != nil {
		envs = []string{s.scope.EnvID}
	}
	for _, env := range envs {
		if e, ok := s.errs["h:"+env]; ok {
			return fail("history of "+ctx.Store.EnvLabel(env), e)
		}
	}
	for _, p := range ctx.Store.Projects() {
		if e, ok := s.errs["m:"+p.ID]; ok {
			return fail("members of "+p.Name, e)
		}
	}
	return tui.FlashMsg{}
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

func (s *screen) Keys() []tui.KeyHint {
	switch {
	case s.mode == modeConfirm:
		return []tui.KeyHint{
			{Key: "y", Label: "delete", Always: true},
			{Key: "n", Label: "cancel", Always: true},
		}
	case s.mode == modePick:
		return []tui.KeyHint{
			{Key: "enter", Label: "pick", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	case s.mode != modeNone:
		return []tui.KeyHint{
			{Key: "enter", Label: "confirm", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	case s.filter.Editing():
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	hints := []tui.KeyHint{
		{Key: "e", Label: "edit", Rank: 1},
		{Key: "a", Label: "add", Rank: 2},
		{Key: "x", Label: "delete", Rank: 3},
		{Key: "u", Label: "undo", Rank: 4},
		{Key: "v", Label: "reveal", Rank: 5},
		{Key: "/", Label: "filter", Rank: 6, Optional: true},
		{Key: "d", Label: "history", Rank: 7, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
	if len(s.errs) > 0 {
		hints = append(hints, tui.KeyHint{Key: "r", Label: "retry", Rank: 8, Optional: true})
	}
	if s.scope == nil {
		g := "by value"
		if s.grouping == view.ByValue {
			g = "by name"
		}
		hints = append(hints,
			tui.KeyHint{Key: "enter", Label: "unfold", Rank: 0},
			tui.KeyHint{Key: "g", Label: g, Rank: 1},
		)
		if s.grouping == view.ByValue {
			hints = append(hints, tui.KeyHint{Key: "R", Label: "rotate", Rank: 7, Optional: true})
		}
	}
	return hints
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

// tree is the groups shown: the scoped place's by name, else the selected
// environments' in the current grouping.
func (s *screen) tree(ctx *tui.Ctx) []view.GroupRow {
	if ctx.Views == nil || ctx.Store == nil {
		return nil
	}
	if s.scope != nil {
		return ctx.Views.Tree(ctx.CS, nil, s.scope, view.ByName)
	}
	return ctx.Views.Tree(ctx.CS, envIDs(ctx), nil, s.grouping)
}

// reads asks for the history (first page) and members of the environments
// and projects of the places shown, once each per round.
func (s *screen) reads(ctx *tui.Ctx) tea.Cmd {
	if ctx.API == nil || ctx.Store == nil {
		return nil
	}
	_, ls, _ := s.lines(ctx)
	clock := ctx.Clock()
	var cmds []tea.Cmd
	for _, l := range ls {
		if l.p == nil {
			continue
		}
		env, proj := l.p.Place.EnvID, l.p.Place.ProjectID
		if _, _, _, read := ctx.Store.History(env); !read && !s.askedHist[env] {
			s.askedHist[env], s.flightHist[env] = true, true
			cmds = append(cmds, loaders.History(ctx.API, env, "", clock().Add(-loaders.HistorySince), clock))
		}
		if ctx.Store.Load(store.MembersKey(proj)).ReadAt.IsZero() && !s.askedMem[proj] {
			s.askedMem[proj], s.flightMem[proj] = true, true
			cmds = append(cmds, loaders.Members(ctx.API, proj, clock))
		}
	}
	return tea.Batch(cmds...)
}

func (s *screen) closeInput() {
	s.mode, s.name, s.targets = modeNone, "", nil
	s.text, s.mask, s.confirm, s.cands = nil, nil, nil, nil
}

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if ctx.Store == nil {
		return s, nil
	}
	cmd := s.update(ctx, msg)
	return s, tea.Batch(cmd, s.reads(ctx))
}

func (s *screen) update(ctx *tui.Ctx, msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case components.CancelMsg:
		s.closeInput()
		return nil
	case components.SubmitMsg:
		return s.submit(ctx, m.Value)
	case components.ConfirmedMsg:
		asked := s.targets
		s.closeInput()
		rows, ok := s.current(ctx, asked)
		if !ok {
			return tui.Flash('!', "the places changed since x · x again")
		}
		return s.deleteAll(ctx, rows) // checks runs and write environments again
	case tea.KeyMsg:
		switch s.mode {
		case modeEdit, modeAddValue:
			return s.mask.Update(m)
		case modeAddName:
			return s.text.Update(m)
		case modeConfirm:
			return s.confirm.Update(m)
		case modePick:
			return s.pickKey(ctx, m)
		}
		if h, cmd := s.filter.Update(ctx, m); h {
			s.hide()
			return cmd
		}
		return s.key(ctx, m)
	case loaders.HistoryMsg:
		if m.After == "" {
			delete(s.flightHist, m.EnvID)
			s.record("h:"+m.EnvID, m.Err)
		}
	case loaders.MembersMsg:
		delete(s.flightMem, m.ProjectID)
		s.record("m:"+m.ProjectID, m.Err)
	}
	// a store or change set update: a new tree hides a revealed value
	if s.revealed != "" {
		if gs := s.tree(ctx); len(gs) == 0 || &gs[0] != s.revealedTree {
			s.hide()
		}
	}
	return nil
}

// refused flashes the write refusal for the first environment of places
// outside Settings' write environments; nil when all are writable.
func refused(ctx *tui.Ctx, places ...domain.Place) tea.Cmd {
	for _, p := range places {
		if ctx.Settings == nil || !slices.Contains(ctx.Settings.WriteEnvs, p.EnvID) {
			return tui.Flash('✗', ctx.Store.EnvLabel(p.EnvID)+" is not in write environments · Settings w")
		}
	}
	return nil
}

// locked flashes when a place's pending change is held by a run.
func locked(rows []view.PlaceRow) tea.Cmd {
	for _, r := range rows {
		if r.Pending != nil && r.Pending.RunID != "" {
			return tui.Flash('✗', "in run "+r.Pending.RunID)
		}
	}
	return nil
}

func placesOf(rows []view.PlaceRow) []domain.Place {
	out := make([]domain.Place, len(rows))
	for i, r := range rows {
		out[i] = r.Place
	}
	return out
}

// removes reports whether deleting a place means dropping its change: a
// pending add, or a variable gone from Railway.
func removes(r view.PlaceRow) bool {
	return r.Pending != nil && (r.Missing || (r.Pending.Kind == "variable.set" && r.Pending.Base.Absent))
}

func (s *screen) submit(ctx *tui.Ctx, value string) tea.Cmd {
	switch s.mode {
	case modeAddName:
		name := value
		// An empty value validates the name alone.
		if _, err := kinds.SetVariable(ctx.Store, nil, *s.scope, name, "", ctx.Now()); err != nil {
			return tui.Flash('✗', err.Error())
		}
		if holds(ctx, *s.scope, name) {
			return tui.Flash('!', name+" exists · e edits it")
		}
		s.mode, s.name, s.text = modeAddValue, name, nil
		s.targets = []view.PlaceRow{{Place: *s.scope, Name: name}}
		s.mask = &components.MaskedInput{Label: name, Repeat: true}
		return nil
	case modeEdit, modeAddValue:
		targets, added := s.targets, s.mode == modeAddValue
		s.closeInput()
		if !added { // the places may have changed while typing
			rows, ok := s.current(ctx, targets)
			if !ok {
				return tui.Flash('!', "the places changed since e · e again")
			}
			if cmd := locked(rows); cmd != nil {
				return cmd
			}
			if cmd := refused(ctx, placesOf(rows)...); cmd != nil {
				return cmd
			}
			targets = rows
		}
		var cmds []tea.Cmd
		for _, t := range targets {
			c, err := kinds.SetVariable(ctx.Store, pending(ctx), t.Place, t.Name, value, ctx.Now())
			if err != nil {
				return tui.Flash('✗', err.Error())
			}
			cmds = append(cmds, tui.Emit(tui.PutChangeMsg{Change: c}))
		}
		if added && len(targets) == 1 && s.filter.Active() && !s.filter.Match(targets[0].Name) {
			cmds = append(cmds, tui.Flash('!', "added · hidden by the filter"))
		}
		return tea.Batch(cmds...)
	}
	return nil
}

// deleteAll deletes every place: a pending add's change is dropped, the
// others get a variable.delete. Nothing is sent when one is refused.
func (s *screen) deleteAll(ctx *tui.Ctx, rows []view.PlaceRow) tea.Cmd {
	var puts []domain.Place
	for _, r := range rows {
		if !removes(r) {
			puts = append(puts, r.Place)
		}
	}
	if cmd := locked(rows); cmd != nil {
		return cmd
	}
	if cmd := refused(ctx, puts...); cmd != nil {
		return cmd
	}
	var cmds []tea.Cmd
	for _, r := range rows {
		if removes(r) {
			cmds = append(cmds, tui.Emit(tui.RemoveChangeMsg{ID: r.Pending.ID}))
			continue
		}
		c, err := kinds.DeleteVariable(ctx.Store, r.Place, r.Name, ctx.Now())
		if err != nil {
			return tui.Flash('✗', err.Error())
		}
		cmds = append(cmds, tui.Emit(tui.PutChangeMsg{Change: c}))
	}
	return tea.Batch(cmds...)
}

// current re-reads rows from the tree as it is now, by location id; false
// when one is gone or its pending change is another.
func (s *screen) current(ctx *tui.Ctx, rows []view.PlaceRow) ([]view.PlaceRow, bool) {
	now := map[string]view.PlaceRow{}
	for _, g := range s.tree(ctx) {
		for _, p := range g.Places {
			now[p.LocationID] = p
		}
	}
	out := make([]view.PlaceRow, 0, len(rows))
	for _, r := range rows {
		n, ok := now[r.LocationID]
		if !ok || pendingID(n) != pendingID(r) || n.Missing != r.Missing || n.Name != r.Name {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func pendingID(r view.PlaceRow) string {
	if r.Pending == nil {
		return ""
	}
	return r.Pending.ID
}

// holds reports whether a place has the variable, in Railway or pending.
func holds(ctx *tui.Ctx, p domain.Place, name string) bool {
	for _, r := range ctx.Views.PlaceVariables(ctx.CS, p) {
		if r.Name == name {
			return true
		}
	}
	return false
}

func (s *screen) key(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	_, ls, _ := s.lines(ctx)
	h := s.listHeight(ctx, ctx.Height-tui.ChromeRows)
	if k.Type != tea.KeyRunes || string(k.Runes) != "v" {
		s.hide() // any other key hides a revealed value
	}
	idx := s.locate(ls)
	move := func(d int) {
		s.list.Move(len(ls), d, h)
		if len(ls) > 0 {
			s.cur = identOf(ls[s.list.Cursor])
		}
	}
	key := ""
	switch k.Type {
	case tea.KeyUp:
		move(-1)
	case tea.KeyDown:
		move(1)
	case tea.KeyPgUp:
		move(-h)
	case tea.KeyPgDown:
		move(h)
	case tea.KeyEnter, tea.KeyRight:
		if idx >= 0 && s.scope == nil && ls[idx].kind == lineGroup {
			s.unfolded[ls[idx].g.Key] = true
		}
	case tea.KeyLeft:
		if idx >= 0 && s.scope == nil && ls[idx].kind != lineSingle {
			delete(s.unfolded, ls[idx].g.Key)
			s.cur = ident{group: ls[idx].g.Key}
		}
	case tea.KeyRunes:
		key = string(k.Runes)
	}
	switch key {
	case "j":
		move(1)
		return nil
	case "k":
		move(-1)
		return nil
	case "g":
		if s.scope == nil {
			s.switchGrouping(ctx, ls, idx)
		}
		return nil
	case "a":
		return s.add(ctx, ls, idx)
	case "r":
		return s.retry(ctx)
	case "R":
		return s.rotate(ls, idx)
	}
	if idx < 0 || key == "" {
		return nil
	}
	l := ls[idx]
	switch key {
	case "e":
		rows := l.rows()
		if cmd := locked(rows); cmd != nil {
			return cmd
		}
		if cmd := refused(ctx, placesOf(rows)...); cmd != nil {
			return cmd
		}
		label := l.name()
		if l.kind == lineGroup {
			label = l.g.Label + " · " + strconv.Itoa(len(rows)) + " places"
		}
		s.mode, s.targets = modeEdit, rows
		s.mask = &components.MaskedInput{Label: label, Repeat: true}
	case "x":
		rows := l.rows()
		if l.kind != lineGroup {
			return s.deleteAll(ctx, rows)
		}
		if cmd := locked(rows); cmd != nil {
			return cmd
		}
		var puts []domain.Place
		for _, r := range rows {
			if !removes(r) {
				puts = append(puts, r.Place)
			}
		}
		if cmd := refused(ctx, puts...); cmd != nil {
			return cmd
		}
		s.mode, s.targets = modeConfirm, rows
		s.confirm = &components.Confirm{Prompt: "delete " + strconv.Itoa(len(rows)) + " places?"}
	case "u":
		rows := l.rows()
		if cmd := locked(rows); cmd != nil {
			return cmd
		}
		var cmds []tea.Cmd
		for _, r := range rows {
			if r.Pending != nil {
				cmds = append(cmds, tui.Emit(tui.RemoveChangeMsg{ID: r.Pending.ID}))
			}
		}
		if len(cmds) == 0 {
			return tui.Flash('!', "nothing to undo here")
		}
		return tea.Batch(cmds...)
	case "v":
		if l.p == nil {
			return nil // a group has no one value
		}
		if s.revealed != "" {
			s.hide()
			return nil
		}
		if !ctx.IsTTY {
			return tui.Flash('!', "reveal needs a terminal")
		}
		v := ctx.Store.Variable(l.p.LocationID)
		switch {
		case v == nil:
			return tui.Flash('!', "nothing to reveal: not in Railway yet")
		case v.Sealed:
			return tui.Flash('!', "sealed values cannot be read")
		}
		gs := s.tree(ctx)
		s.revealed, s.revealedTree = l.p.LocationID, &gs[0]
	case "d":
		return tui.Emit(tui.OpenMsg{Name: "history", Args: []any{l.name()}})
	}
	return nil
}

// rotate opens Rotate on the value of the cursor's by-value group line (a
// one-place group included). A variable gone from
// Railway with nothing new has no value to rotate.
func (s *screen) rotate(ls []line, idx int) tea.Cmd {
	if s.scope == nil && s.grouping == view.ByValue && idx >= 0 && ls[idx].kind != linePlace {
		fp, _, _ := strings.Cut(strings.TrimPrefix(ls[idx].g.Key, "fp:"), "@")
		if fp != "" && fp != "gone" {
			return tui.Emit(tui.OpenMsg{Name: "rotate", Args: []any{fp}})
		}
	}
	return tui.Flash('!', "R works on a value group · g groups by value")
}

// switchGrouping flips by name and by value. The cursor follows its place,
// unfolding the place's new group; from a group line it goes to the group
// now holding that group's first place.
func (s *screen) switchGrouping(ctx *tui.Ctx, ls []line, idx int) {
	loc, onPlace := s.cur.loc, s.cur.loc != ""
	if !onPlace && idx >= 0 && len(ls[idx].g.Places) > 0 {
		loc = ls[idx].g.Places[0].LocationID
	}
	s.grouping = 1 - s.grouping
	for _, g := range s.tree(ctx) {
		for _, p := range g.Places {
			if p.LocationID != loc {
				continue
			}
			switch {
			case len(g.Places) == 1:
				s.cur = ident{group: g.Key, loc: loc}
			case onPlace:
				s.unfolded[g.Key] = true
				s.cur = ident{group: g.Key, loc: loc}
			default:
				s.cur = ident{group: g.Key}
			}
			return
		}
	}
}

// add starts an add. Scoped: a name, then the value. In the tree: a place
// of the selected environments without the cursor's variable, then the
// value.
func (s *screen) add(ctx *tui.Ctx, ls []line, idx int) tea.Cmd {
	if s.scope != nil {
		if cmd := refused(ctx, *s.scope); cmd != nil {
			return cmd
		}
		s.mode = modeAddName
		s.text = &components.TextInput{Label: "name"}
		return nil
	}
	if idx < 0 {
		return tui.Flash('!', "nothing to add to · open a service from Services to add a new name")
	}
	l := ls[idx]
	name := l.name()
	if l.kind == lineGroup && len(l.g.Names) != 1 {
		return tui.Flash('!', "this group has several names · g groups by name")
	}
	var cands []domain.Place
	for _, p := range ctx.Views.TreePlaces(envIDs(ctx)) {
		if !holds(ctx, p, name) {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		return tui.Flash('!', name+" is at every place")
	}
	s.mode, s.name, s.cands, s.pick = modePick, name, cands, components.List{}
	return nil
}

func (s *screen) pickKey(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	h := s.listHeight(ctx, ctx.Height-tui.ChromeRows)
	switch {
	case k.Type == tea.KeyUp || (k.Type == tea.KeyRunes && string(k.Runes) == "k"):
		s.pick.Move(len(s.cands), -1, h)
	case k.Type == tea.KeyDown || (k.Type == tea.KeyRunes && string(k.Runes) == "j"):
		s.pick.Move(len(s.cands), 1, h)
	case k.Type == tea.KeyPgUp:
		s.pick.Move(len(s.cands), -h, h)
	case k.Type == tea.KeyPgDown:
		s.pick.Move(len(s.cands), h, h)
	case k.Type == tea.KeyEsc || k.Type == tea.KeyCtrlC:
		s.closeInput()
	case k.Type == tea.KeyEnter:
		p := s.cands[s.pick.Cursor]
		if cmd := refused(ctx, p); cmd != nil {
			s.closeInput()
			return cmd
		}
		name := s.name
		s.closeInput()
		s.mode, s.name = modeAddValue, name
		s.targets = []view.PlaceRow{{Place: p, Name: name}}
		s.mask = &components.MaskedInput{Label: name + " at " + placeText(ctx, p), Repeat: true}
	}
	return nil
}

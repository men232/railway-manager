package variables

import (
	"strings"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/view"
)

type lineKind int

const (
	// lineGroup is a group of several places, folded or not.
	lineGroup lineKind = iota
	// linePlace is a place of an unfolded group.
	linePlace
	// lineSingle is a group of one place: group and place on one line.
	lineSingle
)

// line is one line of the list. g is its group; p its place (nil on a
// group line); vis the group's places the filter shows (group lines). All
// point into the view's cached tree: read only.
type line struct {
	kind lineKind
	g    *view.GroupRow
	p    *view.PlaceRow
	vis  []*view.PlaceRow
}

// rows is the places the line acts on: on a group line, the places the
// filter shows (bulk actions act on visible places only).
func (l line) rows() []view.PlaceRow {
	if l.p != nil {
		return []view.PlaceRow{*l.p}
	}
	out := make([]view.PlaceRow, len(l.vis))
	for i, p := range l.vis {
		out[i] = *p
	}
	return out
}

// name is the line's variable name: the place's, or the group's first.
func (l line) name() string {
	if l.p != nil {
		return l.p.Name
	}
	return l.g.Names[0]
}

// ident is a line by identity: its group key and its place's location id
// ("" on a group line). It survives refreshes, filters and groupings where
// an index would not.
type ident struct{ group, loc string }

func identOf(l line) ident {
	if l.p != nil {
		return ident{group: l.g.Key, loc: l.p.LocationID}
	}
	return ident{group: l.g.Key}
}

// lines is the tree as lines, after the filter, with the shown and total
// group counts. A group whose label matches shows every place; otherwise
// only the places whose name or "env · service" (as shown) match. The
// scoped view matches names only: its places are all the same.
func (s *screen) lines(ctx *tui.Ctx) (shown int, out []line, total int) {
	s.filter.Load(ctx)
	gs := s.tree(ctx)
	short := ctx.Store != nil && shortEnvs(ctx)
	match := func(p *view.PlaceRow) bool {
		if s.filter.Match(p.Name) {
			return true
		}
		return s.scope == nil && s.filter.Match(locText(short, p))
	}
	for i := range gs {
		g := &gs[i]
		var ps []*view.PlaceRow
		all := s.filter.Match(g.Label)
		for j := range g.Places {
			p := &g.Places[j]
			if all || match(p) {
				ps = append(ps, p)
			}
		}
		if len(ps) == 0 {
			continue
		}
		shown++
		if len(g.Places) == 1 {
			out = append(out, line{kind: lineSingle, g: g, p: ps[0]})
			continue
		}
		out = append(out, line{kind: lineGroup, g: g, vis: ps})
		if s.unfolded[g.Key] {
			for _, p := range ps {
				out = append(out, line{kind: linePlace, g: g, p: p})
			}
		}
	}
	return shown, out, len(gs)
}

// locate puts the cursor on its line: the same place, else its group, else
// the same index clamped. It returns the index, -1 for no lines. Only an
// exact find updates the remembered line: a place the filter hides is
// returned to when it shows again.
func (s *screen) locate(ls []line) int {
	if len(ls) == 0 {
		s.list.Cursor = 0
		return -1
	}
	clamped := max(0, min(s.list.Cursor, len(ls)-1))
	if s.cur == (ident{}) {
		s.list.Cursor, s.cur = clamped, identOf(ls[clamped])
		return clamped
	}
	exact, group := -1, -1
	for i, l := range ls {
		if identOf(l) == s.cur {
			exact = i
			break
		}
		if group < 0 && l.kind != linePlace && l.g.Key == s.cur.group {
			group = i
		}
	}
	if exact < 0 && s.cur.loc != "" { // the place, now in another group
		for i, l := range ls {
			if l.p != nil && l.p.LocationID == s.cur.loc {
				exact = i
				break
			}
		}
	}
	switch {
	case exact >= 0:
		s.list.Cursor, s.cur = exact, identOf(ls[exact])
	case group >= 0:
		s.list.Cursor = group
	default:
		s.list.Cursor = clamped
	}
	return s.list.Cursor
}

// listHeight is the rows left for the list in a body of the given height.
func (s *screen) listHeight(ctx *tui.Ctx, body int) int {
	h := body
	if s.scope != nil {
		h-- // the place header
	}
	if s.filter.Active() || s.filter.Editing() {
		h--
	}
	switch s.mode {
	case modeEdit, modeAddName, modeAddValue, modeConfirm:
		h--
	case modePick:
		h-- // the picker's title
	}
	return max(1, h)
}

func pending(ctx *tui.Ctx) []changeset.Change {
	if ctx.CS == nil {
		return nil
	}
	return ctx.CS.Changes
}

// shortEnvs reports whether environments are shown without their project:
// one project is selected.
func shortEnvs(ctx *tui.Ctx) bool {
	return len(loaders.Targets(ctx.Store.Projects(), ctx.Settings)) <= 1
}

// envText is an environment label as shown, unsanitized.
func envText(short bool, label string) string {
	if !short {
		return label
	}
	if _, after, ok := strings.Cut(label, "/"); ok {
		return after
	}
	return label
}

// placeText is "env · service" for a place, unsanitized.
func placeText(ctx *tui.Ctx, p domain.Place) string {
	return envText(shortEnvs(ctx), ctx.Store.EnvLabel(p.EnvID)) + " · " + ctx.Store.ServiceName(p.ServiceID)
}

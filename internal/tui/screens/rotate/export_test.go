package rotate

import "github.com/men232/railway-manager/internal/tui"

// Cursor is the label of the row under the cursor; "" when none.
func Cursor(ctx *tui.Ctx, s tui.Screen) string {
	sc := s.(*screen)
	rows := sc.shown(ctx)
	if i := sc.locate(rows); i >= 0 {
		return rows[i].Label
	}
	return ""
}

// IsInternal reports whether a message is one the screens send themselves
// (a file read's result), for test helpers that feed them back.
func IsInternal(m any) bool {
	_, ok := m.(fileReadMsg)
	return ok
}

// PlaceCursor is the location id of the places screen's cursor line.
func PlaceCursor(ctx *tui.Ctx, s tui.Screen) string {
	p := s.(*places)
	if r, ok := p.row(ctx); ok {
		if i := p.locate(r); i >= 0 {
			return r.Places[i].Loc.ID
		}
	}
	return ""
}

// RotateRuns is what the screens pass Views.Rotate of the runs.
var RotateRuns = tui.RotateRuns

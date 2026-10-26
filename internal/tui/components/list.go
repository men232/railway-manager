// Package components holds the reusable TUI widgets: list cursor and window,
// text and masked inputs, and the confirmation prompt.
package components

// List tracks a cursor and a scroll offset over n items. Callers pass the item
// count and visible height on each call, so the list survives data changes.
type List struct{ Cursor, Offset int }

// Move moves the cursor by delta, clamped to [0, n-1], and scrolls to keep it
// inside a window of height rows.
func (l *List) Move(n, delta, height int) {
	if n <= 0 {
		l.Cursor, l.Offset = 0, 0
		return
	}
	l.Cursor += delta
	l.Cursor = max(0, min(l.Cursor, n-1))
	l.fit(n, height)
}

// fit clamps the cursor and offset to n items and scrolls the cursor into view.
func (l *List) fit(n, height int) {
	if n <= 0 {
		l.Cursor, l.Offset = 0, 0
		return
	}
	l.Cursor = max(0, min(l.Cursor, n-1))
	if height < 1 {
		l.Offset = max(0, min(l.Offset, n-1))
		return
	}
	if l.Cursor < l.Offset {
		l.Offset = l.Cursor
	}
	if l.Cursor >= l.Offset+height {
		l.Offset = l.Cursor - height + 1
	}
	l.Offset = max(0, min(l.Offset, max(0, n-height)))
}

// Window returns the half-open range [from, to) of items to draw, after
// clamping to n and height.
func (l *List) Window(n, height int) (from, to int) {
	l.fit(n, height)
	if height < 1 || n <= 0 {
		return l.Offset, l.Offset
	}
	return l.Offset, min(n, l.Offset+height)
}

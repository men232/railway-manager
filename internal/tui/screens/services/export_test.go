package services

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/loaders"
)

// Scheduled is a timer the screen asked for: its delay and the message it
// would deliver. Tests deliver Msg themselves instead of waiting.
type Scheduled struct {
	D   time.Duration
	Msg tea.Msg
}

// FakeTicks replaces the screen's timers: each yields a Scheduled at once.
func FakeTicks() {
	tick = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
		return func() tea.Msg { return Scheduled{D: d, Msg: fn(time.Time{})} }
	}
}

// PanelSpanAt is the span the panel reads at step for a body width: the
// chart cells the panel draws at that width, through loaders.PanelSpan.
func PanelSpanAt(bodyWidth, step int) time.Duration {
	cells, _ := chartCells(bodyWidth)
	return loaders.PanelSpan(step, cells)
}

// ChartCells is the panel chart width in cells for a body width.
func ChartCells(bodyWidth int) int {
	cells, _ := chartCells(bodyWidth)
	return cells
}

// Axis is the panel's time axis for cells chart cells at step seconds.
func Axis(step, cells int) string { return axis(step, cells) }

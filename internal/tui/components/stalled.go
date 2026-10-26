package components

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/tui"
)

// Stalled is the stalled-password banner Home and the Change set share:
// a line per screen while any run is stalled after
// a database step, and enter continuing the newest one through the same
// y/n as the Rotate list's c, then tui.ResumeMsg.
type Stalled struct {
	confirm *Confirm
	runID   string
}

// Capturing reports whether the y/n question is open (it takes every key).
func (b *Stalled) Capturing() bool { return b.confirm != nil }

// Keys are the question's hints while it is open; nil otherwise.
func (b *Stalled) Keys() []tui.KeyHint {
	if b.confirm == nil {
		return nil
	}
	return []tui.KeyHint{{Key: "y", Label: "continue", Always: true}, {Key: "n", Label: "cancel", Always: true}}
}

// Lines are the banner line, then the question while it is open; none
// while no run is stalled. Cut to width.
func (b *Stalled) Lines(ctx *tui.Ctx, width int) []string {
	runs := tui.StalledRuns(ctx)
	var out []string
	if len(runs) > 0 {
		// a run to continue from here comes before one running elsewhere
		first := runs[0]
		for _, sr := range runs {
			if !sr.Elsewhere {
				first = sr
				break
			}
		}
		icon, tone := ctx.Icons.Fail()+" ", ctx.Theme.Danger
		if first.Elsewhere {
			icon, tone = ctx.Icons.Run()+" ", ctx.Theme.Warn
		}
		l := icon + tui.StalledLine(first, len(runs)-1, width-ansi.StringWidth(icon))
		out = append(out, tone.Render(ansi.Truncate(l, width, "…")))
	}
	if b.confirm != nil {
		out = append(out, ctx.Theme.Warn.Render(b.confirm.View(width)))
	}
	return out
}

// Enter opens the question for the newest stalled run not executing in
// another instance; false when there is none (the screen then handles
// enter itself).
func (b *Stalled) Enter(ctx *tui.Ctx) bool {
	for _, sr := range tui.StalledRuns(ctx) {
		if sr.Elsewhere {
			continue
		}
		b.runID = sr.RunID
		b.confirm = &Confirm{Prompt: "continue the stalled rotation of " + sr.Label + "?"}
		return true
	}
	return false
}

// Update handles a message while the question is open: its keys, then its
// answer. handled is false for anything else. A yes resumes the run when it
// is still stalled.
func (b *Stalled) Update(ctx *tui.Ctx, msg tea.Msg) (handled bool, cmd tea.Cmd) {
	if b.confirm == nil {
		return false, nil
	}
	switch m := msg.(type) {
	case tea.KeyMsg:
		return true, b.confirm.Update(m)
	case CancelMsg:
		b.confirm, b.runID = nil, ""
		return true, nil
	case ConfirmedMsg:
		id := b.runID
		b.confirm, b.runID = nil, ""
		for _, sr := range tui.StalledRuns(ctx) {
			if sr.RunID == id && !sr.Elsewhere {
				return true, tui.Emit(tui.ResumeMsg{RunID: id})
			}
		}
		return true, tui.Flash('!', "the run changed · enter again")
	}
	return false, nil
}

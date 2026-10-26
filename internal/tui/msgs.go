package tui

import (
	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/rotation"
)

// Intent messages: screens send these as commands, the root model acts on them.

// PushMsg puts a screen on top of the stack.
type PushMsg struct{ Screen Screen }

// PopMsg removes the top screen.
type PopMsg struct{}

// OpenMsg asks the root to build a screen by name (Ctx.Open) and push it.
// Unknown names flash "! not available yet".
type OpenMsg struct {
	Name string
	Args []any
}

// FlashMsg shows a one-line message in the footer. Kind is '✓', '✗' or '!'.
type FlashMsg struct {
	Kind rune
	Text string
}

// PutChangeMsg adds or replaces a change in the change set.
type PutChangeMsg struct{ Change changeset.Change }

// RemoveChangeMsg drops a change from the change set.
type RemoveChangeMsg struct{ ID string }

// ChangesMsg applies several intents as one: the removes, then the puts,
// in order, each with the checks of RemoveChangeMsg and PutChangeMsg. When
// every one succeeds the root flashes Done (of kind Kind, '✓' when zero);
// otherwise it flashes the first failure, keeping the ones that succeeded.
type ChangesMsg struct {
	Remove []string
	Put    []changeset.Change
	Kind   rune
	Done   string
}

// QuitMsg asks the root to quit, as q at the bottom screen does.
type QuitMsg struct{}

// ToggleRedeployMsg flips the redeploy opt-out of a place.
type ToggleRedeployMsg struct{ PlaceKey string }

// ApplyMsg starts a run over the given changes.
type ApplyMsg struct{ IDs []string }

// ImmediateMsg executes one change at once, outside the change set.
type ImmediateMsg struct{ Change changeset.Change }

// ResumeMsg resumes a failed run, or one a quit or crash left running.
type ResumeMsg struct{ RunID string }

// AbandonMsg abandons a failed run, or one a quit or crash left running.
type AbandonMsg struct{ RunID string }

// RevertMsg reverts the database password of Row, a root password row
// whose run RunID stalled before any place got its new password: the root
// abandons the run (no typed word), drops the run's changes of the row's
// group from the change set, and puts a db.password change with source revert
// that logs in with the password the run set (read from its frozen set off
// the UI goroutine).
type RevertMsg struct {
	RunID string
	Row   rotation.PasswordRow
}

// SaveSettingsMsg persists the settings.
type SaveSettingsMsg struct{}

// RefreshMsg re-reads discovery data.
type RefreshMsg struct{}

// RefreshRunsMsg re-reads the runs index (Ctx.RunIndex) off the UI
// goroutine. Screens showing runs send it when they open or resume, so runs
// another instance made are seen.
type RefreshRunsMsg struct{}

// Run messages: the root model sends these while a run executes; they reach
// the top screen after the root has updated Ctx.Running.

// RunEventMsg is one step's new status in a run.
type RunEventMsg struct{ Event apply.Event }

// RunDoneMsg ends a run: Execute's report and error. Immediate runs are not
// in the live change set.
type RunDoneMsg struct {
	RunID     string
	Report    apply.Report
	Err       error
	Immediate bool
}

package root

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// SetQuitWait shortens the wait for cancelled runs; the returned func restores it.
func SetQuitWait(d time.Duration) func() {
	old := quitWait
	quitWait = d
	return func() { quitWait = old }
}

// TargetLabel exposes targetLabel, the conflict flash's wording of a target.
func (m *Model) TargetLabel(t string) string { return m.targetLabel(t) }

// SetMarshalSelection replaces the selection marshal of the prepare
// fingerprint; the returned func restores it.
func SetMarshalSelection(f func(any) ([]byte, error)) func() {
	old := marshalSelection
	marshalSelection = f
	return func() { marshalSelection = old }
}

// PrepareStep reports whether msg is a step of a prepare whose handler
// returns the next disk command (the prepare lock taken, the selection
// checked) rather than the command that starts the run.
func PrepareStep(msg tea.Msg) bool {
	switch msg.(type) {
	case preparedMsg, checkedMsg:
		return true
	}
	return false
}

// IsStartupUnlock reports whether msg is the runs list the startup unlock
// of stale held changes reads.
func IsStartupUnlock(msg tea.Msg) bool {
	_, ok := msg.(staleRunsMsg)
	return ok
}

// SetNewRunID replaces the run id a prepare freezes under; the returned
// func restores it.
func SetNewRunID(f func() string) func() {
	old := newRunID
	newRunID = f
	return func() { newRunID = old }
}

// SetFreezeQuitWait shortens how long quitting waits for a run being frozen
// after quitWait passed; the returned func restores it.
func SetFreezeQuitWait(d time.Duration) func() {
	old := freezeQuitWait
	freezeQuitWait = d
	return func() { freezeQuitWait = old }
}

package loaders

import (
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/apply"
)

// RunsMsg is the runs index of the work dir (Runs.List), read off the UI
// goroutine. Warnings are the fixed texts of the runs that were skipped,
// Corrupt their ids.
type RunsMsg struct {
	Runs     []apply.RunInfo
	Warnings []string
	Corrupt  []string
}

// Runs reads the runs index.
func Runs(r *apply.Runs) tea.Cmd {
	return func() tea.Msg {
		infos, errs := r.List()
		msg := RunsMsg{Runs: infos}
		for _, err := range errs {
			msg.Warnings = append(msg.Warnings, err.Error())
			var re *apply.RunError
			if errors.As(err, &re) {
				msg.Corrupt = append(msg.Corrupt, re.RunID)
			}
		}
		return msg
	}
}

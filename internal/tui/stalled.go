package tui

import (
	"path/filepath"
	"strconv"

	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/view"
)

// RotateRuns is what the views read of the runs: the runs index and the
// live state of each run this process executes.
func RotateRuns(ctx *Ctx) view.RotateRuns {
	r := view.RotateRuns{Runs: ctx.RunIndex.Runs}
	for id, p := range ctx.Running {
		if p.State == "" {
			continue
		}
		if r.Live == nil {
			r.Live = map[string]string{}
		}
		r.Live[id] = p.State
	}
	return r
}

// StalledRuns are the stalled database steps of the runs (view.Stalled),
// newest run first; none before the views and the store exist.
func StalledRuns(ctx *Ctx) []view.StalledRun {
	if ctx.Views == nil || ctx.Store == nil {
		return nil
	}
	return ctx.Views.Stalled(RotateRuns(ctx))
}

// StalledLine is the banner text of a stalled run,
// the longest that fits width cells: "<row>: password changed, N of M apps
// not redeployed · enter continue", shorter forms after it; a run
// executing in another instance reads "<row>: running elsewhere (pid N)".
// more counts the other stalled runs.
func StalledLine(sr view.StalledRun, more, width int) string {
	what, units := "password changed", "apps"
	if sr.KeyFile {
		what, units = "KEYFILE changed", "nodes"
	}
	tail := " · enter continue"
	if sr.Elsewhere {
		tail = ""
	}
	if more > 0 {
		tail += " (+" + strconv.Itoa(more) + " more)"
	}
	var forms []string
	switch {
	case sr.Elsewhere:
		pid := ""
		if sr.HolderPID > 0 {
			pid = " (pid " + strconv.Itoa(sr.HolderPID) + ")"
		}
		forms = append(forms, sr.Label+": "+what+", running elsewhere"+pid+tail, sr.Label+": running elsewhere"+pid+tail)
	case sr.NotRedeployed > 0:
		n, m := strconv.Itoa(sr.NotRedeployed), strconv.Itoa(sr.Redeploys)
		forms = append(forms,
			sr.Label+": "+what+", "+n+" of "+m+" "+units+" not redeployed"+tail,
			sr.Label+": changed, "+n+" of "+m+" not redeployed"+tail,
			sr.Label+": "+n+" of "+m+" not redeployed"+tail)
	default:
		forms = append(forms, sr.Label+": "+what+", not verified"+tail)
	}
	forms = append(forms, sr.Label+": stalled"+tail)
	for _, f := range forms {
		if ansi.StringWidth(f) <= width {
			return f
		}
	}
	return forms[len(forms)-1]
}

// RunElsewhere is the refusal for run runID, which process pid executes in
// another app instance: its pid, and how to release the run should that
// instance be gone — remove the lock file only, never the run directory
// (its journal is the only record that a database password changed).
// Sanitized; home shown as ~.
func RunElsewhere(workDir, runID string, pid int) string {
	lock := shortHome(filepath.Join(workDir, "runs", runID, "lock"))
	n := strconv.Itoa(pid)
	return "run " + text.Sanitize(runID) + " is running in another window (pid " + n + ") · if pid " + n + " has exited, remove " + text.Sanitize(lock) + ", then restart"
}

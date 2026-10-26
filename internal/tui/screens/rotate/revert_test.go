package rotate_test

import (
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/tui"
)

const stalledNew = "stalled-new-password-0123456789"

// revertRun is a run stalled after its database step changed mongo's root
// password, before any place got the new one (its patches failed).
func revertRun(state string) apply.RunInfo {
	pl := domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "mongo"}
	return apply.RunInfo{
		ID: "r1", State: state, Created: now,
		Steps: map[string]apply.StepRecord{
			"dbpw:e1/mongo:mongo": {Status: apply.StatusDone, Acted: true},
			"patch:e1":            {Status: apply.StatusFailed},
			"redeploy:e1/worker":  {Status: apply.StatusBlocked},
		},
		Frozen: []apply.FrozenChange{{ID: "pw", Kind: "db.password", Group: "rotate-db:e1/mongo:mongo", Target: "mongo", Place: pl, Fingerprint: rotation.Fingerprint(stalledNew)}},
	}
}

// R on a stalled root row whose new password no place holds asks, then
// sends RevertMsg with the row; withheld once a place holds it.
func TestPasswordRevert(t *testing.T) {
	c := dbCtx(100, 30)
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: []apply.RunInfo{revertRun(apply.RunFailed)}}
	s := to(t, c, open(t, c), "mongo root")
	got := dbCheck(t, c, s)
	for _, want := range []string{"R revert", "R reverts the database password: no place holds the new one"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	s, ms := dbPress(t, c, s, r("R"))
	if len(ms) != 0 || !strings.Contains(frame(c, s), "set mongo root back to the password in Railway now? (y/n)") {
		t.Fatalf("R did not ask: %#v\n%s", ms, frame(c, s))
	}
	s, ms = dbPress(t, c, s, r("y"))
	rv, ok := ms[0].(tui.RevertMsg)
	if len(ms) != 1 || !ok || rv.RunID != "r1" || rv.Row.Key != "rotate-db:e1/mongo:mongo" || !rv.Row.Root {
		t.Fatalf("R y: %#v", ms)
	}
	if _, ms = dbPress(t, c, s, r("R"), r("n")); len(ms) != 0 {
		t.Errorf("R n: %#v", ms)
	}

	// a patch of the row's environment done: a place holds the new
	// password, only continue is left
	held := revertRun(apply.RunFailed)
	held.Steps["patch:e1"] = apply.StepRecord{Status: apply.StatusDone, Acted: true}
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: []apply.RunInfo{held}}
	s = to(t, c, open(t, c), "mongo root")
	if got := dbCheck(t, c, s); strings.Contains(got, "R revert") {
		t.Errorf("revert offered while a place holds the new password:\n%s", got)
	}
	_, ms = dbPress(t, c, s, r("R"))
	if f := flashText(ms); f != "✗ a place holds the new password · c continues" {
		t.Errorf("R withheld: %q", f)
	}
	// not stalled: nothing to revert
	c.RunIndex = tui.RunIndex{Loaded: true}
	s = to(t, c, open(t, c), "mongo root")
	if _, ms = dbPress(t, c, s, r("R")); len(ms) != 0 {
		t.Errorf("R on a row without a stalled run: %#v", ms)
	}
}

// A run executing in another window is shown as such and takes no
// decision here.
func TestPasswordElsewhere(t *testing.T) {
	c := dbCtx(100, 30)
	run := revertRun(apply.RunRunning)
	run.Elsewhere = true
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: []apply.RunInfo{run}}
	s := to(t, c, open(t, c), "mongo root")
	if got := dbCheck(t, c, s); !strings.Contains(got, "⟳ another window") {
		t.Errorf("state missing:\n%s", got)
	}
	for _, k := range []string{" ", "c", "R"} {
		_, ms := dbPress(t, c, s, r(k))
		if f := flashText(ms); len(ms) != 1 || f != "✗ running in another window" {
			t.Errorf("%q: %#v", k, ms)
		}
	}
}

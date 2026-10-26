package changeset_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/tui"
	cs "github.com/men232/railway-manager/internal/tui/screens/changeset"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

const newPassword = "new-db-password-0123456789"

// The Change set carries the stalled banner too: enter on a run row opens
// the run, elsewhere it continues the stalled run after y/n.
func TestChangeSetStalledBanner(t *testing.T) {
	for _, w := range []int{80, 60} {
		f := newCtx(t, w, 24)
		dir := t.TempDir()
		tuitest.StalledRun(t, dir, "r1", now, place("s1"), place("s2"), newPassword)
		f.ctx.Runs = &apply.Runs{Dir: dir}
		f.ctx.RunIndex = indexOf(t, f.ctx.Runs)

		s := cs.New()
		out := frame(f.ctx, s)
		tuitest.NoSecrets(t, out, secret, newPassword)
		tuitest.Golden(t, "changeset_stalled_"+itoa(w)+".golden", out)
		if w == 80 && !strings.Contains(out, "✗ api root: password changed, 1 of 1 apps not redeployed · enter continue") {
			t.Errorf("banner missing:\n%s", out)
		}

		// the stalled run is the first cursor row: enter opens it
		_, ms := press(f.ctx, s, enter)
		if want := (tui.OpenMsg{Name: "run", Args: []any{"r1"}}); len(ms) != 1 || !reflect.DeepEqual(ms[0], want) {
			t.Fatalf("enter on the run row: %#v", ms)
		}
		// on a change row enter asks, then resumes
		s, ms = press(f.ctx, s, down, enter)
		if len(ms) != 0 || !strings.Contains(frame(f.ctx, s), "continue the stalled rotation of api root? (y/n)") {
			t.Fatalf("enter on a change row: %#v\n%s", ms, frame(f.ctx, s))
		}
		_, ms = press(f.ctx, s, r("y"))
		if len(ms) != 1 || ms[0] != (tui.ResumeMsg{RunID: "r1"}) {
			t.Errorf("y: %#v", ms)
		}
	}
}

func itoa(w int) string {
	if w == 80 {
		return "80"
	}
	return "60"
}

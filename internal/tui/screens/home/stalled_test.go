package home_test

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/tui/screens/home"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

const newPassword = "new-db-password-0123456789"

// withStalled reads a run stalled after its database step (written to disk
// as an apply leaves it) into the runs index, as the root does.
func withStalled(t *testing.T, c *tui.Ctx) {
	t.Helper()
	dir := t.TempDir()
	tuitest.StalledRun(t, dir, "r1", c.Now(),
		domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"},
		domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s2"}, newPassword)
	c.Runs = &apply.Runs{Dir: dir}
	infos, errs := c.Runs.List()
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: infos}
	c.Views = &view.Views{Store: c.Store, Reg: kinds.NewRegistry()}
}

// press sends keys, feeding the confirm's answers back to the screen.
func press(c *tui.Ctx, s tui.Screen, keys ...tea.KeyMsg) (tui.Screen, []tea.Msg) {
	var out []tea.Msg
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch m := cmd().(type) {
		case components.ConfirmedMsg, components.CancelMsg:
			var next tea.Cmd
			s, next = s.Update(c, m)
			run(next)
		case nil:
		default:
			out = append(out, m)
		}
	}
	for _, k := range keys {
		var cmd tea.Cmd
		s, cmd = s.Update(c, k)
		run(cmd)
	}
	return s, out
}

var enterKey = tea.KeyMsg{Type: tea.KeyEnter}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// The banner shows a run stalled after its database step, from its journal
// and frozen actions; enter continues it after y/n.
func TestHomeStalledBanner(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 22)
		loaded(c)
		withStalled(t, c)
		s := home.New()
		out := render(c, s)
		tuitest.NoSecrets(t, out, secret, newPassword)
		tuitest.Golden(t, "home_"+itoa(w)+"_stalled.golden", out)
		if w == 80 && !strings.Contains(out, "✗ api root: password changed, 1 of 1 apps not redeployed · enter continue") {
			t.Errorf("banner missing:\n%s", out)
		}
	}

	c := ctx(80, 22)
	loaded(c)
	withStalled(t, c)
	s, ms := press(c, home.New(), enterKey)
	if len(ms) != 0 {
		t.Fatalf("enter resumed without asking: %#v", ms)
	}
	if out := render(c, s); !strings.Contains(out, "continue the stalled rotation of api root? (y/n)") {
		t.Errorf("question missing:\n%s", out)
	}
	// keys go to the question: s does not open Services
	s, ms = press(c, s, key("n"))
	if len(ms) != 0 || strings.Contains(render(c, s), "(y/n)") {
		t.Errorf("n: msgs %#v", ms)
	}
	_, ms = press(c, s, enterKey, key("y"))
	if len(ms) != 1 || ms[0] != (tui.ResumeMsg{RunID: "r1"}) {
		t.Errorf("y: msgs %#v, want ResumeMsg r1", ms)
	}

	// executing in another instance: shown as running elsewhere with its
	// pid; enter does not continue it
	cw := ctx(80, 22)
	loaded(cw)
	withStalled(t, cw)
	cw.RunIndex.Runs[0].State = apply.RunRunning
	cw.RunIndex.Runs[0].Elsewhere, cw.RunIndex.Runs[0].HolderPID = true, 4242
	sw := home.New()
	if out := render(cw, sw); !strings.Contains(out, "⟳ api root: password changed, running elsewhere (pid 4242)") || strings.Contains(out, "enter continue") {
		t.Errorf("elsewhere banner:\n%s", out)
	}
	if _, ms := press(cw, sw, enterKey, key("y")); len(ms) != 0 {
		t.Errorf("enter continued a run executing elsewhere: %#v", ms)
	}

	// applying here: no banner, enter opens the item under the cursor
	c.Running = map[string]tui.RunProgress{"r1": {State: apply.RunRunning}}
	s = home.New()
	if out := render(c, s); strings.Contains(out, "password changed") {
		t.Errorf("banner while applying:\n%s", out)
	}
	if _, ms := press(c, s, enterKey); len(ms) != 1 || ms[0].(tui.OpenMsg).Name != "services" {
		t.Errorf("enter without a stalled run: %#v", ms)
	}
}

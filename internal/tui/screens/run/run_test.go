package run_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/dbops"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/tui/screens/run"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

const secret = "fixture-value-alpha"

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func writeRun(t *testing.T, dir, id, state string, steps map[string]apply.StepRecord) {
	t.Helper()
	d := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"runId": id, "state": state, "created": now, "steps": steps, "changes": map[string]any{}})
	if err := os.WriteFile(filepath.Join(d, "journal.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "changeset.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rec(status, errText string) apply.StepRecord {
	return apply.StepRecord{Status: status, Error: errText, At: now}
}

func newCtx(t *testing.T, w, h int, state string) *tui.Ctx {
	t.Helper()
	dir := t.TempDir()
	writeRun(t, dir, "r2", state, map[string]apply.StepRecord{
		"1-patch":    rec(apply.StatusDone, ""),
		"2-redeploy": rec(apply.StatusFailed, "deploy crashed\nsecond line "+secret),
		"3-restart":  rec(apply.StatusStarted, ""),
		"4-patch":    rec(apply.StatusPending, ""),
		"5-after":    rec(apply.StatusBlocked, ""),
		"6-noop":     rec(apply.StatusSkipped, "already applied"),
		"7-rebuild":  rec(apply.StatusDone, "rebuild failed\x1b[31m"),
	})
	runs := &apply.Runs{Dir: dir}
	infos, errs := runs.List()
	if len(errs) != 0 {
		t.Fatalf("List: %v", errs)
	}
	return &tui.Ctx{
		Runs:     runs,
		RunIndex: tui.RunIndex{Loaded: true, Runs: infos},
		Theme:    tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Now: func() time.Time { return now },
	}
}

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func settle(c *tui.Ctx, s tui.Screen, cmd tea.Cmd) (tui.Screen, []tea.Msg) {
	var out []tea.Msg
	var runCmd func(tea.Cmd)
	runCmd = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch m := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range m {
				runCmd(c)
			}
		case components.ConfirmedMsg, components.CancelMsg:
			var next tea.Cmd
			s, next = s.Update(c, m)
			runCmd(next)
		case nil:
		default:
			out = append(out, m)
		}
	}
	runCmd(cmd)
	return s, out
}

func press(c *tui.Ctx, s tui.Screen, keys ...tea.KeyMsg) (tui.Screen, []tea.Msg) {
	var all []tea.Msg
	for _, k := range keys {
		var cmd tea.Cmd
		var ms []tea.Msg
		s, cmd = s.Update(c, k)
		s, ms = settle(c, s, cmd)
		all = append(all, ms...)
	}
	return s, all
}

func body(c *tui.Ctx, s tui.Screen) string {
	return ansi.Strip(s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows))
}

// note is the run's line in the frame's header.
func note(c *tui.Ctx, s tui.Screen) string { return s.(tui.Noter).Note(c) }

// hasRow reports whether v has a line with the parts in order, apart by
// any spaces (a step's words, then at the right its error or note).
func hasRow(v string, parts ...string) bool {
	q := make([]string, len(parts))
	for i, p := range parts {
		q[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(strings.Join(q, `\s+`)).MatchString(v)
}

func frame(c *tui.Ctx, s tui.Screen) string {
	return tuitest.Plain(tui.Frame(c, s.Title(), note(c, s), s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
}

func TestRunViewResumeOnlyWhenFailed(t *testing.T) {
	c := newCtx(t, 80, 24, apply.RunFailed)
	_, ms := press(c, run.New("r2"), r("r"))
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], tui.ResumeMsg{RunID: "r2"}) {
		t.Fatalf("failed: msgs = %#v", ms)
	}
	// Running here: refused.
	c = newCtx(t, 80, 24, apply.RunRunning)
	c.Running = map[string]tui.RunProgress{"r2": {State: apply.RunRunning}}
	for _, k := range []string{"r", "X"} {
		_, ms = press(c, run.New("r2"), r(k))
		if len(ms) != 1 {
			t.Fatalf("running %s: msgs = %#v", k, ms)
		}
		if f, ok := ms[0].(tui.FlashMsg); !ok || f.Kind != '!' {
			t.Fatalf("running %s: msg = %#v", k, ms[0])
		}
	}
	// Running on disk only (the app quit or crashed): resumable.
	c = newCtx(t, 80, 24, apply.RunRunning)
	_, ms = press(c, run.New("r2"), r("r"))
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], tui.ResumeMsg{RunID: "r2"}) {
		t.Fatalf("interrupted: msgs = %#v", ms)
	}
	s, ms := press(c, run.New("r2"), r("X"))
	if len(ms) != 0 || !s.(tui.Capturer).Capturing() {
		t.Fatalf("interrupted abandon: msgs = %#v", ms)
	}
	// Finished: refused.
	c = newCtx(t, 80, 24, apply.RunFinished)
	if _, ms = press(c, run.New("r2"), r("r")); len(ms) != 1 {
		t.Fatalf("finished: msgs = %#v", ms)
	} else if _, ok := ms[0].(tui.FlashMsg); !ok {
		t.Fatalf("finished: msg = %#v", ms[0])
	}
}

func TestRunViewAbandon(t *testing.T) {
	c := newCtx(t, 80, 24, apply.RunFailed)
	s, ms := press(c, run.New("r2"), r("X"))
	if len(ms) != 0 || !s.(tui.Capturer).Capturing() {
		t.Fatalf("msgs = %#v", ms)
	}
	if v := body(c, s); !strings.Contains(v, "abandon run r2? written changes stay as they are (y/n)") {
		t.Errorf("prompt:\n%s", v)
	}
	_, ms = press(c, s, r("y"))
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], tui.AbandonMsg{RunID: "r2"}) {
		t.Fatalf("msgs = %#v", ms)
	}
	s, _ = press(c, run.New("r2"), r("X"))
	s, ms = press(c, s, r("n"))
	if len(ms) != 0 || s.(tui.Capturer).Capturing() {
		t.Errorf("n should cancel: %#v", ms)
	}
}

func TestRunViewErrorsAndGlyphs(t *testing.T) {
	c := newCtx(t, 100, 24, apply.RunFailed)
	s := run.New("r2")
	v := body(c, s)
	if n := note(c, s); !strings.HasPrefix(n, "r2 · failed · 3/7") {
		t.Errorf("note %q", n)
	}
	for _, want := range [][]string{
		{"✓ 1-patch"}, {"✗ 2-redeploy", "deploy crashed"}, {"⟳ 3-restart"}, {"○ 4-patch"}, {"⊘ 5-after"},
		{"» 6-noop", "already applied"},
		{"✓ 7-rebuild", "rebuild failed"}, // a failed rebuild: done with an error
	} {
		if !hasRow(v, want...) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "second line") || strings.Contains(v, "\x1b") {
		t.Errorf("only the first sanitized error line belongs here:\n%s", v)
	}
}

func TestRunViewMergesLiveEvents(t *testing.T) {
	c := newCtx(t, 100, 24, apply.RunRunning)
	c.Running = map[string]tui.RunProgress{"r2": {Done: 4, Total: 9, State: apply.RunRunning}}
	s, _ := press(c, run.New("r2"))
	s, _ = s.Update(c, tui.RunEventMsg{Event: apply.Event{RunID: "r2", StepKey: "4-patch", Status: apply.StatusFailed, Error: "boom"}})
	v := body(c, s)
	if !strings.HasPrefix(note(c, s), "r2 · running · 4/9") || !hasRow(v, "✗ 4-patch", "boom") {
		t.Errorf("live merge:\n%s", v)
	}
}

func TestRunViewBuild(t *testing.T) {
	if run.Build("r1") == nil {
		t.Error("Build(id) = nil")
	}
	for _, args := range [][]any{nil, {1}, {"a", "b"}, {""}} {
		if run.Build(args...) != nil {
			t.Errorf("Build(%v) should be nil", args)
		}
	}
}

func TestRunViewGoldens(t *testing.T) {
	for name, w := range map[string]int{"80": 80, "60": 60} {
		c := newCtx(t, w, 24, apply.RunFailed)
		out := frame(c, run.New("r2"))
		tuitest.NoSecrets(t, out, secret)
		tuitest.Golden(t, "run_"+name+".golden", out)
	}
}

func TestRunViewLiveEventNotDoubleCounted(t *testing.T) {
	c := newCtx(t, 100, 24, apply.RunRunning)
	s, _ := press(c, run.New("r2"))
	// 4-patch is pending in the journal; the live event says done.
	s, _ = s.Update(c, tui.RunEventMsg{Event: apply.Event{RunID: "r2", StepKey: "4-patch", Status: apply.StatusDone}})
	v := body(c, s)
	if !strings.Contains(note(c, s), "running · 4/7") || strings.Count(v, "4-patch") != 1 {
		t.Errorf("double counted:\n%s", v)
	}
}

func TestRunViewUnknownErrorKeyNoSyntheticStep(t *testing.T) {
	c := newCtx(t, 100, 24, apply.RunRunning)
	c.Running = map[string]tui.RunProgress{"r2": {State: apply.RunRunning, Errors: map[string]string{"ghost": "x"}}}
	if v := body(c, run.New("r2")); strings.Contains(v, "ghost") {
		t.Errorf("synthetic step:\n%s", v)
	}
}

func TestRunViewStrayConfirmedIgnored(t *testing.T) {
	c := newCtx(t, 80, 24, apply.RunFailed)
	_, cmd := run.New("r2").Update(c, components.ConfirmedMsg{})
	if cmd != nil {
		t.Error("stray ConfirmedMsg emitted a command")
	}
}

// A corrupt run is skipped by the runs index and reported: its view says
// so instead of "not found", and holds nothing of the files.
func TestRunViewCorruptRunNotNotFound(t *testing.T) {
	c := newCtx(t, 80, 24, apply.RunFailed)
	d := filepath.Join(c.Runs.Dir, "runs", "bad")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "journal.json"), []byte("{zx"+secret), 0o644); err != nil {
		t.Fatal(err)
	}
	infos, errs := c.Runs.List()
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: infos, Corrupt: []string{"bad"}}
	if len(infos) != 1 || len(errs) != 1 {
		t.Fatalf("List = %d runs, %v", len(infos), errs)
	}
	v := body(c, run.New("bad"))
	if strings.Contains(v, "not found") || !strings.Contains(v, "unavailable") || !strings.Contains(v, "(keep it: it may record a database password change), then restart") || strings.Contains(v, secret) {
		t.Errorf("view:\n%s", v)
	}
	// The other runs still show.
	if v := note(c, run.New("r2")); !strings.HasPrefix(v, "r2 · ") {
		t.Errorf("r2 view:\n%s", v)
	}
}

func TestRunViewBeforeIndexLoaded(t *testing.T) {
	c := newCtx(t, 80, 24, apply.RunFailed)
	c.RunIndex = tui.RunIndex{}
	if v := body(c, run.New("r2")); !strings.Contains(v, "reading runs") {
		t.Errorf("view:\n%s", v)
	}
}

func TestRunViewFilterNarrowsSteps(t *testing.T) {
	c := newCtx(t, 100, 24, apply.RunFailed)
	s, _ := press(c, run.New("r2"), r("/"), r("patch"))
	v := body(c, s)
	if !strings.Contains(v, "1-patch") || !strings.Contains(v, "4-patch") || strings.Contains(v, "3-restart") || !strings.Contains(v, "2/7") {
		t.Errorf("filtered:\n%s", v)
	}
	if c.Filters["run:r2"] != "patch" {
		t.Errorf("saved under %v", c.Filters)
	}
}

// A preflight that would not connect to an ssh host not trusted yet: t shows
// the key's fingerprint, and the typed word trusts it in the work dir's
// known hosts file, so r can resume; esc trusts nothing.
func TestRunViewTrustsHostKey(t *testing.T) {
	const fp = "SHA256:u1QuOrYHORJv/vPkg03o7Lm+Tnjj+8j0VzPomvJuDOU"
	dir := t.TempDir()
	pre := rec(apply.StatusFailed, "the ssh host key of ssh.railway.com is not trusted yet")
	pre.Data = json.RawMessage(`{"untrusted":{"host":"ssh.railway.com","keys":["ssh.railway.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIC/3HMGJU6ZTAKmx4j6OvVVZMxv/6Wx2e+nrJWCfN5xu"],"fingerprints":["` + fp + `"]}}`)
	writeRun(t, dir, "r3", apply.RunFailed, map[string]apply.StepRecord{"preflight:e1/smongo:root": pre})
	runs := &apply.Runs{Dir: dir}
	infos, errs := runs.List()
	if len(errs) != 0 {
		t.Fatalf("List: %v", errs)
	}
	c := &tui.Ctx{
		Runs: runs, RunIndex: tui.RunIndex{Loaded: true, Runs: infos},
		Theme: tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: 100, Height: 24, IsTTY: true, Now: func() time.Time { return now },
	}
	known := dbops.KnownHostsFile(dir)

	if !strings.Contains(frame(c, run.New("r3")), "t trust ssh key") {
		t.Errorf("no trust hint:\n%s", frame(c, run.New("r3")))
	}
	s, _ := press(c, run.New("r3"), r("t"))
	if v := body(c, s); !strings.Contains(v, fp) || !strings.Contains(v, `type "trust"`) {
		t.Fatalf("trust question:\n%s", v)
	}
	s, _ = press(c, s, tea.KeyMsg{Type: tea.KeyEsc})
	if ok, _ := dbops.Trusted(known, "ssh.railway.com"); ok || s.(tui.Capturer).Capturing() {
		t.Fatal("esc trusted the key")
	}
	s, _ = press(c, s, r("t"))
	for _, ch := range "trust" {
		s, _ = press(c, s, r(string(ch)))
	}
	_, ms := press(c, s, tea.KeyMsg{Type: tea.KeyEnter})
	if ok, err := dbops.Trusted(known, "ssh.railway.com"); !ok || err != nil {
		t.Fatalf("not trusted after confirming: %v, %v", ok, err)
	}
	if len(ms) != 1 || !strings.Contains(ms[0].(tui.FlashMsg).Text, "r resumes") {
		t.Errorf("msgs = %#v", ms)
	}
}

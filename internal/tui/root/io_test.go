package root_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/root"
)

// runDirs lists the names under runs/.
func runDirs(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func prepareLockHeld(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "prepare.lock"))
	return err == nil
}

// refChange is B_NEW = ${{A_NEW}}: it reads what A_NEW writes.
func refChange() changeset.Change {
	ref := setChange("B_NEW", "${{A_NEW}}")
	nv, _ := json.Marshal(map[string]any{"value": "${{A_NEW}}", "refs": []string{domain.LocationID("e1", "s1", "A_NEW")}})
	ref.New = nv
	return ref
}

// toCheck feeds an apply of ids and the prepare lock it took, and returns
// the command that checks and compiles the selection.
func (r *rig) toCheck(ids []string) tea.Cmd {
	r.t.Helper()
	cmd := r.update(tui.ApplyMsg{IDs: ids})
	if cmd == nil {
		r.t.Fatalf("apply returned no command:\n%s", r.view())
	}
	check := r.update(cmd())
	if check == nil {
		r.t.Fatalf("no check command:\n%s", r.view())
	}
	return check
}

// An unselected change added while the selection is checked off the UI
// goroutine (here one that reads what the selection writes) is not missed:
// the check ran on a copy, the live set's Rev moved, the apply is refused
// and nothing is written. The edit runs concurrently with the check (-race).
func TestPrepareRefusesChangeAddedWhileChecking(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	a := r.cs.Changes[0].ID
	before := runDirs(t, r.dir)

	check := r.toCheck([]string{a})
	got := make(chan tea.Msg, 1)
	go func() { got <- check() }()
	r.drain(r.update(tui.PutChangeMsg{Change: refChange()}))
	msg := <-got
	if !root.PrepareStep(msg) {
		t.Fatalf("check gave %T", msg)
	}
	r.drain(r.update(msg))

	if v := r.view(); !strings.Contains(v, "✗ the change set changed while preparing · apply again") {
		t.Errorf("flash missing:\n%s", v)
	}
	if got := runDirs(t, r.dir); !slices.Equal(got, before) {
		t.Errorf("runs = %v, want %v", got, before)
	}
	if prepareLockHeld(r.dir) {
		t.Errorf("prepare lock left held")
	}
	for _, c := range r.cs.Changes {
		if c.RunID != "" {
			t.Errorf("refused selection locked: %+v", c)
		}
	}
	// Applying again sees the new change.
	r.drain(r.update(tui.ApplyMsg{IDs: []string{a}}))
	if v := r.view(); !strings.Contains(v, "✗ an unselected change needs shop/production api.A_NEW: include it or remove it") {
		t.Errorf("conflict not found on the second apply:\n%s", v)
	}
}

// The checked selection is locked in the live set (not saved yet) while it
// freezes; the run exists and the lock is released before its result lands;
// only then is the set saved and the run executed. A second apply waits.
func TestPrepareLocksFreezesSavesExecutesInOrder(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("C_NEW", "c")}))
	a := r.cs.Changes[0].ID

	check := r.toCheck([]string{a})
	if !prepareLockHeld(r.dir) {
		t.Fatal("prepare lock not held while checking")
	}
	checked := check()
	freeze := r.update(checked)
	if freeze == nil {
		t.Fatalf("no freeze command:\n%s", r.view())
	}
	runID := r.cs.Changes[0].RunID
	if runID == "" || r.cs.Changes[1].RunID != "" {
		t.Fatalf("live set not locked by the checked selection: %+v", r.cs.Changes)
	}
	if got := runDirs(t, r.dir); len(got) != 0 {
		t.Errorf("run written before the freeze: %v", got)
	}
	if saved, _ := changeset.Load(r.deps.ChangeSetPath); slices.ContainsFunc(saved.Changes, func(c changeset.Change) bool { return c.RunID != "" }) {
		t.Errorf("lock saved before the freeze: %+v", saved.Changes)
	}
	if c := r.update(tui.ApplyMsg{IDs: []string{r.cs.Changes[1].ID}}); c != nil {
		t.Errorf("second apply while freezing returned a command")
	}
	if v := r.view(); !strings.Contains(v, "a run is being prepared") {
		t.Errorf("no preparing flash:\n%s", v)
	}

	frozen := freeze()
	if got := runDirs(t, r.dir); !slices.Equal(got, []string{runID}) {
		t.Errorf("runs after the freeze = %v, want %s", got, runID)
	}
	if prepareLockHeld(r.dir) {
		t.Errorf("prepare lock held after the freeze")
	}
	if s := journalState(t, r.dir, runID); s != apply.RunRunning {
		t.Errorf("state = %q", s)
	}
	cmds := split(r.update(frozen))
	if len(cmds) != 2 {
		t.Fatalf("start commands = %d", len(cmds))
	}
	r.update(cmds[0]()) // save
	saved, _ := changeset.Load(r.deps.ChangeSetPath)
	if len(saved.Changes) != 2 || saved.Changes[0].RunID != runID {
		t.Errorf("saved = %+v", saved.Changes)
	}
	r.drain(cmds[1])
	if s := journalState(t, r.dir, runID); s != apply.RunFinished {
		t.Errorf("state after execute = %q", s)
	}
	if v, _ := r.fake.Var("e1", "s1", "A_NEW"); v != "a" {
		t.Errorf("not applied: %q", v)
	}
	// Preparing is over: another apply goes ahead.
	r.drain(r.update(tui.ApplyMsg{}))
	if len(r.cs.Changes) != 0 {
		t.Errorf("second apply: %+v\n%s", r.cs.Changes, r.view())
	}
}

// A freeze that fails (here: the run id is taken) unlocks the selection in
// the live set, with its old flags, leaves the existing directory alone and
// ends the prepare.
func TestPrepareFreezeFailureUnlocks(t *testing.T) {
	const taken = "20261024-120000-dead"
	defer root.SetNewRunID(func() string { return taken })()
	r := newRig(t)
	r.drain(r.m.Init())
	c := setChange("A_NEW", "a")
	r.drain(r.update(tui.PutChangeMsg{Change: c}))
	r.cs.Changes[0].Flag = "failed: earlier"
	other := filepath.Join(r.dir, "runs", taken)
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	freeze := r.update(r.toCheck(nil)())
	if freeze == nil {
		t.Fatalf("no freeze command:\n%s", r.view())
	}
	if r.cs.Changes[0].RunID != taken {
		t.Fatalf("not locked: %+v", r.cs.Changes)
	}
	r.drain(r.update(freeze()))

	if got := r.cs.Changes[0]; got.RunID != "" || got.Flag != "failed: earlier" {
		t.Errorf("after a failed freeze: %+v", got)
	}
	if v := r.view(); !strings.Contains(v, "✗") {
		t.Errorf("no failure flash:\n%s", v)
	}
	if _, err := os.Stat(filepath.Join(other, "keep")); err != nil {
		t.Errorf("the taken run dir was touched: %v", err)
	}
	if got := runDirs(t, r.dir); !slices.Equal(got, []string{taken}) {
		t.Errorf("runs = %v", got)
	}
	if prepareLockHeld(r.dir) {
		t.Errorf("prepare lock left held")
	}
	if saved, _ := changeset.Load(r.deps.ChangeSetPath); len(saved.Changes) != 1 || saved.Changes[0].RunID != "" {
		t.Errorf("saved = %+v", saved.Changes)
	}
	root.SetNewRunID(apply.NewRunID)
	r.drain(r.update(tui.ApplyMsg{}))
	if len(r.cs.Changes) != 0 {
		t.Errorf("apply after a failed freeze: %+v\n%s", r.cs.Changes, r.view())
	}
}

// Quitting while the run freezes waits for it; the frozen run is not
// executed but saved as held, so it can be resumed.
func TestQuitWhileFreezing(t *testing.T) {
	defer root.SetQuitWait(time.Millisecond)()
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	freeze := r.update(r.toCheck(nil)())
	runID := r.cs.Changes[0].RunID
	for _, m := range runAll(r.update(key("q"))) {
		if _, ok := m.(tea.QuitMsg); ok {
			t.Fatal("quit while freezing did not wait")
		}
	}
	quit := false
	for _, m := range runAll(r.update(freeze())) {
		if _, ok := m.(tea.QuitMsg); ok {
			quit = true
		}
	}
	if !quit {
		t.Error("no quit after the freeze")
	}
	if s := journalState(t, r.dir, runID); s != apply.RunRunning {
		t.Errorf("state = %q, want running (not executed)", s)
	}
	if saved, _ := changeset.Load(r.deps.ChangeSetPath); len(saved.Changes) != 1 || saved.Changes[0].RunID != runID {
		t.Errorf("saved = %+v", saved.Changes)
	}
	if n := len(r.fake.Mutations()); n != 0 {
		t.Errorf("mutations = %d", n)
	}
}

// While an abandon is on disk, the run cannot be resumed or abandoned again;
// its result unlocks the run's changes and saves.
func TestAbandonInCommand(t *testing.T) {
	runID, dir := interruptedRun(t)
	r := restarted(t, dir, runID)
	cmd := r.update(tui.AbandonMsg{RunID: runID})
	if cmd == nil {
		t.Fatal("abandon returned no command")
	}
	if s := journalState(t, dir, runID); s != apply.RunRunning {
		t.Errorf("abandoned in Update: %q", s)
	}
	if c := r.update(tui.ResumeMsg{RunID: runID}); c != nil {
		t.Errorf("resume while abandoning returned a command")
	}
	if v := r.view(); !strings.Contains(v, "run "+runID+" is being abandoned") {
		t.Errorf("no refusal:\n%s", v)
	}
	if c := r.update(tui.AbandonMsg{RunID: runID}); c != nil {
		t.Errorf("second abandon returned a command")
	}
	msg := cmd()
	if s := journalState(t, dir, runID); s != apply.RunAbandoned {
		t.Errorf("state = %q", s)
	}
	if r.m.Ctx().CS.Changes[0].RunID == "" {
		t.Errorf("unlocked before the abandon result")
	}
	r.drain(r.update(msg))
	if r.m.Ctx().CS.Changes[0].RunID != "" {
		t.Errorf("not unlocked: %+v", r.m.Ctx().CS.Changes)
	}
	if v := r.view(); !strings.Contains(v, "✓ abandoned run "+runID) {
		t.Errorf("no flash:\n%s", v)
	}
	if saved, _ := changeset.Load(filepath.Join(dir, "changeset.json")); len(saved.Changes) != 1 || saved.Changes[0].RunID != "" {
		t.Errorf("saved = %+v", saved.Changes)
	}
	// It may be resumed again once the abandon landed (it is no longer
	// unfinished, so that is refused by its state, not as being abandoned).
	r.drain(r.update(tui.ResumeMsg{RunID: runID}))
	if v := r.view(); !strings.Contains(v, "is not unfinished") {
		t.Errorf("resume after abandon:\n%s", v)
	}
}

// A failed abandon flashes and keeps the run's changes held.
func TestAbandonFailureKeepsHeld(t *testing.T) {
	runID, dir := interruptedRun(t)
	r := restarted(t, dir, runID)
	r.drain(r.update(tui.AbandonMsg{RunID: "no-such-run"}))
	if v := r.view(); !strings.Contains(v, "✗ run no-such-run") {
		t.Errorf("no failure flash:\n%s", v)
	}
	if r.m.Ctx().CS.Changes[0].RunID != runID {
		t.Errorf("held change touched: %+v", r.m.Ctx().CS.Changes)
	}
}

// The startup unlock reads the runs off the UI goroutine. Until its answer
// lands, a held change stays held; it unlocks only the runs held at startup,
// never one started since, even if that one is gone by then.
func TestStartupUnlockOnlyCapturedRuns(t *testing.T) {
	cs := &changeset.ChangeSet{Changes: []changeset.Change{setChange("A_NEW", "a"), setChange("C_NEW", "c")}}
	cs.Changes[0].ID, cs.Changes[0].RunID = "c1", "gone-run"
	cs.Changes[1].ID = "c2"
	r := newRig(t, func(d *root.Deps) { d.CS = cs })
	if cs.Changes[0].RunID != "gone-run" {
		t.Fatalf("unlocked in New")
	}
	var unlock tea.Msg
	for _, c := range split(r.m.Init()) {
		msg := c()
		if root.IsStartupUnlock(msg) {
			unlock = msg
			continue
		}
		r.drain(r.update(msg))
	}
	if unlock == nil {
		t.Fatal("Init read no runs for the startup unlock")
	}
	if cs.Changes[0].RunID != "gone-run" {
		t.Errorf("unlocked before the runs list landed")
	}

	// c2 is applied meanwhile: frozen, not executed, then its run vanishes.
	_ = split(r.prepare(tui.ApplyMsg{IDs: []string{"c2"}}))
	newRun := cs.Changes[1].RunID
	if newRun == "" {
		t.Fatalf("c2 not prepared:\n%s", r.view())
	}
	if err := os.RemoveAll(filepath.Join(r.dir, "runs", newRun)); err != nil {
		t.Fatal(err)
	}

	r.drain(r.update(unlock))
	if cs.Changes[0].RunID != "" {
		t.Errorf("stale run kept held")
	}
	if cs.Changes[1].RunID != newRun {
		t.Errorf("a run started since was unlocked")
	}
	saved, err := changeset.Load(r.deps.ChangeSetPath)
	if err != nil || len(saved.Changes) != 2 || saved.Changes[0].RunID != "" {
		t.Errorf("saved = %+v, %v", saved, err)
	}
}

// ids returns a newRunID stub handing out ids in order.
func ids(list ...string) func() string {
	i := 0
	return func() string {
		id := list[min(i, len(list)-1)]
		i++
		return id
	}
}

// The live set reaches disk holding the run before the run is written: a
// quit or crash after the freeze leaves a run whose changes are held.
func TestFreezeSavesLockFirst(t *testing.T) {
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	freeze := r.update(r.toCheck(nil)())
	runID := r.cs.Changes[0].RunID
	if saved, _ := changeset.Load(r.deps.ChangeSetPath); len(saved.Changes) != 1 || saved.Changes[0].RunID != "" {
		t.Fatalf("saved before the freeze command = %+v", saved.Changes)
	}
	freeze() // its message is never fed: the app quit
	saved, _ := changeset.Load(r.deps.ChangeSetPath)
	if len(saved.Changes) != 1 || saved.Changes[0].RunID != runID {
		t.Errorf("saved after the freeze = %+v, want held by %s", saved.Changes, runID)
	}
	if s := journalState(t, r.dir, runID); s != apply.RunRunning {
		t.Errorf("state = %q", s)
	}
}

// When that save fails, no run is written and the selection is unlocked.
func TestFreezeNotWrittenWhenSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a mode 0500 dir")
	}
	csDir := filepath.Join(t.TempDir(), "cs")
	if err := os.MkdirAll(csDir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(d *root.Deps) { d.ChangeSetPath = filepath.Join(csDir, "changeset.json") })
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	freeze := r.update(r.toCheck(nil)())
	if err := os.Chmod(csDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(csDir, 0o700) })
	msg := freeze()
	if err := os.Chmod(csDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := runDirs(t, r.dir); len(got) != 0 {
		t.Errorf("run written though the live set was not saved: %v", got)
	}
	if prepareLockHeld(r.dir) {
		t.Errorf("prepare lock left held")
	}
	r.drain(r.update(msg))
	if r.cs.Changes[0].RunID != "" {
		t.Errorf("not unlocked: %+v", r.cs.Changes)
	}
	if v := r.view(); !strings.Contains(v, "✗ change set not saved") {
		t.Errorf("flash:\n%s", v)
	}
}

// The quit timeout keeps waiting while a run is being frozen, up to a
// second, longer cap.
func TestQuitTimeoutWaitsForFreeze(t *testing.T) {
	defer root.SetQuitWait(time.Millisecond)()
	defer root.SetFreezeQuitWait(time.Millisecond)()
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	_ = r.update(r.toCheck(nil)()) // freezing; its command never ends here
	tick := r.update(key("q"))
	if tick == nil {
		t.Fatal("quit returned nothing")
	}
	timeout := tick()
	if _, ok := timeout.(tea.QuitMsg); ok {
		t.Fatal("quit at once while freezing")
	}
	wait := r.update(timeout)
	if wait == nil {
		t.Fatal("the quit timeout stopped waiting for the freeze")
	}
	capped := wait() // a tea.Tick command fires once
	if _, ok := capped.(tea.QuitMsg); ok {
		t.Fatal("the quit timeout quit while freezing")
	}
	q := r.update(capped)
	if q == nil {
		t.Fatal("no quit after the freeze cap")
	}
	if msg := q(); msg != (tea.QuitMsg{}) {
		t.Errorf("after the freeze cap: %T, want quit", msg)
	}
}

// A drawn run id that a live change, or a run of this process, already
// carries is drawn again; the change holding it stays held.
func TestRunIDTakenIsDrawnAgain(t *testing.T) {
	defer root.SetNewRunID(ids("20261024-120000-000000000001", "20261024-120000-000000000002", "20261024-120000-000000000003"))()
	r := newRig(t)
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("C_NEW", "c")}))
	r.cs.Changes[0].RunID = "20261024-120000-000000000001" // held by an earlier run
	r.m.Ctx().Running["20261024-120000-000000000002"] = tui.RunProgress{State: apply.RunFailed}
	c := r.cs.Changes[1].ID
	_ = split(r.prepare(tui.ApplyMsg{IDs: []string{c}}))
	if got := r.cs.Changes[1].RunID; got != "20261024-120000-000000000003" {
		t.Errorf("run id = %q, want the third drawn\n%s", got, r.view())
	}
	if got := r.cs.Changes[0].RunID; got != "20261024-120000-000000000001" {
		t.Errorf("the change held by the taken id = %q", got)
	}
	if got := runDirs(t, r.dir); !slices.Equal(got, []string{"20261024-120000-000000000003"}) {
		t.Errorf("runs = %v", got)
	}
}

// A newer save that failed is not proof the lock is on disk: the freeze's
// own (older, skipped) save reports that failure and no run is written.
func TestFreezeNotWrittenWhenNewerSaveFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a mode 0500 dir")
	}
	csDir := filepath.Join(t.TempDir(), "cs")
	if err := os.MkdirAll(csDir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(d *root.Deps) { d.ChangeSetPath = filepath.Join(csDir, "changeset.json") })
	r.drain(r.m.Init())
	r.drain(r.update(tui.PutChangeMsg{Change: setChange("A_NEW", "a")}))
	freeze := r.update(r.toCheck(nil)())
	newer := r.update(tui.PutChangeMsg{Change: setChange("C_NEW", "c")})
	if newer == nil {
		t.Fatal("put returned no save")
	}
	if err := os.Chmod(csDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(csDir, 0o700) })
	if _, ok := newer().(tui.FlashMsg); !ok {
		t.Fatal("the newer save did not fail")
	}
	if err := os.Chmod(csDir, 0o700); err != nil {
		t.Fatal(err)
	}
	msg := freeze()
	if got := runDirs(t, r.dir); len(got) != 0 {
		t.Errorf("run written though the lock never reached disk: %v", got)
	}
	r.drain(r.update(msg))
	if r.cs.Changes[0].RunID != "" {
		t.Errorf("not unlocked: %+v", r.cs.Changes)
	}
}

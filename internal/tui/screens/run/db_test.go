package run_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/run"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

const newPassword = "new-db-password-0123456789"

var (
	owner = domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "smongo"}
	app   = domain.Place{ProjectID: "p1", EnvID: "e2", ServiceID: "sapi"}
)

// dbCtx holds run r1, stalled after its database step changed the
// password (tuitest.StalledRun); edit changes its journal first.
func dbCtx(t *testing.T, w int, edit func(j *apply.Journal)) *tui.Ctx {
	t.Helper()
	dir := t.TempDir()
	tuitest.StalledRun(t, dir, "r1", now, owner, app, newPassword)
	if edit != nil {
		j, err := apply.OpenJournal(filepath.Join(dir, "runs", "r1", "journal.json"))
		if err != nil {
			t.Fatal(err)
		}
		edit(j)
	}
	runs := &apply.Runs{Dir: dir, Reg: kinds.NewRegistry()}
	infos, errs := runs.List()
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	// the names the view shows for the fixture's ids
	st := store.New()
	st.ReplaceCatalog(railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "shop", Workspace: "ws",
		Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true}, {ID: "e2", Name: "staging", CanAccess: true}},
		Services:     []railway.Service{{ID: "smongo", Name: "mongo"}, {ID: "sapi", Name: "api"}},
	}}})
	return &tui.Ctx{
		Store: st,
		Runs:  runs, RunIndex: tui.RunIndex{Loaded: true, Runs: infos},
		Theme: newCtx(t, w, 24, apply.RunFailed).Theme,
		Width: w, Height: 24, IsTTY: true,
		Now: newCtx(t, w, 24, apply.RunFailed).Now,
	}
}

func typeWord(w string) []tea.KeyMsg {
	var out []tea.KeyMsg
	for _, c := range w {
		out = append(out, r(string(c)))
	}
	return append(out, tea.KeyMsg{Type: tea.KeyEnter})
}

// Abandoning a run whose database step acted asks for the typed word and
// names the risk.
func TestRunViewAbandonActedAsksWord(t *testing.T) {
	c := dbCtx(t, 90, nil)
	s, ms := press(c, run.New("r1"), r("X"))
	if len(ms) != 0 || !s.(tui.Capturer).Capturing() {
		t.Fatalf("msgs = %#v", ms)
	}
	v := body(c, s)
	if !strings.Contains(v, "the database password may already be new; abandoning leaves places on the old one") ||
		!strings.Contains(v, `abandon run r1? type "abandon" and press enter`) {
		t.Errorf("prompt:\n%s", v)
	}
	for _, w := range []int{80, 60} {
		cw := dbCtx(t, w, nil)
		sw, _ := press(cw, run.New("r1"), r("X"))
		out := frame(cw, sw)
		tuitest.NoSecrets(t, out, newPassword)
		tuitest.Golden(t, "run_abandon_db_"+strconv.Itoa(w)+".golden", out)
	}
	// y alone does not abandon
	if _, ms := press(c, s, r("y"), tea.KeyMsg{Type: tea.KeyEnter}); len(ms) != 0 {
		t.Errorf("y abandoned: %#v", ms)
	}
	s, _ = press(c, run.New("r1"), r("X"))
	_, ms = press(c, s, typeWord("abandon")...)
	if len(ms) != 1 || !reflect.DeepEqual(ms[0], tui.AbandonMsg{RunID: "r1"}) {
		t.Fatalf("typed word: msgs = %#v", ms)
	}

	// a hand run: its pre-flight acted, no dbpw step
	c = dbCtx(t, 120, func(j *apply.Journal) {
		delete(j.Steps, "dbpw:e1/smongo:root")
		if err := j.SetState(apply.RunFailed); err != nil {
			t.Fatal(err)
		}
	})
	if s, _ := press(c, run.New("r1"), r("X")); !strings.Contains(body(c, s), `type "abandon"`) {
		t.Errorf("hand run: no typed word:\n%s", body(c, s))
	}

	// before the database step acted: y/n as any run
	c = dbCtx(t, 120, func(j *apply.Journal) {
		for _, k := range []string{"dbpw:e1/smongo:root", "preflight:e1/smongo:root"} {
			delete(j.Steps, k)
		}
		if err := j.Set("dbpw:e1/smongo:root", apply.StepRecord{Status: apply.StatusBlocked}); err != nil {
			t.Fatal(err)
		}
	})
	s, _ = press(c, run.New("r1"), r("X"))
	if v := body(c, s); strings.Contains(v, `"abandon"`) || !strings.Contains(v, "(y/n)") {
		t.Errorf("unacted run:\n%s", v)
	}
}

// A run executing in another app instance is not resumed or abandoned from
// here.
func TestRunViewElsewhere(t *testing.T) {
	c := dbCtx(t, 120, func(j *apply.Journal) {
		if err := j.SetState(apply.RunRunning); err != nil {
			t.Fatal(err)
		}
	})
	lock := filepath.Join(c.Runs.Dir, "runs", "r1", "lock")
	if err := os.WriteFile(lock, []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	infos, _ := c.Runs.List()
	c.RunIndex = tui.RunIndex{Loaded: true, Runs: infos}
	for _, k := range []string{"r", "X"} {
		s, ms := press(c, run.New("r1"), r(k))
		f, ok := ms[0].(tui.FlashMsg)
		pid := strconv.Itoa(os.Getppid())
		want := "run r1 is running in another window (pid " + pid + ") · if pid " + pid + " has exited, remove " + filepath.Join(c.Runs.Dir, "runs", "r1", "lock") + ", then restart"
		if len(ms) != 1 || !ok || f.Text != want || s.(tui.Capturer).Capturing() {
			t.Errorf("%s: msgs = %#v", k, ms)
		}
	}
	if v := note(c, run.New("r1")); !strings.HasPrefix(v, "r1 · running in another window") {
		t.Errorf("header:\n%s", v)
	}
}

// The run detail says what a database step did without a value: a changed
// password not verified, the KEYFILE verify's repaired outcome, a tunnel
// warning.
func TestRunViewDatabaseDetail(t *testing.T) {
	warn, _ := json.Marshal(map[string]any{"tunnels": []map[string]any{{"socket": "/tmp/x", "pid": 4242}}, "warning": "1 ssh tunnel(s) could not be stopped"})
	for _, w := range []int{80, 60} {
		c := dbCtx(t, w, func(j *apply.Journal) {
			rec := j.Step("dbverify:e1/smongo:root")
			rec.Data = warn
			if err := j.Set("dbverify:e1/smongo:root", rec); err != nil {
				t.Fatal(err)
			}
		})
		out := frame(c, run.New("r1"))
		tuitest.NoSecrets(t, out, newPassword)
		tuitest.Golden(t, "run_db_"+strconv.Itoa(w)+".golden", out)
	}
	c := dbCtx(t, 160, nil)
	v := body(c, run.New("r1"))
	if !hasRow(v, "✓ change the password in the database", "password changed · not verified") {
		t.Errorf("dbpw detail:\n%s", v)
	}
	c = dbCtx(t, 160, func(j *apply.Journal) {
		rec := j.Step("dbverify:e1/smongo:root")
		rec.Data = warn
		_ = j.Set("dbverify:e1/smongo:root", rec)
		_ = j.SetOutcome("r1-pw", apply.Outcome{Status: "done", Reason: "repaired: 1 member\nsecond"})
	})
	v = body(c, run.New("r1"))
	for _, want := range [][]string{{"log in with the new password", "repaired: 1 member · 1 ssh tunnel(s) could not be stopped"}} {
		if !hasRow(v, want...) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "4242") || strings.Contains(v, "second") {
		t.Errorf("tunnel record or a second line shown:\n%s", v)
	}
	// verified: no longer "not verified"
	c = dbCtx(t, 160, func(j *apply.Journal) {
		_ = j.Set("dbverify:e1/smongo:root", apply.StepRecord{Status: apply.StatusDone})
	})
	if v := body(c, run.New("r1")); strings.Contains(v, "not verified") || !hasRow(v, "change the password in the database", "password changed") {
		t.Errorf("verified:\n%s", v)
	}
}

// A run the index does not show (yet) may have acted: abandoning it asks
// for the typed word.
func TestRunViewAbandonUnknownFailsClosed(t *testing.T) {
	c := dbCtx(t, 90, nil)
	c.Running = map[string]tui.RunProgress{"ghost": {State: apply.RunFailed}}
	s, _ := press(c, run.New("ghost"), r("X"))
	if v := body(c, s); !strings.Contains(v, `type "abandon"`) {
		t.Errorf("unknown run: no typed word:\n%s", v)
	}
}

// Steps are grouped by database and environment and said in words; a
// write lists its changes without values, a skipped one says why.
func TestRunViewGroupsAndChanges(t *testing.T) {
	c := dbCtx(t, 120, func(j *apply.Journal) {
		_ = j.SetOutcome("r1-url", apply.Outcome{Status: apply.StatusSkipped, Reason: "changed on Railway"})
	})
	v := body(c, run.New("r1"))
	for _, want := range [][]string{
		{"▾ database · production · mongo · root password"},
		{"▾ production"}, {"✓ redeploy mongo"},
		{"▾ staging"}, {"✓ write 1 variable"},
		{"api", "DB_URL", "→ 70 chars", "» skipped: changed on Railway"},
		{"✗ redeploy api"},
	} {
		if !hasRow(v, want...) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	if strings.Index(v, "▾ database") > strings.Index(v, "▾ production") || strings.Index(v, "▾ production") > strings.Index(v, "▾ staging") {
		t.Errorf("groups out of order:\n%s", v)
	}
	tuitest.NoSecrets(t, v, newPassword)
	// the cursor's step in full at the bottom
	s, _ := press(c, run.New("r1"), tea.KeyMsg{Type: tea.KeyDown})
	if v := body(c, s); !strings.Contains(v, "✓ change the password in the database · production · mongo") {
		t.Errorf("detail of the selected step:\n%s", v)
	}
}

// A long service name shows whole when there is room; a change with no
// overlay (a run scrubbed before overlays were kept) says what its kind
// does.
func TestRunViewChangeColumns(t *testing.T) {
	c := dbCtx(t, 120, nil)
	c.Store.ReplaceCatalog(railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "shop", Workspace: "ws",
		Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true}, {ID: "e2", Name: "staging", CanAccess: true}},
		Services:     []railway.Service{{ID: "smongo", Name: "mongo"}, {ID: "sapi", Name: "Billing Worker"}},
	}}})
	c.RunIndex.Runs[0].Frozen[1].Overlay = apply.Overlay{}
	v := body(c, run.New("r1"))
	if !hasRow(v, "Billing Worker", "DB_URL", "set") {
		t.Errorf("change line:\n%s", v)
	}
}

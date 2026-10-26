package root_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
)

const (
	dbOld   = "mongo-old-root-password-0123"
	dbNew   = "mongo-new-root-password-4567"
	dbGroup = "rotate-db:e1/mongo:mongo"
)

var dbCatalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"mongo", "s1", "s2"}}},
	Services:     []railway.Service{{ID: "mongo", Name: "mongo"}, {ID: "s1", Name: "api"}, {ID: "s2", Name: "web"}},
}}}

// stalledRevertRig holds a run of mongo's root password stalled after its
// database step and before any place got the new password: its changes
// are locked in the live set, next to an unrelated pending change (on the
// web service) and a later decision of the same group outside the run.
func stalledRevertRig(t *testing.T) (r *rig, runID string, row rotation.PasswordRow, other string) {
	t.Helper()
	r = newRig(t)
	r.st.ReplaceCatalog(dbCatalog)
	r.st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "mongo", Name: "MONGO_INITDB_ROOT_PASSWORD", Unrendered: dbOld, Rendered: dbOld},
		{EnvID: "e1", ServiceID: "mongo", Name: "MONGO_INITDB_ROOT_USERNAME", Unrendered: "mongo", Rendered: "mongo"},
		{EnvID: "e1", ServiceID: "s1", Name: "MONGO_URL", Unrendered: "mongodb://mongo:" + dbOld + "@mongo.railway.internal:27017/db", Rendered: "x"},
		{EnvID: "e1", ServiceID: "s2", Name: "PLAIN", Unrendered: "one", Rendered: "one"},
	}, railway.SystemRecord{EnvID: "e1", ServiceID: "mongo", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "mongo.railway.internal"})
	o := rotation.Options{EnvIDs: []string{"e1"}, AllEnvIDs: []string{"e1"}}
	var ok bool
	if row, ok = rotation.RowOf(r.st, dbGroup, o); !ok {
		t.Fatal("no mongo root row")
	}
	now := r.deps.Now()
	pw, err := kinds.NewPasswordChanges(r.st, nil, row, kinds.SourceGenerate, dbNew, now)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range pw {
		if err := r.cs.Put(c, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	plain, err := kinds.SetVariable(r.st, nil, domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s2"}, "PLAIN", "two", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cs.Put(plain, nil); err != nil {
		t.Fatal(err)
	}
	// a later decision of the group, outside the run
	keep := changeset.Change{ID: changeset.NewID(), Kind: "variable.keep", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s2"}, Target: "LATER", Group: dbGroup, Created: now}
	if err := r.cs.Put(keep, nil); err != nil {
		t.Fatal(err)
	}

	// freeze the password's changes into a run, as Prepare does, and
	// journal it stalled after its database step
	runID = "20261024-120000-aaaaaaaaaaaa"
	dir := filepath.Join(r.dir, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := changeset.Save(filepath.Join(dir, "changeset.json"), changeset.ChangeSet{Changes: pw}); err != nil {
		t.Fatal(err)
	}
	j, err := apply.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	for k, rec := range map[string]apply.StepRecord{
		"preflight:e1/mongo:mongo": {Status: apply.StatusDone, Acted: true},
		"dbpw:e1/mongo:mongo":      {Status: apply.StatusDone, Acted: true},
		"patch:e1":                 {Status: apply.StatusFailed, Error: "boom"},
	} {
		if err := j.Set(k, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.SetState(apply.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := r.cs.Lock(ids, runID); err != nil {
		t.Fatal(err)
	}
	return r, runID, row, plain.ID
}

// Revert abandons the stalled run without the typed word, drops every
// change of the old rotation from the live set, saves, and starts a run of
// a revert that logs in with the password the run set at once: the row's
// rewritten URL needs no decision, and Vet is asked about the revert before
// the abandon and again by the run's prepare.
func TestRevertStalledPassword(t *testing.T) {
	r, runID, row, other := stalledRevertRig(t)
	r.snap.Store(r.st.Clone())
	var vetted []string
	r.runs.Vet = func(s *store.Store, sel, live changeset.ChangeSet, actions []apply.FrozenAction) error {
		for _, c := range sel.Changes {
			vetted = append(vetted, c.Kind)
		}
		return kinds.CheckPasswordGroups(s, []string{"e1"}, sel, live, actions)
	}
	seen := r.drain(r.update(tui.RevertMsg{RunID: runID, Row: row}))
	if s := journalState(t, r.dir, runID); s != apply.RunAbandoned {
		t.Errorf("run state = %q, want abandoned", s)
	}
	if !slices.Equal(vetted, []string{"db.password", "db.password"}) {
		t.Errorf("Vet saw %q, want the revert before the abandon and at its prepare", vetted)
	}
	var group []changeset.Change
	for _, c := range r.cs.Changes {
		if c.Group == dbGroup {
			group = append(group, c)
		} else if c.RunID != "" {
			t.Errorf("%s still locked by %s", c.Target, c.RunID)
		}
	}
	if _, ok := r.cs.Get(other); !ok {
		t.Errorf("unrelated change dropped")
	}
	if len(group) != 1 || group[0].Kind != "db.password" {
		var got []string
		for _, c := range group {
			got = append(got, c.Kind+" "+c.Target+" run "+c.RunID)
		}
		t.Fatalf("group changes = %q, want the revert only", got)
	}
	rv := group[0]
	if rv.RunID == "" || rv.RunID == runID {
		t.Fatalf("revert run = %q, want a new run started at once", rv.RunID)
	}
	frozen, err := r.runs.Frozen(rv.RunID)
	if err != nil || len(frozen.Changes) != 1 || frozen.Changes[0].ID != rv.ID {
		t.Fatalf("revert run %s froze %+v, %v; want the revert alone", rv.RunID, frozen.Changes, err)
	}
	if s := journalState(t, r.dir, rv.RunID); s == "" {
		t.Errorf("the revert run was not executed")
	}
	src, value, _ := kinds.PasswordValue(rv)
	var nv struct {
		Login string `json:"login"`
	}
	_ = json.Unmarshal(rv.New, &nv)
	if src != kinds.SourceRevert || value != dbOld || nv.Login != dbNew {
		t.Errorf("revert: source %q, value is current %v, login is the run's %v", src, value == dbOld, nv.Login == dbNew)
	}
	// saved: the file holds the same set
	saved, err := changeset.Load(r.deps.ChangeSetPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Changes) != len(r.cs.Changes) {
		t.Errorf("saved %d changes, live %d", len(saved.Changes), len(r.cs.Changes))
	}
	if v := r.view(); strings.Contains(v, dbNew) || strings.Contains(v, dbOld) {
		t.Errorf("view holds a password:\n%s", v)
	}
	for _, m := range seen {
		if f, ok := m.(tui.FlashMsg); ok && (strings.Contains(f.Text, dbNew) || strings.Contains(f.Text, dbOld)) {
			t.Errorf("flash holds a password: %q", f.Text)
		}
	}
}

// A revert Vet refuses is refused before the run is abandoned.
func TestRevertVettedBeforeAbandon(t *testing.T) {
	r, runID, row, _ := stalledRevertRig(t)
	r.snap.Store(r.st.Clone())
	r.runs.Vet = func(*store.Store, changeset.ChangeSet, changeset.ChangeSet, []apply.FrozenAction) error {
		return errors.New("production · mongo is read only · Settings w or k keeps it")
	}
	r.drain(r.update(tui.RevertMsg{RunID: runID, Row: row}))
	if s := journalState(t, r.dir, runID); s != apply.RunFailed {
		t.Errorf("run state = %q, want failed (untouched)", s)
	}
	if v := r.view(); !strings.Contains(v, "cannot revert") || !strings.Contains(v, "read only") {
		t.Errorf("flash:\n%s", v)
	}
}

// A revert that cannot be built is refused before the run is abandoned:
// the run and its locked changes stay as they are.
func TestRevertCheckedBeforeAbandon(t *testing.T) {
	r, runID, row, _ := stalledRevertRig(t)
	r.st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "mongo", Name: "MONGO_INITDB_ROOT_PASSWORD", Sealed: true},
		{EnvID: "e1", ServiceID: "mongo", Name: "MONGO_INITDB_ROOT_USERNAME", Unrendered: "mongo", Rendered: "mongo"},
	}, railway.SystemRecord{EnvID: "e1", ServiceID: "mongo", Name: "RAILWAY_PRIVATE_DOMAIN", Value: "mongo.railway.internal"})
	r.drain(r.update(tui.RevertMsg{RunID: runID, Row: row}))
	if s := journalState(t, r.dir, runID); s != apply.RunFailed {
		t.Errorf("run state = %q, want failed (untouched)", s)
	}
	if v := r.view(); !strings.Contains(v, "cannot revert") {
		t.Errorf("flash:\n%s", v)
	}
	for _, c := range r.cs.Changes {
		if c.Group == dbGroup && c.Kind == "db.password" && c.RunID != runID {
			t.Errorf("the run's password change was unlocked or replaced")
		}
	}
}

// A revert is refused while the run applies here, and leaves the run
// alone when its frozen set has no password change of the row.
func TestRevertRefused(t *testing.T) {
	r, runID, row, _ := stalledRevertRig(t)
	other := row
	other.Key = "rotate-db:e1/mongo:someone"
	r.drain(r.update(tui.RevertMsg{RunID: runID, Row: other}))
	if s := journalState(t, r.dir, runID); s != apply.RunFailed {
		t.Errorf("run state = %q, want failed (untouched)", s)
	}
	if v := r.view(); !strings.Contains(v, "cannot revert") {
		t.Errorf("flash:\n%s", v)
	}
	locked := 0
	for _, c := range r.cs.Changes {
		if c.RunID == runID {
			locked++
		}
	}
	if locked == 0 {
		t.Errorf("changes unlocked by a refused revert")
	}
}

// A run another live instance executes is not abandoned from here: the
// flash says where it runs.
func TestAbandonElsewhereWording(t *testing.T) {
	r, runID, _, _ := stalledRevertRig(t)
	if err := os.WriteFile(filepath.Join(r.dir, "runs", runID, "lock"), []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	r.drain(r.update(tui.AbandonMsg{RunID: runID}))
	if v := r.view(); !strings.Contains(v, "run "+runID+" is running in another window") {
		t.Errorf("flash:\n%s", v)
	}
	if s := journalState(t, r.dir, runID); s != apply.RunFailed {
		t.Errorf("state = %q", s)
	}
}

// R is refused while another run is being prepared: the revert would start
// at once after its abandon.
func TestRevertRefusedWhilePreparing(t *testing.T) {
	r, runID, row, other := stalledRevertRig(t)
	if cmd := r.update(tui.ApplyMsg{IDs: []string{other}}); cmd == nil {
		t.Fatal("the apply did not start preparing")
	}
	if cmd := r.update(tui.RevertMsg{RunID: runID, Row: row}); cmd != nil {
		r.drain(cmd)
	}
	if v := r.view(); !strings.Contains(v, "a run is being prepared · R again after it") {
		t.Errorf("flash:\n%s", v)
	}
	if s := journalState(t, r.dir, runID); s != apply.RunFailed {
		t.Errorf("run state = %q, want failed (untouched)", s)
	}
}

// A revert whose run does not start (its prepare refused) stays in the
// change set, and the flash says so.
func TestRevertPendingWhenItCannotStart(t *testing.T) {
	r, runID, row, _ := stalledRevertRig(t)
	r.snap.Store(r.st.Clone())
	calls := 0
	r.runs.Vet = func(*store.Store, changeset.ChangeSet, changeset.ChangeSet, []apply.FrozenAction) error {
		calls++
		if calls > 1 {
			return errors.New("refused at prepare")
		}
		return nil
	}
	r.drain(r.update(tui.RevertMsg{RunID: runID, Row: row}))
	if v := r.view(); !strings.Contains(v, "revert of mongo root is pending · A applies it") {
		t.Errorf("flash:\n%s", v)
	}
	var reverts int
	for _, c := range r.cs.Changes {
		if c.Group == dbGroup && c.Kind == "db.password" && c.RunID == "" {
			reverts++
		}
	}
	if reverts != 1 {
		t.Errorf("pending reverts = %d, want 1", reverts)
	}
}

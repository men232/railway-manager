package view_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/view"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func patchEntry(id, env, patchID string, when time.Time) railway.HistoryEntry {
	return railway.HistoryEntry{
		ID: id, Object: "EnvironmentPatch", Action: "applied", CreatedAt: when,
		Actor: "u1", WorkflowID: "commitChanges/" + env + "/" + patchID,
	}
}

func feedRow(t *testing.T, rows []view.FeedRow, at time.Time) view.FeedRow {
	t.Helper()
	for _, r := range rows {
		if r.At.Equal(at) {
			return r
		}
	}
	t.Fatalf("no row at %v in %d rows", at, len(rows))
	return view.FeedRow{}
}

func TestFeedMergedNewestFirst(t *testing.T) {
	f := newTreeFx(t)
	f.st.SetMembers("p1", []railway.Member{{ID: "u1", Name: "Ann"}})
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		{ID: "a3", Object: "Variable", Action: "updated", CreatedAt: at(3), ServiceIDs: []string{"web", "api"}, Names: []string{"KEY"}, Actor: "u1"},
		{ID: "a1", Object: "Deployment", Action: "created", CreatedAt: at(1)},
	}, EndCursor: "c1", HasNext: true})
	f.st.AppendHistory("e2", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		{ID: "b4", Object: "Variable", Action: "deleted", CreatedAt: at(4), Actor: "stranger-principal"},
		{ID: "b2", Object: "SSHSession", Action: "authenticated", CreatedAt: at(2), Actor: "u1"},
	}})
	f.st.AppendHistory("e3", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		{ID: "c9", Object: "Variable", Action: "created", CreatedAt: at(9)},
	}})

	rows := f.v.Feed(bothEnvs, nil)
	var got []time.Time
	for _, r := range rows {
		got = append(got, r.At)
	}
	if want := []time.Time{at(4), at(3), at(2), at(1)}; !slices.Equal(got, want) {
		t.Fatalf("times %v, want %v (e3 not asked)", got, want)
	}
	r := rows[1]
	if r.Who != "Ann" || r.Action != "updated" || r.Object != "Variable" || r.Env != "Proj/dev" ||
		r.EnvName != "dev" || r.Service != "web, api" || !slices.Equal(r.Names, []string{"KEY"}) {
		t.Errorf("row %+v", r)
	}
	if r := rows[0]; r.Who != "stranger" || r.Env != "Proj/alpha" || r.Service != "" {
		t.Errorf("non-member row %+v", r)
	}
	if r := rows[3]; r.Who != "?" {
		t.Errorf("no actor: who %q, want ?", r.Who)
	}
	if r := rows[0]; r.RunID != "" || r.Local {
		t.Errorf("a variable entry is not a run: %+v", r)
	}

	// memoised; a new page rebuilds
	before := f.v.FeedBuilds()
	f.v.Feed(bothEnvs, nil)
	if f.v.FeedBuilds() != before {
		t.Errorf("rebuilt with nothing changed")
	}
	_, cur, _, _ := f.st.History("e1")
	f.st.AppendHistory("e1", cur, railway.HistoryPage{Entries: []railway.HistoryEntry{{ID: "a0", Object: "Variable", CreatedAt: at(0)}}})
	if got := f.v.Feed(bothEnvs, nil); len(got) != 5 {
		t.Errorf("after a page: %d rows, want 5", len(got))
	}
}

func TestFeedRecognisesLocalRun(t *testing.T) {
	f := newTreeFx(t)
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		patchEntry("h1", "e1", "patch-1", at(1)),
	}})
	runs := []apply.RunInfo{{ID: "run-a", PatchIDs: map[string]string{"patch-1": "e1"}}}
	r := feedRow(t, f.v.Feed(bothEnvs, runs), at(1))
	if r.RunID != "run-a" || !r.Local {
		t.Errorf("run %q local %v, want run-a local", r.RunID, r.Local)
	}

	// The memo key follows the runs: without them the entry is unknown.
	r = feedRow(t, f.v.Feed(bothEnvs, nil), at(1))
	if r.RunID != "" || r.Local {
		t.Errorf("no runs, no patches: run %q local %v", r.RunID, r.Local)
	}
}

func TestFeedRecognisesByMessage(t *testing.T) {
	f := newTreeFx(t)
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		patchEntry("h1", "e1", "patch-gone", at(1)),  // run removed locally
		patchEntry("h2", "e1", "patch-retry", at(2)), // run kept, but this was an earlier attempt
	}})
	f.st.SetPatches("e1", []railway.EnvPatch{
		{ID: "patch-gone", Message: "railway-manager run-old: 3 changes"},
		{ID: "patch-retry", Message: "railway-manager run-b: 1 changes"},
	})
	// run-b's journal holds only its last attempt's patch id.
	runs := []apply.RunInfo{{ID: "run-b", PatchIDs: map[string]string{"patch-last": "e1"}}, {ID: "run-c"}}
	f.v.Me = "u1" // the operator, who made both patches
	rows := f.v.Feed(bothEnvs, runs)
	if r := feedRow(t, rows, at(1)); r.RunID != "run-old" || r.Local {
		t.Errorf("removed run: run %q local %v, want run-old not local", r.RunID, r.Local)
	}
	if r := feedRow(t, rows, at(2)); r.RunID != "run-b" || !r.Local {
		t.Errorf("earlier attempt: run %q local %v, want run-b local", r.RunID, r.Local)
	}

	// Patches read later rebuild the feed.
	f.st.SetPatches("e1", nil)
	if r := feedRow(t, f.v.Feed(bothEnvs, runs), at(1)); r.RunID != "" {
		t.Errorf("patches gone: run %q", r.RunID)
	}
}

func TestFeedForeignPatchNotOurs(t *testing.T) {
	f := newTreeFx(t)
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		patchEntry("h1", "e1", "patch-ui", at(1)),
		patchEntry("h2", "e1", "patch-odd", at(2)),
		patchEntry("h3", "e1", "patch-unread", at(3)),
		{ID: "h4", Object: "EnvironmentPatch", Action: "applied", CreatedAt: at(4), WorkflowID: "somethingElse/e1/patch-ui"},
	}})
	f.st.SetPatches("e1", []railway.EnvPatch{
		{ID: "patch-ui", Message: "tweak from the dashboard"},
		{ID: "patch-odd", Message: "railway-manager : no run id"},
	})
	runs := []apply.RunInfo{{ID: "run-a", PatchIDs: map[string]string{"patch-mine": "e1"}}}
	for _, r := range f.v.Feed(bothEnvs, runs) {
		if r.RunID != "" || r.Local {
			t.Errorf("entry at %v marked ours: run %q local %v", r.At, r.RunID, r.Local)
		}
	}
}

// Names carry only the payload's variable names, raw (screens sanitize at
// render), in a copy the store doesn't share.
func TestFeedNamesRawAndCopied(t *testing.T) {
	f := newTreeFx(t)
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		{ID: "h1", Object: "Variable", Action: "updated", CreatedAt: at(1), Names: []string{"A\x1b[31m", "B"}},
	}})
	rows := f.v.Feed(bothEnvs, nil)
	if len(rows) != 1 || !slices.Equal(rows[0].Names, []string{"A\x1b[31m", "B"}) {
		t.Fatalf("rows %+v", rows)
	}
	rows[0].Names[0] = "changed"
	entries, _, _, _ := f.st.History("e1")
	if entries[0].Names[0] != "A\x1b[31m" {
		t.Errorf("row names alias the store")
	}
}

// A patch's message is free text any project member can write: one that
// names a local run marks the row with it, but only a patch the operator
// made is the local run itself (openable).
func TestFeedMessageOfAnotherMemberNotLocal(t *testing.T) {
	f := newTreeFx(t)
	e := patchEntry("h1", "e1", "patch-x", at(1))
	e.Actor = "u2"
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{e}})
	f.st.SetPatches("e1", []railway.EnvPatch{{ID: "patch-x", Message: "railway-manager run-b: 1 changes"}})
	runs := []apply.RunInfo{{ID: "run-b"}}
	f.v.Me = "u1"
	if r := feedRow(t, f.v.Feed(bothEnvs, runs), at(1)); r.RunID != "run-b" || r.Local {
		t.Errorf("another member's message: run %q local %v, want run-b not local", r.RunID, r.Local)
	}
	f.v.Me = "" // not signed in yet: no message is ours
	if r := feedRow(t, f.v.Feed(bothEnvs, runs), at(1)); r.Local {
		t.Error("with no account, a message made the row local")
	}
}

func TestFeedPatchIDInAnotherEnvNotLocal(t *testing.T) {
	f := newTreeFx(t)
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		patchEntry("h1", "e1", "patch-1", at(1)),
	}})
	runs := []apply.RunInfo{{ID: "run-a", PatchIDs: map[string]string{"patch-1": "e2"}}}
	if r := feedRow(t, f.v.Feed(bothEnvs, runs), at(1)); r.RunID != "" || r.Local {
		t.Errorf("patch id of another env: run %q local %v", r.RunID, r.Local)
	}
}

func TestFeedRunOnlyOnPatchEntries(t *testing.T) {
	f := newTreeFx(t)
	e := patchEntry("h1", "e1", "patch-1", at(1))
	e.Object = "Variable"
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{e}})
	f.st.SetPatches("e1", []railway.EnvPatch{{ID: "patch-1", Message: "railway-manager run-a: 1 changes"}})
	runs := []apply.RunInfo{{ID: "run-a", PatchIDs: map[string]string{"patch-1": "e1"}}}
	if r := feedRow(t, f.v.Feed(bothEnvs, runs), at(1)); r.RunID != "" || r.Local {
		t.Errorf("a Variable entry got run %q local %v", r.RunID, r.Local)
	}
}

func TestFeedTiesKeepEnvOrder(t *testing.T) {
	f := newTreeFx(t)
	for _, env := range []string{"e1", "e2"} {
		f.st.AppendHistory(env, "", railway.HistoryPage{Entries: []railway.HistoryEntry{
			{ID: env + "-x", Object: "Variable", CreatedAt: at(1)},
			{ID: env + "-y", Object: "Deployment", CreatedAt: at(1)},
		}})
	}
	var got []string
	for _, r := range f.v.Feed([]string{"e2", "e1"}, nil) {
		got = append(got, r.Env+" "+r.Object)
	}
	want := []string{"Proj/alpha Variable", "Proj/alpha Deployment", "Proj/dev Variable", "Proj/dev Deployment"}
	if !slices.Equal(got, want) {
		t.Errorf("tie order %v, want %v", got, want)
	}
}

// The workflow's env, not the history's, names the patch.
func TestFeedWorkflowEnvDiffersFromHistoryEnv(t *testing.T) {
	f := newTreeFx(t)
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		patchEntry("h1", "e2", "patch-1", at(1)),
		patchEntry("h2", "e2", "patch-2", at(2)),
	}})
	f.st.SetPatches("e1", []railway.EnvPatch{{ID: "patch-1", Message: "railway-manager run-e1: 1 changes"}})
	f.st.SetPatches("e2", []railway.EnvPatch{{ID: "patch-1", Message: "railway-manager run-e2: 1 changes"}})
	runs := []apply.RunInfo{{ID: "run-b", PatchIDs: map[string]string{"patch-2": "e2"}}}
	rows := f.v.Feed(bothEnvs, runs)
	if r := feedRow(t, rows, at(1)); r.RunID != "run-e2" || r.Local {
		t.Errorf("by message: run %q local %v, want run-e2 not local", r.RunID, r.Local)
	}
	if r := feedRow(t, rows, at(2)); r.RunID != "run-b" || !r.Local {
		t.Errorf("by journal: run %q local %v, want run-b local", r.RunID, r.Local)
	}
}

// A run id that spells another run's id and patch id is another key.
func TestFeedRunsDigestDelimitsIDs(t *testing.T) {
	f := newTreeFx(t)
	f.st.AppendHistory("e1", "", railway.HistoryPage{Entries: []railway.HistoryEntry{
		patchEntry("h1", "e1", "patch-1", at(1)),
	}})
	f.v.Feed(bothEnvs, []apply.RunInfo{{ID: "run-a", PatchIDs: map[string]string{"patch-1": "e1"}}})
	if r := feedRow(t, f.v.Feed(bothEnvs, []apply.RunInfo{{ID: "run-a|patch-1"}}), at(1)); r.Local {
		t.Errorf("stale feed: run %q local", r.RunID)
	}
}

type feedTokens struct{}

func (feedTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (t feedTokens) Refresh(ctx context.Context) railway.TokenStatus { return t.Get(ctx) }
func (t feedTokens) Peek() railway.TokenStatus                       { return t.Get(context.Background()) }

// A non-Variable entry's payload `name`/`names` never reaches a feed row:
// read through the client, as the loaders do.
func TestFeedNoNamesForOtherObjects(t *testing.T) {
	fk := railwaytest.New(t)
	fk.SetHistoryRaw("e1", []map[string]any{
		{"id": "h1", "object": "SSHSession", "action": "created", "createdAt": at(3).Format(time.RFC3339Nano),
			"payload": map[string]any{"name": "SSH_NAME"}},
		{"id": "h2", "object": "Volume", "action": "updated", "createdAt": at(2).Format(time.RFC3339Nano),
			"payload": map[string]any{"names": []any{"VOL_NAME"}}},
		{"id": "h3", "object": "Domain", "action": "created", "createdAt": at(1).Format(time.RFC3339Nano),
			"payload": map[string]any{"name": "DOMAIN_NAME"}},
	})
	api := &railway.Client{T: &railway.Transport{Endpoint: fk.URL, Tokens: feedTokens{}}}
	p, err := api.EnvironmentHistory(context.Background(), "e1", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	f := newTreeFx(t)
	f.st.AppendHistory("e1", "", p)
	rows := f.v.Feed(bothEnvs, nil)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if len(r.Names) != 0 {
			t.Errorf("%s row Names = %q, want none", r.Object, r.Names)
		}
	}
}

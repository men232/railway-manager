package apply_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/view"
)

// The read-only live checks of history, patches and members. They run
// only with LIVE_RAILWAY=1, read liveEnv of liveProject and write nothing:
// their client refuses every write. They log counts and shapes only, never a
// payload, a variable name, a member or a patch message.

// liveReadWindow is how far back the patch and feed checks read.
const liveReadWindow = 30 * 24 * time.Hour

// liveReadClient is a Railway client that may write nowhere.
func liveReadClient(t *testing.T) *railway.Client {
	t.Helper()
	api, err := newReadClient()
	if err != nil {
		t.Fatal(err)
	}
	return api
}

// newReadClient is a Railway client that refuses every write.
func newReadClient() (*railway.Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	tokens := railway.NewTokenSource(os.Getenv, filepath.Join(home, ".railway", "config.json"), proc.RailwayWhoami, time.Now)
	return &railway.Client{
		T:            &railway.Transport{Endpoint: railway.DefaultEndpoint, HTTP: &http.Client{Timeout: 60 * time.Second}, Tokens: tokens},
		WriteAllowed: func(string) bool { return false },
	}, nil
}

// historyEntryFields is every field a history entry may hold: ids, kinds,
// a time, service ids, actor and workflow ids, and the variable names. No
// other payload text survives the decoder.
var historyEntryFields = map[string]reflect.Type{
	"ID":         reflect.TypeFor[string](),
	"Object":     reflect.TypeFor[string](),
	"Action":     reflect.TypeFor[string](),
	"CreatedAt":  reflect.TypeFor[time.Time](),
	"ServiceIDs": reflect.TypeFor[[]string](),
	"Names":      reflect.TypeFor[[]string](),
	"Actor":      reflect.TypeFor[string](),
	"WorkflowID": reflect.TypeFor[string](),
}

func TestHistoryEntryHoldsNoPayload(t *testing.T) {
	// Runs always (no Live in its name): the shape guard TestLiveHistoryShape
	// repeats live.
	assertHistoryEntryFields(t)
}

func assertHistoryEntryFields(t *testing.T) {
	t.Helper()
	typ := reflect.TypeFor[railway.HistoryEntry]()
	if typ.NumField() != len(historyEntryFields) {
		t.Errorf("HistoryEntry has %d fields, want %d", typ.NumField(), len(historyEntryFields))
	}
	for f := range typ.Fields() {
		want, ok := historyEntryFields[f.Name]
		if !ok {
			t.Errorf("HistoryEntry field %s is not a known field: no payload text may survive", f.Name)
			continue
		}
		if f.Type != want {
			t.Errorf("HistoryEntry field %s is %s, want %s", f.Name, f.Type, want)
		}
	}
}

// patchIDOf returns the patch id of an entry's workflow id for env.
func patchIDOf(e railway.HistoryEntry, env string) (string, bool) {
	rest, ok := strings.CutPrefix(e.WorkflowID, "commitChanges/"+env+"/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

func TestLiveHistoryShape(t *testing.T) {
	liveSkip(t)
	assertHistoryEntryFields(t)
	api := liveReadClient(t)
	ctx := context.Background()

	first, err := api.EnvironmentHistory(ctx, liveEnv, "", time.Time{})
	if err != nil {
		t.Fatalf("EnvironmentHistory first page: %v", err)
	}
	t.Logf("first page: %d entries, has next %v", len(first.Entries), first.HasNext)
	if len(first.Entries) == 0 {
		t.Fatal("first page has no entries")
	}

	objects := map[string]int{}
	withNames := map[string]int{}
	patches, variables, variablesNamed, variablesWithService := 0, 0, 0, 0
	for _, e := range first.Entries {
		objects[e.Object]++
		if len(e.Names) > 0 {
			withNames[e.Object]++
			if e.Object != "Variable" {
				t.Errorf("%s entry %s has payload names; only Variable names are read", e.Object, e.ID)
			}
		}
		switch e.Object {
		case "EnvironmentPatch":
			if e.WorkflowID == "" {
				continue
			}
			if _, ok := patchIDOf(e, liveEnv); !ok {
				t.Errorf("EnvironmentPatch entry %s: workflowId is not commitChanges/<liveEnv>/<id>", e.ID)
				continue
			}
			patches++
		case "Variable":
			variables++
			if len(e.Names) == 1 && e.Names[0] != "" {
				variablesNamed++
			}
			if len(e.ServiceIDs) > 0 {
				variablesWithService++
			}
		}
	}
	t.Logf("objects: %v", objects)
	t.Logf("entries with names, by object: %v", withNames)
	t.Logf("EnvironmentPatch with workflow: %d; Variable: %d, named: %d, with service ids: %d",
		patches, variables, variablesNamed, variablesWithService)
	if patches == 0 {
		t.Error("no EnvironmentPatch entry with a commitChanges/<liveEnv>/<id> workflowId on the first page")
	}
	if variables > 0 && variablesNamed != variables {
		t.Errorf("%d of %d Variable entries have no single name", variables-variablesNamed, variables)
	}
	if variables > 0 && variablesWithService == 0 {
		t.Error("no Variable entry carries service ids")
	}

	if !first.HasNext {
		t.Log("one page only: second page not read")
		return
	}
	second, err := api.EnvironmentHistory(ctx, liveEnv, first.EndCursor, time.Time{})
	if err != nil {
		t.Fatalf("EnvironmentHistory second page: %v", err)
	}
	t.Logf("second page: %d entries, has next %v", len(second.Entries), second.HasNext)
	if len(second.Entries) == 0 {
		t.Error("second page has no entries")
	}
	seen := map[string]bool{}
	for _, e := range first.Entries {
		seen[e.ID] = true
	}
	overlap := 0
	for _, e := range second.Entries {
		if seen[e.ID] {
			overlap++
		}
	}
	if overlap > 0 {
		t.Errorf("second page repeats %d entries of the first", overlap)
	}
}

func TestLivePatchesMessage(t *testing.T) {
	liveSkip(t)
	api := liveReadClient(t)
	ctx := context.Background()
	since := time.Now().Add(-liveReadWindow)

	patches, err := api.EnvironmentPatches(ctx, liveEnv, since)
	if err != nil {
		t.Fatalf("EnvironmentPatches: %v", err)
	}
	ours := 0
	byID := make(map[string]bool, len(patches))
	for i, p := range patches {
		byID[p.ID] = true
		if strings.HasPrefix(p.Message, "railway-manager ") {
			ours++
		}
		if p.CreatedAt.Before(since) {
			t.Errorf("patch %d is older than the window", i)
		}
		if i > 0 && p.CreatedAt.After(patches[i-1].CreatedAt) {
			t.Errorf("patch %d is newer than patch %d: not newest first", i, i-1)
		}
	}
	t.Logf("patches in %v: %d, with the railway-manager prefix: %d", liveReadWindow, len(patches), ours)
	if ours == 0 {
		t.Error("no patch with the railway-manager prefix in the window")
	}

	first, err := api.EnvironmentHistory(ctx, liveEnv, "", time.Time{})
	if err != nil {
		t.Fatalf("EnvironmentHistory: %v", err)
	}
	seen, found, recent := 0, 0, 0
	for _, e := range first.Entries {
		if e.Object != "EnvironmentPatch" {
			continue
		}
		id, ok := patchIDOf(e, liveEnv)
		if !ok {
			continue
		}
		seen++
		switch {
		case byID[id]:
			found++
		case !e.CreatedAt.Before(since):
			recent++
		}
	}
	t.Logf("history patch ids on the first page: %d, found among patches: %d, not found: %d (in the window: %d)",
		seen, found, seen-found, recent)
	if recent > 0 {
		t.Errorf("%d patch ids of entries in the window are missing from the patches", recent)
	}
}

func TestLiveMembers(t *testing.T) {
	liveSkip(t)
	api := liveReadClient(t)
	ctx := context.Background()

	members, err := api.ProjectMembers(ctx, liveProject)
	if err != nil {
		t.Fatalf("ProjectMembers: %v", err)
	}
	named := 0
	for _, m := range members {
		if m.Name != "" || m.Email != "" {
			named++
		}
	}
	t.Logf("members: %d, with a name or email: %d", len(members), named)
	if len(members) == 0 {
		t.Fatal("no members")
	}

	st := store.New()
	st.SetMembers(liveProject, members)
	page, err := api.EnvironmentHistory(ctx, liveEnv, "", time.Time{})
	if err != nil {
		t.Fatalf("EnvironmentHistory: %v", err)
	}
	actors := map[string]bool{}
	for _, e := range page.Entries {
		if e.Actor != "" {
			actors[e.Actor] = true
		}
	}
	resolved := 0
	for a := range actors {
		prefix := a
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		if st.MemberName(liveProject, a) != prefix {
			resolved++
		}
	}
	t.Logf("distinct actors on the first page: %d, resolved to a member: %d", len(actors), resolved)
	if resolved == 0 {
		t.Error("no history actor resolves to a member")
	}
}

func TestLiveFeedRecognisesOurRuns(t *testing.T) {
	liveSkip(t)
	api := liveReadClient(t)
	ctx := context.Background()
	since := time.Now().Add(-liveReadWindow)

	cat, err := api.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	st := store.New()
	st.ReplaceCatalog(cat)
	if got := st.PlaceOf(liveEnv, "").ProjectID; got != liveProject {
		t.Fatalf("liveEnv is in project %q, want %q", got, liveProject)
	}
	members, err := api.ProjectMembers(ctx, liveProject)
	if err != nil {
		t.Fatalf("ProjectMembers: %v", err)
	}
	st.SetMembers(liveProject, members)
	patches, err := api.EnvironmentPatches(ctx, liveEnv, since)
	if err != nil {
		t.Fatalf("EnvironmentPatches: %v", err)
	}
	st.SetPatches(liveEnv, patches)
	page, err := api.EnvironmentHistory(ctx, liveEnv, "", since)
	if err != nil {
		t.Fatalf("EnvironmentHistory: %v", err)
	}
	st.AppendHistory(liveEnv, "", page)

	v := &view.Views{Store: st}
	rows := v.Feed([]string{liveEnv}, nil)
	ours, local := 0, 0
	for _, r := range rows {
		if r.RunID != "" {
			ours++
		}
		if r.Local {
			local++
		}
	}
	t.Logf("feed rows: %d, ours by message: %d, local: %d", len(rows), ours, local)
	if ours == 0 {
		t.Error("no feed row recognised as a railway-manager run from its patch message")
	}
	if local > 0 {
		t.Errorf("%d rows are local with no local runs", local)
	}
	if !slices.IsSortedFunc(rows, func(a, b view.FeedRow) int { return b.At.Compare(a.At) }) {
		t.Error("feed rows are not newest first")
	}
}

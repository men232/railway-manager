package loaders_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
)

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

func project(id string) railway.Project {
	return railway.Project{
		ID: id, Name: "n" + id, Workspace: "ws",
		Environments: []railway.Environment{
			{ID: id + "-prod", Name: "production", CanAccess: true, ServiceIDs: []string{id + "-s"}},
			{ID: id + "-dev", Name: "dev", CanAccess: true, ServiceIDs: []string{id + "-s"}},
		},
		Services: []railway.Service{{ID: id + "-s", Name: "api"}},
	}
}

func TestLoaders(t *testing.T) {
	f := railwaytest.New(t)
	cat := railway.Catalog{Projects: []railway.Project{project("a"), project("b")}}
	f.SetCatalog(cat)
	f.SetVar("a-prod", "a-s", "X", "1", false)
	f.SetVar("b-dev", "b-s", "Y", "2", false)
	f.SetSystemVar("a-prod", "a-s", "RAILWAY_PRIVATE_DOMAIN", "a.railway.internal")
	c := &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}

	if m := loaders.WhoAmI(c)().(loaders.AccountMsg); m.Err != nil || m.Account.Email == "" {
		t.Fatalf("whoami = %+v", m)
	}
	cm := loaders.Catalog(c, time.Now)().(loaders.CatalogMsg)
	st := store.New()
	cm.ApplyTo(st)
	if cm.Err != nil || len(st.Projects()) != 2 || st.Load("catalog").ReadAt.IsZero() {
		t.Fatalf("catalog = %+v", cm)
	}

	set := &settings.Settings{Projects: []string{"a", "b", "gone"}, Envs: []string{"b-dev"}}
	ts := loaders.Targets(st.Projects(), set)
	if len(ts) != 2 || len(ts[0].Environments) != 2 || len(ts[1].Environments) != 1 {
		t.Fatalf("targets = %+v", ts)
	}
	all := loaders.LoadTargets(st.Projects(), set)
	if len(all) != 2 || len(all[0].Environments) != 2 || len(all[1].Environments) != 2 {
		t.Fatalf("load targets = %+v", all)
	}
	if len(ts[1].Environments) != 1 {
		t.Fatalf("Targets must keep listing the selected environments only")
	}
	batch, ok := loaders.Inventory(c, ts, 7, time.Now)().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("inventory is not a batch of 2")
	}
	for _, cmd := range batch {
		m := cmd().(loaders.InventoryMsg)
		if m.Err != nil || m.Seq != 7 {
			t.Fatalf("inventory %s: %v", m.ProjectID, m.Err)
		}
		m.ApplyTo(st)
		if st.Load("inventory:" + m.ProjectID).ReadAt.IsZero() {
			t.Errorf("load %s not finished", m.ProjectID)
		}
	}
	if st.Variable("a-prod/a-s/X") == nil || st.Variable("b-dev/b-s/Y") == nil {
		t.Errorf("variables not stored")
	}
	if got := st.SystemVar("a-prod", "a-s", "RAILWAY_PRIVATE_DOMAIN"); got != "a.railway.internal" {
		t.Errorf("system value not stored: %q", got)
	}

	// a partial read: the place it could not read keeps its last variables,
	// and the load says why instead of looking complete
	partial := loaders.InventoryMsg{ProjectID: "a", At: time.Now(), Result: railway.DiscoveryResult{
		ProjectID: "a",
		Warnings:  []string{"a/staging: no access", "a/prod/a-s: rate limited"},
		Unread:    []railway.UnreadPlace{{EnvID: "a-prod", ServiceID: "a-s", Why: "a/prod/a-s: rate limited"}},
	}}
	partial.ApplyTo(st)
	if st.Variable("a-prod/a-s/X") == nil {
		t.Error("a partial read dropped the variables of the place it could not read")
	}
	if got := st.SystemVar("a-prod", "a-s", "RAILWAY_PRIVATE_DOMAIN"); got != "a.railway.internal" {
		t.Errorf("a partial read dropped the system value of an unread place: %q", got)
	}
	if ls := st.Load("inventory:a"); ls.Err != "! a/prod/a-s: rate limited" || ls.ReadAt.IsZero() {
		t.Errorf("partial load = %+v, want a warning", ls)
	}

	failed := loaders.InventoryMsg{ProjectID: "a", Err: errors.New("boom")}
	failed.ApplyTo(st)
	if ls := st.Load("inventory:a"); ls.Err != "boom" || st.Variable("a-prod/a-s/X") == nil {
		t.Errorf("failed load = %+v", ls)
	}

	// an environment without access alone is no partial read
	locked := loaders.InventoryMsg{ProjectID: "a", At: time.Now(), Result: railway.DiscoveryResult{
		ProjectID: "a", Warnings: []string{"a/staging: no access"},
	}}
	locked.ApplyTo(st)
	if ls := st.Load("inventory:a"); ls.Err != "" {
		t.Errorf("a read without failures warns: %+v", ls)
	}
}

func historyClient(t *testing.T) (*railwaytest.Fake, *railway.Client) {
	t.Helper()
	f := railwaytest.New(t)
	return f, &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
}

func historyEntries(n int, at time.Time) []railway.HistoryEntry {
	out := make([]railway.HistoryEntry, n)
	for i := range out {
		out[i] = railway.HistoryEntry{
			ID: fmt.Sprintf("h%03d", i), Object: "Variable", Action: "updated",
			CreatedAt: at.Add(-time.Duration(i) * time.Minute), Names: []string{"N"}, Actor: "u1",
		}
	}
	return out
}

func TestHistoryLoaderAppliesPage(t *testing.T) {
	f, c := historyClient(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	f.SetHistory("e1", historyEntries(150, now))
	f.SetPatches("e1", []railway.EnvPatch{{ID: "p1", Message: "rotate keys", CreatedAt: now.Add(-time.Hour)}})
	f.SetMembers("p", []railway.Member{{ID: "u1", Name: "Ann", Email: "ann@example.com"}})
	st := store.New()

	since := now.Add(-loaders.HistorySince)
	m := loaders.History(c, "e1", "", since, clock)().(loaders.HistoryMsg)
	if m.Err != nil || m.EnvID != "e1" || m.After != "" || len(m.Page.Entries) != 100 || !m.Page.HasNext || !m.At.Equal(now) {
		t.Fatalf("first page msg = env %q after %q entries %d next %v err %v", m.EnvID, m.After, len(m.Page.Entries), m.Page.HasNext, m.Err)
	}
	req, _ := f.LastRequest("EnvironmentHistory")
	if req.Vars["since"] != since.Format(time.RFC3339) {
		t.Errorf("since = %v, want %s", req.Vars["since"], since.Format(time.RFC3339))
	}
	m.ApplyTo(st)
	entries, cursor, hasNext, read := st.History("e1")
	if !read || len(entries) != 100 || !hasNext || cursor == "" {
		t.Fatalf("store after page 1: %d entries, cursor %q, next %v, read %v", len(entries), cursor, hasNext, read)
	}
	if l := st.Load(store.HistoryKey("e1")); !l.ReadAt.Equal(now) || l.Err != "" {
		t.Errorf("load = %+v", l)
	}

	m2 := loaders.History(c, "e1", cursor, since, clock)().(loaders.HistoryMsg)
	if m2.After != cursor {
		t.Errorf("after = %q, want %q", m2.After, cursor)
	}
	m2.ApplyTo(st)
	if entries, _, hasNext, _ = st.History("e1"); len(entries) != 150 || hasNext {
		t.Errorf("store after page 2: %d entries, next %v", len(entries), hasNext)
	}

	pm := loaders.Patches(c, "e1", since, clock)().(loaders.PatchesMsg)
	pm.ApplyTo(st)
	if msg, ok := st.PatchMessage("e1", "p1"); !ok || msg != "rotate keys" || pm.Err != nil {
		t.Errorf("patch = %q, %v (err %v)", msg, ok, pm.Err)
	}
	if l := st.Load(store.PatchesKey("e1")); !l.ReadAt.Equal(now) {
		t.Errorf("patches load = %+v", l)
	}

	mm := loaders.Members(c, "p", clock)().(loaders.MembersMsg)
	mm.ApplyTo(st)
	if got := st.MemberName("p", "u1"); got != "Ann" || mm.Err != nil {
		t.Errorf("member = %q (err %v)", got, mm.Err)
	}
	if l := st.Load(store.MembersKey("p")); !l.ReadAt.Equal(now) {
		t.Errorf("members load = %+v", l)
	}
}

func TestHistoryLoaderFailureKeepsEntries(t *testing.T) {
	st := store.New()
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	page := railway.HistoryPage{Entries: historyEntries(3, at), EndCursor: "e1:3", HasNext: true}
	loaders.HistoryMsg{EnvID: "e1", Page: page, At: at}.ApplyTo(st)
	loaders.PatchesMsg{EnvID: "e1", Patches: []railway.EnvPatch{{ID: "p1", Message: "m"}}, At: at}.ApplyTo(st)
	loaders.MembersMsg{ProjectID: "p", Members: []railway.Member{{ID: "u1", Name: "Ann"}}, At: at}.ApplyTo(st)

	boom := errors.New("boom")
	later := at.Add(time.Minute)
	loaders.HistoryMsg{EnvID: "e1", After: "e1:3", Err: boom, At: later}.ApplyTo(st)
	loaders.HistoryMsg{EnvID: "e1", Err: boom, At: later}.ApplyTo(st)
	loaders.PatchesMsg{EnvID: "e1", Err: boom, At: later}.ApplyTo(st)
	loaders.MembersMsg{ProjectID: "p", Err: boom, At: later}.ApplyTo(st)

	if entries, cursor, hasNext, _ := st.History("e1"); len(entries) != 3 || cursor != "e1:3" || !hasNext {
		t.Errorf("history lost: %d entries, cursor %q, next %v", len(entries), cursor, hasNext)
	}
	if _, ok := st.PatchMessage("e1", "p1"); !ok {
		t.Error("patches lost")
	}
	if st.MemberName("p", "u1") != "Ann" {
		t.Error("members lost")
	}
	for _, k := range []string{store.HistoryKey("e1"), store.PatchesKey("e1"), store.MembersKey("p")} {
		if l := st.Load(k); l.Err != "boom" || !l.ReadAt.Equal(at) {
			t.Errorf("%s load = %+v", k, l)
		}
	}
}

func TestPartialHistoryIsWarning(t *testing.T) {
	st := store.New()
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	warn := &railway.PartialError{Warnings: []string{"entry x denied"}}
	page := railway.HistoryPage{Entries: historyEntries(2, at)}
	loaders.HistoryMsg{EnvID: "e1", Page: page, Err: warn, At: at}.ApplyTo(st)
	loaders.PatchesMsg{EnvID: "e1", Patches: []railway.EnvPatch{{ID: "p1"}}, Err: warn, At: at}.ApplyTo(st)
	loaders.MembersMsg{ProjectID: "p", Members: []railway.Member{{ID: "u1", Name: "Ann"}}, Err: warn, At: at}.ApplyTo(st)

	if entries, _, _, read := st.History("e1"); !read || len(entries) != 2 {
		t.Errorf("partial page not applied: %d entries", len(entries))
	}
	if _, ok := st.PatchMessage("e1", "p1"); !ok {
		t.Error("partial patches not applied")
	}
	if st.MemberName("p", "u1") != "Ann" {
		t.Error("partial members not applied")
	}
	for _, k := range []string{store.HistoryKey("e1"), store.PatchesKey("e1"), store.MembersKey("p")} {
		if l := st.Load(k); l.Err != "! entry x denied" || !l.ReadAt.Equal(at) || l.Loading {
			t.Errorf("%s load = %+v", k, l)
		}
	}
}

func TestHistoryKeys(t *testing.T) {
	if store.HistoryKey("e") != "history:e" || store.PatchesKey("e") != "patches:e" || store.MembersKey("p") != "members:p" {
		t.Error("keys changed")
	}
	if loaders.HistorySince != 30*24*time.Hour {
		t.Errorf("HistorySince = %v", loaders.HistorySince)
	}
}

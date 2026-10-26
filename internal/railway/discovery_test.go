package railway_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
)

// target: project p, env e0 (services s1, s2), env e1 without access.
func target() railway.DiscoveryTarget {
	return railway.DiscoveryTarget{
		ProjectID: "p", ProjectName: "proj",
		Environments: []railway.Environment{
			{ID: "e0", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2"}},
			{ID: "e1", Name: "locked", CanAccess: false, ServiceIDs: []string{"s3"}},
		},
		ServiceNames: map[string]string{"s1": "api", "s2": "web", "s3": "other"},
	}
}

func catalogOf(t railway.DiscoveryTarget) railway.Catalog {
	p := railway.Project{ID: t.ProjectID, Name: t.ProjectName, Workspace: "w", Environments: t.Environments}
	for id, n := range t.ServiceNames {
		p.Services = append(p.Services, railway.Service{ID: id, Name: n})
	}
	sort.Slice(p.Services, func(i, j int) bool { return p.Services[i].ID < p.Services[j].ID })
	return railway.Catalog{Projects: []railway.Project{p}}
}

func TestBuildDiscoveryRequest(t *testing.T) {
	doc, vars := railway.BuildDiscoveryRequestForTest(target())
	for _, a := range []string{"t0:", "sh0u:", "sh0r:", "s0_0u:", "s0_0r:", "s0_1u:", "s0_1r:"} {
		if !strings.Contains(doc, a) {
			t.Errorf("document lacks %s\n%s", a, doc)
		}
	}
	for _, bad := range []string{"t1:", "sh1", "s1_", "e1", "s3"} {
		if strings.Contains(doc, bad) {
			t.Errorf("document contains %q for the inaccessible env\n%s", bad, doc)
		}
	}
	if want := (map[string]any{"p": "p", "e0": "e0", "s0_0": "s1", "s0_1": "s2"}); !reflect.DeepEqual(vars, want) {
		t.Errorf("vars = %v", vars)
	}
	for _, id := range []string{`"p"`, `"e0"`, `"s1"`} {
		if strings.Contains(doc, id) {
			t.Errorf("id %s inlined in document", id)
		}
	}
	if !strings.Contains(doc, "$p: String!") || !strings.Contains(doc, "$s0_1: String!") {
		t.Errorf("declarations missing:\n%s", doc)
	}
}

func TestDiscover(t *testing.T) {
	f := railwaytest.New(t)
	tg := target()
	f.SetCatalog(catalogOf(tg))
	f.SetVar("e0", "s1", "DB", "postgres://x", false)
	f.SetVar("e0", "", "SHARED", "v", false)
	f.SetVar("e0", "s1", "SEC", "hidden", true)
	c := newClient(f)

	res, err := c.Discover(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	// s2 has no variables (empty) and no error; none of the warnings is a not-found.
	got := map[string]railway.VarRecord{}
	for _, r := range res.Vars {
		got[r.ServiceID+"/"+r.Name] = r
	}
	if len(got) != 3 {
		t.Fatalf("records = %+v", res.Vars)
	}
	if r := got["s1/DB"]; r.Unrendered != "postgres://x" || r.Rendered != "postgres://x" || r.EnvID != "e0" || r.UpdatedAt != "2026-01-02T03:04:05" || r.Sealed {
		t.Errorf("DB = %+v", r)
	}
	if r := got["/SHARED"]; r.Unrendered != "v" {
		t.Errorf("SHARED = %+v", r)
	}
	if r := got["s1/SEC"]; r.Unrendered != "" || r.Rendered != "" || !r.Sealed {
		t.Errorf("SEC = %+v", r)
	}
	if want := []string{"proj/locked: no access"}; !reflect.DeepEqual(res.Warnings, want) {
		t.Errorf("warnings = %v", res.Warnings)
	}

	// s2 not deployed in the catalog: Railway says not found: dropped silently.
	tg2 := target()
	tg2.Environments[0].ServiceIDs = []string{"s1", "s2", "ghost"}
	res, err = c.Discover(context.Background(), tg2)
	if err != nil || len(res.Vars) != 3 {
		t.Fatalf("not-found: %+v, %v", res, err)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "not found") {
			t.Errorf("warning for not-found: %q", w)
		}
	}

	// another partial error becomes a warning with the labelled location
	f.FailVars("e0", "s2", "rate limited")
	res, err = c.Discover(context.Background(), tg)
	if err != nil || len(res.Vars) != 3 {
		t.Fatalf("partial: %+v, %v", res, err)
	}
	found := false
	for _, w := range res.Warnings {
		if w == "proj/production/web: rate limited" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", res.Warnings)
	}
	// "not found" inside a longer message is no missing service: a warning
	f.FailVars("e0", "s2", "permission denied: variables not found for this token")
	res, err = c.Discover(context.Background(), tg)
	if err != nil || len(res.Unread) != 1 || res.Unread[0].ServiceID != "s2" {
		t.Errorf("a long not-found message was dropped: %+v, %v", res, err)
	}
	f.FailVars("e0", "s2", "rate limited")
	res, _ = c.Discover(context.Background(), tg)

	// and its place is named as unread, so the last variables there can be
	// kept; the environment without access is not (there was nothing to read)
	want := []railway.UnreadPlace{{EnvID: "e0", ServiceID: "s2", Why: "proj/production/web: rate limited"}}
	if !reflect.DeepEqual(res.Unread, want) {
		t.Errorf("unread = %+v, want %+v", res.Unread, want)
	}
}

func TestDiscoverTimestampPages(t *testing.T) {
	f := railwaytest.New(t)
	f.TimestampPageSize = 2
	tg := target()
	f.SetCatalog(catalogOf(tg))
	for i := range 5 {
		f.SetVar("e0", "s1", fmt.Sprintf("V%d", i), "x", i == 4)
	}
	res, err := newClient(f).Discover(context.Background(), tg)
	if err != nil || len(res.Vars) != 5 {
		t.Fatalf("%+v, %v", res, err)
	}
	for _, r := range res.Vars {
		if r.UpdatedAt == "" {
			t.Errorf("%s has no timestamp (page 2+ not read)", r.Name)
		}
		if r.Name == "V4" && !r.Sealed {
			t.Error("V4 not sealed")
		}
	}
}

func TestDiscoverEnv(t *testing.T) {
	f := railwaytest.New(t)
	tg := target()
	f.SetCatalog(catalogOf(tg))
	f.SetVar("e0", "s2", "A", "1", false)
	f.SetVar("e1", "s3", "B", "2", false)
	recs, err := newClient(f).DiscoverEnv(context.Background(), "p", "e0")
	if err != nil || len(recs) != 1 || recs[0].Name != "A" || recs[0].ServiceID != "s2" {
		t.Fatalf("%+v, %v", recs, err)
	}
	if _, err := newClient(f).DiscoverEnv(context.Background(), "p", "nope"); err == nil {
		t.Error("unknown env: want error")
	}
}

func TestDiscoverAll(t *testing.T) {
	f := railwaytest.New(t)
	var cat railway.Catalog
	var ts []railway.DiscoveryTarget
	for i := range 20 {
		tg := target()
		tg.ProjectID = fmt.Sprintf("p%d", i)
		tg.Environments = []railway.Environment{{ID: fmt.Sprintf("e%d", i), Name: "prod", CanAccess: true, ServiceIDs: []string{fmt.Sprintf("s%d", i)}}}
		tg.ServiceNames = nil
		cat.Projects = append(cat.Projects, railway.Project{ID: tg.ProjectID, Name: "n", Environments: tg.Environments})
		f.SetVar(tg.Environments[0].ID, tg.Environments[0].ServiceIDs[0], "K", "v", false)
		ts = append(ts, tg)
	}
	f.SetCatalog(cat)
	var inCb, calls, maxIn atomic.Int32
	var mu sync.Mutex
	seen := map[string]int{}
	f.Delay = 20 * time.Millisecond
	newClient(f).DiscoverAll(context.Background(), ts, func(r railway.DiscoveryResult, err error) {
		n := inCb.Add(1)
		maxIn.Store(max(maxIn.Load(), n))
		calls.Add(1)
		mu.Lock()
		seen[r.ProjectID] += len(r.Vars)
		mu.Unlock()
		if err != nil {
			t.Error(err)
		}
		inCb.Add(-1)
	})
	if m := f.MaxInFlight(); m > 8 || m < 2 {
		t.Errorf("max in-flight requests = %d, want 2..8", m)
	}
	if calls.Load() != 20 || len(seen) != 20 || maxIn.Load() != 1 {
		t.Errorf("calls=%d seen=%d concurrent callbacks=%d", calls.Load(), len(seen), maxIn.Load())
	}
	for p, n := range seen {
		if n != 1 {
			t.Errorf("%s: %d records", p, n)
		}
	}
}

func TestDiscoverWholeRequestFailure(t *testing.T) {
	f := railwaytest.New(t)
	tg := target()
	f.SetCatalog(catalogOf(tg))
	f.SetVar("e0", "s1", "A", "1", false)
	f.FailNext("Discover", "internal server hiccup")
	res, err := newClient(f).Discover(context.Background(), tg)
	if err == nil || len(res.Vars) != 0 {
		t.Fatalf("want an error, got %+v, %v", res, err)
	}
}

func TestDiscoverRendered(t *testing.T) {
	f := railwaytest.New(t)
	tg := target()
	f.SetCatalog(catalogOf(tg))
	f.SetVar("e0", "s1", "BASE", "base-value", false)
	f.SetVar("e0", "s1", "SELF", "x-${{BASE}}", false)
	f.SetVar("e0", "s1", "OTHER", "${{web.WEBVAR}}/${{shared.SH}}/${{nope.X}}", false)
	f.SetVar("e0", "s2", "WEBVAR", "w", false)
	f.SetVar("e0", "", "SH", "sh", false)
	res, err := newClient(f).Discover(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]railway.VarRecord{}
	for _, r := range res.Vars {
		got[r.ServiceID+"/"+r.Name] = r
	}
	if r := got["s1/SELF"]; r.Unrendered != "x-${{BASE}}" || r.Rendered != "x-base-value" {
		t.Errorf("SELF = %+v", r)
	}
	if r := got["s1/OTHER"]; r.Unrendered == r.Rendered || r.Rendered != "w/sh/${{nope.X}}" {
		t.Errorf("OTHER = %+v", r)
	}
}

func TestDiscoverSystemVars(t *testing.T) {
	f := railwaytest.New(t)
	tg := target()
	f.SetCatalog(catalogOf(tg))
	f.SetVar("e0", "s1", "POSTGRES_PASSWORD", "pw", false)
	f.SetSystemVar("e0", "s1", "RAILWAY_PRIVATE_DOMAIN", "pg.railway.internal")
	f.SetSystemVar("e0", "s1", "RAILWAY_TCP_PROXY_DOMAIN", "pg.proxy.rlwy.net")
	f.SetSystemVar("e0", "s1", "RAILWAY_TCP_PROXY_PORT", "23456")
	f.SetSystemVar("e0", "s1", "RAILWAY_PROJECT_NAME", "proj")
	f.SetSystemVar("e0", "s2", "RAILWAY_PRIVATE_DOMAIN", "web.railway.internal")
	res, err := newClient(f).Discover(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Vars {
		if strings.HasPrefix(r.Name, "RAILWAY_") {
			t.Errorf("system variable in Vars: %s/%s", r.ServiceID, r.Name)
		}
	}
	if len(res.Vars) != 1 {
		t.Errorf("Vars = %d, want 1", len(res.Vars))
	}
	got := map[string]string{}
	for _, r := range res.System {
		if r.EnvID != "e0" {
			t.Errorf("system record env = %q", r.EnvID)
		}
		got[r.ServiceID+"/"+r.Name] = r.Value
	}
	want := map[string]string{
		"s1/RAILWAY_PRIVATE_DOMAIN":   "pg.railway.internal",
		"s1/RAILWAY_TCP_PROXY_DOMAIN": "pg.proxy.rlwy.net",
		"s1/RAILWAY_TCP_PROXY_PORT":   "23456",
		"s2/RAILWAY_PRIVATE_DOMAIN":   "web.railway.internal",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("System = %v\nwant %v", got, want)
	}
}

// A user variable named like a system one is no system value: the
// Postgres tunnel host must never come from a value anyone can set.
func TestDiscoverShadowedSystemVar(t *testing.T) {
	f := railwaytest.New(t)
	tg := target()
	f.SetCatalog(catalogOf(tg))
	f.SetVar("e0", "s1", "RAILWAY_PRIVATE_DOMAIN", "elsewhere.example", false)
	f.SetSystemVar("e0", "s2", "RAILWAY_PRIVATE_DOMAIN", "web.railway.internal")
	res, err := newClient(f).Discover(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.System {
		if r.ServiceID == "s1" {
			t.Errorf("a user variable became a system value: %+v", r)
		}
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "RAILWAY_PRIVATE_DOMAIN")
	}
	if !found || len(res.System) != 1 {
		t.Errorf("warnings = %v, system = %v", res.Warnings, res.System)
	}
}

// An environment the token cannot read (every alias of it fails) leaves the
// project's other environments readable.
func TestDiscoverOneEnvironmentFailing(t *testing.T) {
	f := railwaytest.New(t)
	tg := target()
	tg.Environments[1].CanAccess = true
	f.SetCatalog(catalogOf(tg))
	f.SetVar("e0", "s1", "A", "1", false)
	f.SetVar("e1", "s3", "B", "2", false)
	f.FailVars("e1", "", "forbidden")
	f.FailVars("e1", "s3", "forbidden")
	res, err := newClient(f).Discover(context.Background(), tg)
	if err != nil {
		t.Fatalf("project failed: %v", err)
	}
	var got []string
	for _, r := range res.Vars {
		got = append(got, r.EnvID+"/"+r.Name)
	}
	if !reflect.DeepEqual(got, []string{"e0/A"}) {
		t.Errorf("vars = %v", got)
	}
	n := 0
	for _, w := range res.Warnings {
		if strings.HasPrefix(w, "proj/locked") && strings.Contains(w, "forbidden") {
			n++
		}
	}
	if n == 0 {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

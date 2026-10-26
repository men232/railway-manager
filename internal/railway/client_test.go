package railway_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
)

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

func newClient(f *railwaytest.Fake) *railway.Client {
	return &railway.Client{
		T:            &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}},
		WriteAllowed: func(string) bool { return true },
	}
}

func project(id, name, ws string) railway.Project {
	return railway.Project{
		ID: id, Name: name, Workspace: ws,
		Environments: []railway.Environment{
			{ID: id + "-prod", Name: "production", CanAccess: true, ServiceIDs: []string{id + "-s1", id + "-s2"}},
			{ID: id + "-x", Name: "locked", CanAccess: false},
		},
		Services: []railway.Service{{ID: id + "-s1", Name: "api"}, {ID: id + "-s2", Name: "web"}},
	}
}

func TestWhoAmI(t *testing.T) {
	f := railwaytest.New(t)
	acc, err := newClient(f).WhoAmI(context.Background())
	if err != nil || acc.ID != "u1" || acc.Email == "" {
		t.Fatalf("WhoAmI = %+v, %v", acc, err)
	}
}

func TestListProjects(t *testing.T) {
	f := railwaytest.New(t)
	f.PageSize = 1
	want := []railway.Project{
		project("a", "alpha", "w1"), project("b", "beta", "w1"), project("c", "gamma", "w2"),
	}
	f.SetCatalog(railway.Catalog{Projects: []railway.Project{want[2], want[1], want[0]}})
	got, err := newClient(f).ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Projects, want) {
		t.Fatalf("projects = %+v\nwant %+v", got.Projects, want)
	}
}

func TestCommitPatchShape(t *testing.T) {
	f := railwaytest.New(t)
	f.SetCatalog(railway.Catalog{Projects: []railway.Project{{ID: "p", Environments: []railway.Environment{{ID: "e", CanAccess: true, ServiceIDs: []string{"s1"}}}}}})
	f.SetVar("e", "s1", "A", "0", false)
	f.SetVar("e", "s1", "B", "old", false)
	f.SetVar("e", "", "S", "sealed-old", true)
	c := newClient(f)
	patch := map[string]any{
		"services":        map[string]any{"s1": map[string]any{"variables": map[string]any{"A": map[string]any{"value": "1"}, "B": nil}}},
		"sharedVariables": map[string]any{"S": map[string]any{"value": "2"}},
	}
	id, err := c.CommitPatch(context.Background(), "e", patch, "m")
	if err != nil || id == "" || strings.Contains(id, "/") {
		t.Fatalf("CommitPatch = %q, %v", id, err)
	}
	ms := f.Mutations()
	if len(ms) != 1 || ms[0].Op != "CommitPatch" {
		t.Fatalf("mutations = %+v", ms)
	}
	if ms[0].Vars["skipDeploys"] != true || ms[0].Vars["commitMessage"] != "m" || ms[0].Vars["environmentId"] != "e" {
		t.Errorf("mutation vars = %+v", ms[0].Vars)
	}
	if v, _ := f.Var("e", "s1", "A"); v != "1" {
		t.Errorf("A = %q", v)
	}
	if _, ok := f.Var("e", "s1", "B"); ok {
		t.Error("B not deleted")
	}
	if v, _ := f.Var("e", "", "S"); v != "2" {
		t.Errorf("S = %q", v)
	}
	// sealed stays sealed: unrendered read answers null -> ""
	got, err := c.ReadVariables(context.Background(), "p", "e", "", true)
	if v, ok := got["S"]; err != nil || !ok || v != "" {
		t.Errorf("sealed read = %q, %v, %v", v, ok, err)
	}
}

func TestPatchStatusAndDeployments(t *testing.T) {
	f := railwaytest.New(t)
	f.PatchStatusAfter(2)
	c := newClient(f)
	ctx := context.Background()
	id, err := c.CommitPatch(ctx, "e", map[string]any{}, "m")
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for range 3 {
		st, le, err := c.PatchStatus(ctx, id)
		if err != nil || le != "" {
			t.Fatal(st, le, err)
		}
		seq = append(seq, st)
	}
	if !reflect.DeepEqual(seq, []string{"APPLYING", "APPLYING", "COMMITTED"}) {
		t.Errorf("statuses = %v", seq)
	}

	if a, err := c.ActiveDeployment(ctx, "e", "s"); err != nil || a != "" {
		t.Errorf("active = %q, %v", a, err)
	}
	f.SetActiveDeployment("e", "s", "d1")
	if a, _ := c.ActiveDeployment(ctx, "e", "s"); a != "d1" {
		t.Errorf("active = %q", a)
	}
	f.RedeployBusy(1)
	if err := c.Redeploy(ctx, "e", "s"); err != nil {
		t.Fatal(err)
	}
	ds, err := c.Deployments(ctx, "e", "s")
	if err != nil || len(ds) != 2 || ds[0].Status != "BUILDING" || ds[0].CreatedAt.IsZero() {
		t.Fatalf("deployments = %+v, %v", ds, err)
	}
	if ds, _ = c.Deployments(ctx, "e", "s"); len(ds) != 1 || ds[0].Status != "SUCCESS" {
		t.Errorf("after busy: %+v", ds)
	}
	if err := c.RestartDeployment(ctx, "e", "d1"); err != nil {
		t.Fatal(err)
	}
	ms := f.Mutations()
	if last := ms[len(ms)-1]; last.Op != "RestartDeployment" || last.Vars["id"] != "d1" {
		t.Errorf("last mutation = %+v", last)
	}
}

func TestFailNext(t *testing.T) {
	f := railwaytest.New(t)
	f.FailNext("WhoAmI", "boom")
	c := newClient(f)
	_, err := c.WhoAmI(context.Background())
	var ge *railway.GraphQLError
	if !errors.As(err, &ge) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.WhoAmI(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWriteNotAllowed(t *testing.T) {
	f := railwaytest.New(t)
	for name, wa := range map[string]func(string) bool{"false": func(string) bool { return false }, "nil": nil} {
		c := newClient(f)
		c.WriteAllowed = wa
		ctx := context.Background()
		errs := []error{
			func() error { _, err := c.CommitPatch(ctx, "e", map[string]any{}, "m"); return err }(),
			c.Redeploy(ctx, "e", "s"),
			c.RestartDeployment(ctx, "e", "d"),
		}
		for i, err := range errs {
			var we *railway.WriteNotAllowedError
			if !errors.As(err, &we) || we.EnvID != "e" {
				t.Errorf("%s/%d: err = %v", name, i, err)
			}
		}
	}
	if n := len(f.Mutations()); n != 0 {
		t.Errorf("mutations = %d, want 0", n)
	}
}

func TestPatchAppliesOnCommitted(t *testing.T) {
	f := railwaytest.New(t)
	f.PatchStatusAfter(1)
	c := newClient(f)
	ctx := context.Background()
	f.SetVar("e", "s1", "A", "old", false)
	patch := map[string]any{"services": map[string]any{"s1": map[string]any{"variables": map[string]any{"A": map[string]any{"value": "new"}}}}}
	id, err := c.CommitPatch(ctx, "e", patch, "m")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Var("e", "s1", "A"); v != "old" {
		t.Fatalf("applied at commit: %q", v)
	}
	if st, _, _ := c.PatchStatus(ctx, id); st != "APPLYING" {
		t.Fatalf("status = %s", st)
	}
	if v, _ := f.Var("e", "s1", "A"); v != "old" {
		t.Fatalf("applied while APPLYING: %q", v)
	}
	if st, _, _ := c.PatchStatus(ctx, id); st != "COMMITTED" {
		t.Fatalf("status = %s", st)
	}
	if v, _ := f.Var("e", "s1", "A"); v != "new" {
		t.Errorf("after COMMITTED: %q", v)
	}
}

func TestFailedPatchAppliesNothing(t *testing.T) {
	f := railwaytest.New(t)
	f.FailPatches("conflict")
	c := newClient(f)
	ctx := context.Background()
	f.SetVar("e", "", "A", "old", false)
	id, err := c.CommitPatch(ctx, "e", map[string]any{"sharedVariables": map[string]any{"A": map[string]any{"value": "new"}}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	st, le, err := c.PatchStatus(ctx, id)
	if err != nil || st != "FAILED" || le != "conflict" {
		t.Fatalf("%s %q %v", st, le, err)
	}
	if v, _ := f.Var("e", "", "A"); v != "old" {
		t.Errorf("A = %q", v)
	}
}

func TestMalformedPatch(t *testing.T) {
	f := railwaytest.New(t)
	_, err := newClient(f).CommitPatch(context.Background(), "e", map[string]any{"services": "oops"}, "m")
	var ge *railway.GraphQLError
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.Mutations()); n != 0 {
		t.Errorf("mutations = %d", n)
	}
}

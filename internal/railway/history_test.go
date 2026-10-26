package railway_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestHistoryPaging(t *testing.T) {
	f := railwaytest.New(t)
	var entries []railway.HistoryEntry
	for i := range 250 {
		entries = append(entries, railway.HistoryEntry{
			ID: fmt.Sprintf("h%03d", i), Object: "Variable", Action: "updated",
			CreatedAt:  t0.Add(-time.Duration(i) * time.Minute),
			ServiceIDs: []string{"s1"}, Names: []string{"N"}, Actor: "u1",
		})
	}
	f.SetHistory("e1", entries)
	c := newClient(f)

	var got []railway.HistoryEntry
	var sizes []int
	after := ""
	for range 4 {
		p, err := c.EnvironmentHistory(context.Background(), "e1", after, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(p.Entries))
		got = append(got, p.Entries...)
		if !p.HasNext {
			break
		}
		if p.EndCursor == "" {
			t.Fatalf("page %d: HasNext with no EndCursor", len(sizes))
		}
		after = p.EndCursor
	}
	if !reflect.DeepEqual(sizes, []int{100, 100, 50}) {
		t.Fatalf("page sizes = %v, want [100 100 50]", sizes)
	}
	if !reflect.DeepEqual(got, entries) {
		t.Fatalf("entries differ from the fake's:\n got[0] %+v\nwant[0] %+v", got[0], entries[0])
	}
	req, _ := f.LastRequest("EnvironmentHistory")
	if !strings.Contains(req.Doc, "first: 100") {
		t.Errorf("document lacks first: 100")
	}
	if strings.Contains(req.Doc, "objects") {
		t.Errorf("document filters objects; history reads every object type")
	}
}

func TestHistoryPayloadNamesOnly(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistoryRaw("e1", []map[string]any{{
		"id": "h1", "object": "Variable", "action": "updated", "createdAt": t0.Format(time.RFC3339Nano),
		"serviceIds": []any{"s1"},
		"payload": map[string]any{
			"names": []any{"A", "B"},
			"value": "s3cret",
			"diff":  map[string]any{"A": map[string]any{"old": "s3cret", "new": "s3cret2"}},
		},
		"workflowId": "commitChanges/e1/p1",
		"actor":      map[string]any{"principal": "u1", "onBehalfOf": "s3cret"},
	}})
	p, err := newClient(f).EnvironmentHistory(context.Background(), "e1", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(p.Entries))
	}
	e := p.Entries[0]
	if !reflect.DeepEqual(e.Names, []string{"A", "B"}) {
		t.Errorf("Names = %q, want [A B]", e.Names)
	}
	if e.WorkflowID != "commitChanges/e1/p1" || e.Actor != "u1" || e.Object != "Variable" || e.Action != "updated" {
		t.Errorf("entry = %+v", e)
	}
	if s := fmt.Sprintf("%+v", p); strings.Contains(s, "s3cret") {
		t.Errorf("a payload value survived decoding")
	}
}

func TestHistoryPayloadOddShapes(t *testing.T) {
	cases := map[string]any{
		"null":            nil,
		"string payload":  "s3cret",
		"names not array": map[string]any{"names": "s3cret"},
		"mixed names":     map[string]any{"names": []any{"A", 1, map[string]any{"v": "s3cret"}}},
		"name not string": map[string]any{"name": map[string]any{"v": "s3cret"}},
		"deployment":      map[string]any{"branch": "main", "commitHash": "abc", "commitMessage": "s3cret"},
		"ssh session":     map[string]any{"id": "x", "command": "s3cret"},
		"patch":           map[string]any{},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			f := railwaytest.New(t)
			f.SetHistoryRaw("e1", []map[string]any{{
				"id": "h1", "object": "Variable", "action": "updated", "createdAt": t0.Format(time.RFC3339Nano),
				"serviceIds": nil, "payload": payload, "actor": nil,
			}})
			p, err := newClient(f).EnvironmentHistory(context.Background(), "e1", "", time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Entries) != 1 || p.Entries[0].ID != "h1" {
				t.Fatalf("entries = %+v", p.Entries)
			}
			if s := fmt.Sprintf("%+v", p); strings.Contains(s, "s3cret") {
				t.Errorf("a payload value survived decoding")
			}
		})
	}
}

func TestHistorySince(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistory("e1", []railway.HistoryEntry{
		{ID: "new", Object: "Variable", Action: "updated", CreatedAt: t0},
		{ID: "old", Object: "Variable", Action: "updated", CreatedAt: t0.Add(-48 * time.Hour)},
	})
	since := time.Date(2026, 9, 30, 15, 4, 5, 0, time.FixedZone("x", 3600))
	p, err := newClient(f).EnvironmentHistory(context.Background(), "e1", "", since)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := f.LastRequest("EnvironmentHistory")
	if got := req.Vars["since"]; got != "2026-09-30T14:04:05Z" {
		t.Errorf("since = %v, want 2026-09-30T14:04:05Z", got)
	}
	if req.Vars["environmentId"] != "e1" {
		t.Errorf("environmentId = %v", req.Vars["environmentId"])
	}
	if len(p.Entries) != 1 || p.Entries[0].ID != "new" {
		t.Errorf("entries = %+v, want only new", p.Entries)
	}

	// A zero since sends no filter value.
	if _, err := newClient(f).EnvironmentHistory(context.Background(), "e1", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	req, _ = f.LastRequest("EnvironmentHistory")
	if req.Vars["since"] != nil {
		t.Errorf("zero since sent %v", req.Vars["since"])
	}
}

func TestHistoryFailure(t *testing.T) {
	f := railwaytest.New(t)
	f.FailNext("EnvironmentHistory", "Problem processing request")
	_, err := newClient(f).EnvironmentHistory(context.Background(), "e1", "", time.Time{})
	var gqe *railway.GraphQLError
	if !errors.As(err, &gqe) {
		t.Fatalf("err = %v, want a GraphQLError", err)
	}

	f.SetHistory("e1", []railway.HistoryEntry{{ID: "h1", Object: "Variable", Action: "updated", CreatedAt: t0}})
	f.PartialError("EnvironmentHistory", "Problem reading one entry")
	p, err := newClient(f).EnvironmentHistory(context.Background(), "e1", "", time.Time{})
	var pe *railway.PartialError
	if !errors.As(err, &pe) || len(p.Entries) != 1 {
		t.Fatalf("entries = %d, err = %v; want 1 entry and a PartialError", len(p.Entries), err)
	}
}

func TestHistoryPayloadSingleName(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistoryRaw("e1", []map[string]any{
		{
			"id": "h1", "object": "Variable", "action": "created", "createdAt": t0.Format(time.RFC3339Nano),
			"payload": map[string]any{"id": "v1", "isSealed": false, "name": "DATABASE_URL", "serviceId": "s1", "value": "s3cret"},
		},
		{
			"id": "h2", "object": "Variable", "action": "updated", "createdAt": t0.Format(time.RFC3339Nano),
			"payload": map[string]any{"names": []any{"A", "B"}, "name": "C"},
		},
		{
			"id": "h3", "object": "Variable", "action": "deleted", "createdAt": t0.Format(time.RFC3339Nano),
			"payload": map[string]any{"name": ""},
		},
	})
	p, err := newClient(f).EnvironmentHistory(context.Background(), "e1", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"DATABASE_URL"}, {"A", "B"}, nil}
	for i, e := range p.Entries {
		if !reflect.DeepEqual(e.Names, want[i]) {
			t.Errorf("%s: Names = %q, want %q", e.ID, e.Names, want[i])
		}
	}
	if s := fmt.Sprintf("%+v", p); strings.Contains(s, "s3cret") || strings.Contains(s, "v1") {
		t.Errorf("a payload field other than the names survived decoding: %s", s)
	}
}

func TestFakeHistoryRejectsBadCursor(t *testing.T) {
	f := railwaytest.New(t)
	var entries []railway.HistoryEntry
	for i := range 150 {
		entries = append(entries, railway.HistoryEntry{ID: fmt.Sprintf("h%d", i), Object: "Variable", Action: "updated", CreatedAt: t0})
	}
	f.SetHistory("e1", entries)
	f.SetHistory("e2", entries)
	c := newClient(f)
	p, err := c.EnvironmentHistory(context.Background(), "e1", "", time.Time{})
	if err != nil || !p.HasNext {
		t.Fatalf("first page: %v, HasNext %v", err, p.HasNext)
	}
	for _, after := range []string{"bogus", "e1:abc", "e1:-1", "e1:999", strings.Replace(p.EndCursor, "e1", "e2", 1)} {
		_, err := c.EnvironmentHistory(context.Background(), "e1", after, time.Time{})
		var gqe *railway.GraphQLError
		if !errors.As(err, &gqe) {
			t.Errorf("after %q: err = %v, want a GraphQLError", after, err)
		}
	}
}

func patchesFixture(n int) []railway.EnvPatch {
	out := make([]railway.EnvPatch, n)
	for i := range out {
		// Given oldest first; the fake answers newest first.
		out[i] = railway.EnvPatch{ID: fmt.Sprintf("p%03d", i), Message: fmt.Sprintf("run %d", i), CreatedAt: t0.Add(time.Duration(i-n) * time.Hour)}
	}
	return out
}

func TestEnvironmentPatches(t *testing.T) {
	f := railwaytest.New(t)
	all := patchesFixture(250) // p249 is newest, at t0-1h
	f.SetPatches("e1", all)
	f.SetPatches("e2", []railway.EnvPatch{{ID: "other", CreatedAt: t0}})
	c := newClient(f)

	since := all[120].CreatedAt
	got, err := c.EnvironmentPatches(context.Background(), "e1", since)
	if err != nil {
		t.Fatal(err)
	}
	var want []railway.EnvPatch
	for i := 249; i >= 120; i-- {
		want = append(want, all[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %d patches (first %+v), want %d (first %+v)", len(got), got[0], len(want), want[0])
	}
	// Patches p249..p150 then p149..p050: the second page's oldest is before since, so no third page.
	if n := f.Requests("EnvironmentPatches"); n != 2 {
		t.Errorf("pages read = %d, want 2", n)
	}
	req, _ := f.LastRequest("EnvironmentPatches")
	if req.Vars["environmentId"] != "e1" || !strings.Contains(req.Doc, "pageInfo { hasNextPage endCursor }") {
		t.Errorf("request = %+v", req)
	}

	// A zero since reads every page.
	got, err = c.EnvironmentPatches(context.Background(), "e1", time.Time{})
	if err != nil || len(got) != 250 || got[0].ID != "p249" || got[249].ID != "p000" {
		t.Fatalf("all: %d patches, err %v", len(got), err)
	}
}

func TestEnvironmentPatchesPageCap(t *testing.T) {
	f := railwaytest.New(t)
	f.SetPatches("e1", patchesFixture(1200))
	got, err := newClient(f).EnvironmentPatches(context.Background(), "e1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1000 || f.Requests("EnvironmentPatches") != 10 {
		t.Fatalf("got %d patches in %d pages, want 1000 in 10", len(got), f.Requests("EnvironmentPatches"))
	}
}

func TestEnvironmentPatchesFailure(t *testing.T) {
	f := railwaytest.New(t)
	f.SetPatches("e1", patchesFixture(3))
	f.FailNext("EnvironmentPatches", "Problem processing request")
	_, err := newClient(f).EnvironmentPatches(context.Background(), "e1", time.Time{})
	var gqe *railway.GraphQLError
	if !errors.As(err, &gqe) {
		t.Fatalf("err = %v, want a GraphQLError", err)
	}
	f.PartialError("EnvironmentPatches", "Problem reading one patch")
	got, err := newClient(f).EnvironmentPatches(context.Background(), "e1", time.Time{})
	var pe *railway.PartialError
	if !errors.As(err, &pe) || len(got) != 3 {
		t.Fatalf("patches = %d, err = %v; want 3 and a PartialError", len(got), err)
	}
}

func TestProjectMembers(t *testing.T) {
	f := railwaytest.New(t)
	want := []railway.Member{
		{ID: "u1", Name: "Ada", Email: "ada@example.test"},
		{ID: "u2", Name: "", Email: "bob@example.test"},
	}
	f.SetMembers("p1", want)
	got, err := newClient(f).ProjectMembers(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("members = %+v\nwant %+v", got, want)
	}
	req, _ := f.LastRequest("ProjectMembers")
	if req.Vars["projectId"] != "p1" || !strings.Contains(req.Doc, "workspace { members { id name email } }") {
		t.Errorf("request = %+v", req)
	}

	// A project with no workspace: no members, no error.
	got, err = newClient(f).ProjectMembers(context.Background(), "p-none")
	if err != nil || len(got) != 0 {
		t.Errorf("no workspace: members = %+v, err = %v", got, err)
	}
}

func TestProjectMembersFailure(t *testing.T) {
	f := railwaytest.New(t)
	f.SetMembers("p1", []railway.Member{{ID: "u1", Email: "ada@example.test"}})
	f.FailNext("ProjectMembers", "Problem processing request")
	_, err := newClient(f).ProjectMembers(context.Background(), "p1")
	var gqe *railway.GraphQLError
	if !errors.As(err, &gqe) {
		t.Fatalf("err = %v, want a GraphQLError", err)
	}
	f.PartialError("ProjectMembers", "Problem reading one member")
	got, err := newClient(f).ProjectMembers(context.Background(), "p1")
	var pe *railway.PartialError
	if !errors.As(err, &pe) || len(got) != 1 {
		t.Fatalf("members = %d, err = %v; want 1 and a PartialError", len(got), err)
	}
}

// Only a Variable entry's payload is read: another object's `name` or
// `names` (an SSH session, a volume, a domain…) is not a variable name.
func TestHistoryNamesOnlyForVariables(t *testing.T) {
	f := railwaytest.New(t)
	f.SetHistoryRaw("e1", []map[string]any{
		{"id": "h1", "object": "SSHSession", "action": "created", "createdAt": t0.Format(time.RFC3339Nano),
			"payload": map[string]any{"name": "SSH_NAME"}},
		{"id": "h2", "object": "Volume", "action": "updated", "createdAt": t0.Format(time.RFC3339Nano),
			"payload": map[string]any{"names": []any{"VOL_A", "VOL_B"}}},
		{"id": "h3", "object": "Domain", "action": "created", "createdAt": t0.Format(time.RFC3339Nano),
			"payload": map[string]any{"name": "example.test", "names": []any{"DOM"}}},
		{"id": "h4", "object": "Variable", "action": "updated", "createdAt": t0.Format(time.RFC3339Nano),
			"payload": map[string]any{"name": "KEEP"}},
	})
	p, err := newClient(f).EnvironmentHistory(context.Background(), "e1", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(p.Entries))
	}
	for _, e := range p.Entries[:3] {
		if e.Names != nil {
			t.Errorf("%s (%s): Names = %q, want nil", e.ID, e.Object, e.Names)
		}
	}
	if !reflect.DeepEqual(p.Entries[3].Names, []string{"KEEP"}) {
		t.Errorf("Variable Names = %q, want [KEEP]", p.Entries[3].Names)
	}
}

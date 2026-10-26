package store

import (
	"slices"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/railway"
)

func entry(id string) railway.HistoryEntry {
	return railway.HistoryEntry{ID: id, Object: "Variable", Action: "updated", Names: []string{"N_" + id}}
}

func entryIDs(es []railway.HistoryEntry) []string {
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.ID
	}
	return ids
}

func page(end string, next bool, ids ...string) railway.HistoryPage {
	p := railway.HistoryPage{EndCursor: end, HasNext: next}
	for _, id := range ids {
		p.Entries = append(p.Entries, entry(id))
	}
	return p
}

func TestHistoryFirstPageReplaces(t *testing.T) {
	s := New()
	if _, _, _, read := s.History("e1"); read {
		t.Fatal("history read before any page")
	}
	s.AppendHistory("e1", "", page("c1", true, "a", "b"))
	s.AppendHistory("e1", "c1", page("c2", true, "c"))
	before := s.Rev(History)
	s.AppendHistory("e1", "", page("n1", false, "x", "a"))
	if s.Rev(History) != before+1 {
		t.Errorf("rev = %d, want %d", s.Rev(History), before+1)
	}
	es, end, next, read := s.History("e1")
	if got := entryIDs(es); !slices.Equal(got, []string{"x", "a"}) || end != "n1" || next || !read {
		t.Errorf("History = %v %q %v %v", got, end, next, read)
	}
	if _, _, _, read := s.History("e2"); read {
		t.Error("first page of e1 marked e2 read")
	}
}

func TestHistoryNextPageAppends(t *testing.T) {
	s := New()
	s.AppendHistory("e1", "", page("c1", true, "a", "b"))
	s.AppendHistory("e1", "c1", page("c2", false, "c", "d"))
	es, end, next, _ := s.History("e1")
	if got := entryIDs(es); !slices.Equal(got, []string{"a", "b", "c", "d"}) || end != "c2" || next {
		t.Errorf("History = %v %q %v", got, end, next)
	}
}

func TestHistoryStalePageDropped(t *testing.T) {
	s := New()
	// A next page for an env never read is dropped.
	s.AppendHistory("e1", "c1", page("c2", true, "z"))
	if es, _, _, read := s.History("e1"); read || len(es) != 0 {
		t.Errorf("unread env took a next page: %v %v", entryIDs(es), read)
	}
	s.AppendHistory("e1", "", page("c1", true, "a"))
	s.AppendHistory("e1", "c1", page("c2", true, "b"))
	before := s.Rev(History)
	// The page after c1 again (a second fetch in flight) and an unknown cursor.
	s.AppendHistory("e1", "c1", page("c2", true, "b2"))
	s.AppendHistory("e1", "other", page("c9", true, "q"))
	if s.Rev(History) != before {
		t.Errorf("stale page bumped rev: %d -> %d", before, s.Rev(History))
	}
	es, end, next, _ := s.History("e1")
	if got := entryIDs(es); !slices.Equal(got, []string{"a", "b"}) || end != "c2" || !next {
		t.Errorf("History = %v %q %v", got, end, next)
	}
}

func TestHistoryDedup(t *testing.T) {
	s := New()
	s.AppendHistory("e1", "", page("c1", true, "a", "b", "a"))
	s.AppendHistory("e1", "c1", page("c2", false, "b", "c"))
	es, _, _, _ := s.History("e1")
	if got := entryIDs(es); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("History = %v, want [a b c]", got)
	}
}

func TestMemberNameFallbacks(t *testing.T) {
	s := New()
	before := s.Rev(Members)
	s.SetMembers("p", []railway.Member{
		{ID: "u-named", Name: "Ada", Email: "ada@example.com"},
		{ID: "u-email", Email: "bob@example.com"},
		{ID: "0123456789abcdef"},
	})
	if s.Rev(Members) != before+1 {
		t.Errorf("Members rev = %d, want %d", s.Rev(Members), before+1)
	}
	cases := []struct{ project, principal, want string }{
		{"p", "u-named", "Ada"},
		{"p", "u-email", "bob@example.com"},
		{"p", "0123456789abcdef", "01234567"},
		{"p", "fedcba9876543210", "fedcba98"}, // not a member
		{"p", "short", "short"},
		{"q", "u-named", "u-named"}, // another project
		{"p", "", ""},
	}
	for _, c := range cases {
		if got := s.MemberName(c.project, c.principal); got != c.want {
			t.Errorf("MemberName(%q, %q) = %q, want %q", c.project, c.principal, got, c.want)
		}
	}
	s.SetMembers("p", nil)
	if got := s.MemberName("p", "u-named"); got != "u-named" {
		t.Errorf("after replace: %q", got)
	}
}

func TestPatchMessage(t *testing.T) {
	s := New()
	before := s.Rev(Patches)
	s.SetPatches("e1", []railway.EnvPatch{{ID: "p1", Message: "rotate keys"}, {ID: "p2"}})
	if s.Rev(Patches) != before+1 {
		t.Errorf("Patches rev = %d, want %d", s.Rev(Patches), before+1)
	}
	if m, ok := s.PatchMessage("e1", "p1"); !ok || m != "rotate keys" {
		t.Errorf("p1 = %q %v", m, ok)
	}
	if m, ok := s.PatchMessage("e1", "p2"); !ok || m != "" {
		t.Errorf("p2 = %q %v", m, ok)
	}
	if _, ok := s.PatchMessage("e2", "p1"); ok {
		t.Error("patch leaked across environments")
	}
	s.SetPatches("e1", nil)
	if _, ok := s.PatchMessage("e1", "p1"); ok {
		t.Error("replaced patch kept")
	}
}

func TestHistoryCloneIsDeep(t *testing.T) {
	s := New()
	p := page("c1", true, "a")
	p.Entries[0].ServiceIDs = []string{"s1"}
	p.Entries[0].CreatedAt = time.Unix(1, 0)
	s.AppendHistory("e1", "", p)
	// The store owns what it was given.
	p.Entries[0].Names[0] = "CHANGED"
	p.Entries[0].ServiceIDs[0] = "CHANGED"
	s.SetMembers("p", []railway.Member{{ID: "u1", Name: "Ada"}})
	s.SetPatches("e1", []railway.EnvPatch{{ID: "p1", Message: "m"}})

	c := s.Clone()
	es, _, _, _ := c.History("e1")
	if es[0].Names[0] != "N_a" || es[0].ServiceIDs[0] != "s1" {
		t.Fatalf("store aliased the page: %+v", es[0])
	}
	// Mutate the original through what it hands out and through writes.
	orig, _, _, _ := s.History("e1")
	orig[0].Names[0] = "X"
	orig[0].ServiceIDs[0] = "X"
	s.AppendHistory("e1", "c1", page("c2", false, "b"))
	s.SetMembers("p", []railway.Member{{ID: "u1", Name: "Bob"}})
	s.SetPatches("e1", []railway.EnvPatch{{ID: "p1", Message: "other"}})

	es, end, next, _ := c.History("e1")
	if got := entryIDs(es); !slices.Equal(got, []string{"a"}) || end != "c1" || !next {
		t.Errorf("clone history = %v %q %v", got, end, next)
	}
	if es[0].Names[0] != "N_a" || es[0].ServiceIDs[0] != "s1" {
		t.Errorf("clone entry shared: %+v", es[0])
	}
	if got := c.MemberName("p", "u1"); got != "Ada" {
		t.Errorf("clone member = %q", got)
	}
	if m, _ := c.PatchMessage("e1", "p1"); m != "m" {
		t.Errorf("clone patch = %q", m)
	}
	if c.Rev(History) != 1 {
		t.Errorf("clone rev = %d", c.Rev(History))
	}
}

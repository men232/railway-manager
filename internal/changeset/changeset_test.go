package changeset

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/domain"
)

func chg(id, target, val string) Change {
	return Change{
		ID: id, Kind: "variable.set",
		Place:  domain.Place{ProjectID: "p", EnvID: "e", ServiceID: "s"},
		Target: target, New: json.RawMessage(val),
		Base: Base{Fingerprint: "f0"},
	}
}

func mustPut(t *testing.T, s *ChangeSet, c Change) {
	t.Helper()
	if err := s.Put(c, never); err != nil {
		t.Fatal(err)
	}
}

func never(Change) bool { return false }

func TestPutMergesKeepingBase(t *testing.T) {
	var s ChangeSet
	if err := s.Put(chg("id1", "A", "1"), never); err != nil {
		t.Fatal(err)
	}
	c2 := chg("id2", "A", "2")
	c2.Base = Base{Fingerprint: "f1"}
	if err := s.Put(c2, never); err != nil {
		t.Fatal(err)
	}
	if len(s.Changes) != 1 {
		t.Fatalf("got %d changes", len(s.Changes))
	}
	got := s.Changes[0]
	if string(got.New) != "2" || got.Base.Fingerprint != "f0" || got.ID != "id1" {
		t.Errorf("merged = %+v", got)
	}
	if s.Rev != 2 {
		t.Errorf("Rev = %d", s.Rev)
	}
}

func TestPutAssignsID(t *testing.T) {
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	s := ChangeSet{Now: func() time.Time { return now }}
	mustPut(t, &s, chg("", "A", "1"))
	if len(s.Changes[0].ID) != 16 {
		t.Errorf("ID = %q", s.Changes[0].ID)
	}
	if !s.Changes[0].Created.Equal(now) {
		t.Errorf("Created = %v", s.Changes[0].Created)
	}
}

func TestPutBackToBaseRemoves(t *testing.T) {
	var s ChangeSet
	mustPut(t, &s, chg("id1", "A", "1"))
	if err := s.Put(chg("id2", "A", "0"), func(Change) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if len(s.Changes) != 0 {
		t.Errorf("set not empty: %+v", s.Changes)
	}
}

func TestPutLocked(t *testing.T) {
	var s ChangeSet
	c := chg("id1", "A", "1")
	c.RunID = "r1"
	s.Changes = []Change{c}
	if err := s.Put(chg("id2", "A", "2"), never); !errors.Is(err, ErrLocked) {
		t.Errorf("Put err = %v", err)
	}
	if err := s.Remove("id1"); !errors.Is(err, ErrLocked) {
		t.Errorf("Remove err = %v", err)
	}
	if len(s.Changes) != 1 || string(s.Changes[0].New) != "1" {
		t.Errorf("set modified: %+v", s.Changes)
	}
}

func TestRemoveGetLockUnlockDrop(t *testing.T) {
	var s ChangeSet
	mustPut(t, &s, chg("a", "A", "1"))
	mustPut(t, &s, chg("b", "B", "1"))
	mustPut(t, &s, chg("c", "C", "1"))
	if _, ok := s.Get("b"); !ok {
		t.Error("Get b")
	}
	if _, ok := s.Get("zz"); ok {
		t.Error("Get zz")
	}
	if err := s.Lock([]string{"a", "b"}, "r1"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Get("a"); c.RunID != "r1" {
		t.Errorf("a = %+v", c)
	}
	s.Unlock("r1", map[string]string{"a": "written, not deployed"})
	a, _ := s.Get("a")
	b, _ := s.Get("b")
	if a.RunID != "" || a.Flag != "written, not deployed" || b.RunID != "" || b.Flag != "" {
		t.Errorf("a=%+v b=%+v", a, b)
	}
	if err := s.Remove("c"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("c"); err == nil {
		t.Error("remove missing should error")
	}
	s.Drop([]string{"a"})
	if len(s.Changes) != 1 || s.Changes[0].ID != "b" {
		t.Errorf("after drop: %+v", s.Changes)
	}
}

func TestToggleRedeploy(t *testing.T) {
	var s ChangeSet
	s.ToggleRedeploy("e/s")
	if !reflect.DeepEqual(s.RedeployOff, []string{"e/s"}) {
		t.Fatalf("%v", s.RedeployOff)
	}
	s.ToggleRedeploy("e/s")
	if len(s.RedeployOff) != 0 {
		t.Fatalf("%v", s.RedeployOff)
	}
}

func TestSelect(t *testing.T) {
	s := ChangeSet{RedeployOff: []string{"e/s"}}
	for _, id := range []string{"a", "b", "c"} {
		mustPut(t, &s, chg(id, id, "1"))
	}
	sel, rest := s.Select([]string{"a", "c"})
	if len(sel.Changes) != 2 || len(rest.Changes) != 1 || rest.Changes[0].ID != "b" {
		t.Fatalf("sel=%+v rest=%+v", sel.Changes, rest.Changes)
	}
	if !reflect.DeepEqual(sel.RedeployOff, []string{"e/s"}) || !reflect.DeepEqual(rest.RedeployOff, []string{"e/s"}) {
		t.Errorf("RedeployOff not copied")
	}
	sel.ToggleRedeploy("e/s")
	if len(rest.RedeployOff) != 1 {
		t.Error("RedeployOff aliased")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changeset.json")
	c := chg("id1", "A", `"x"`)
	c.RunID, c.Flag, c.Group = "r1", "skipped: changed in Railway since", "g1"
	c.Origin = &Origin{Producer: "rotation", Row: "row1", Letter: "b"}
	c.Requires = []string{"id0"}
	c.Created = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := ChangeSet{Changes: []Change{c}, RedeployOff: []string{"e/s"}, Rev: 9}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Rev = 0
	if !reflect.DeepEqual(got, s) {
		t.Errorf("got %+v\nwant %+v", got, s)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "Rev") || strings.Contains(string(raw), "rev") {
		t.Errorf("Rev serialized: %s", raw)
	}
}

func TestLoadMissing(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "changeset.json"))
	if err != nil || len(got.Changes) != 0 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestLoadCorruptChangeSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changeset.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	got, err := Load(path)
	var ce *CorruptError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if len(got.Changes) != 0 {
		t.Errorf("set not empty")
	}
	if !strings.HasPrefix(filepath.Base(ce.MovedTo), "changeset.json.corrupt-") {
		t.Errorf("MovedTo = %q", ce.MovedTo)
	}
	if b, err := os.ReadFile(ce.MovedTo); err != nil || string(b) != "{not json" {
		t.Errorf("moved file: %q %v", b, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("original still exists: %v", err)
	}
}

type fakeEffects struct{ reads map[string][]string }

func (f fakeEffects) Writes(c Change) []string { return []string{"var:" + c.Key()} }
func (f fakeEffects) Reads(c Change) []string  { return f.reads[c.ID] }

func set(cs ...Change) ChangeSet { return ChangeSet{Changes: cs} }

func TestCheckConflicts(t *testing.T) {
	e := fakeEffects{reads: map[string][]string{}}
	cand := set(chg("c1", "A", "1"))

	t.Run("write-write", func(t *testing.T) {
		got := CheckConflicts(e, cand, map[string]ChangeSet{"r1": set(chg("o1", "A", "2"))})
		if len(got) != 1 || got[0].OtherRun != "r1" || got[0].Candidate.ID != "c1" || got[0].Other.ID != "o1" ||
			got[0].Target != "var:e/s|A" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("candidate reads what other writes", func(t *testing.T) {
		e := fakeEffects{reads: map[string][]string{"c1": {"var:e/s|B"}}}
		got := CheckConflicts(e, cand, map[string]ChangeSet{"r1": set(chg("o1", "B", "2"))})
		if len(got) != 1 || got[0].Target != "var:e/s|B" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("candidate writes what other reads", func(t *testing.T) {
		e := fakeEffects{reads: map[string][]string{"o1": {"var:e/s|A"}}}
		got := CheckConflicts(e, cand, map[string]ChangeSet{"r1": set(chg("o1", "B", "2"))})
		if len(got) != 1 || got[0].Target != "var:e/s|A" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("disjoint", func(t *testing.T) {
		got := CheckConflicts(e, cand, map[string]ChangeSet{"r1": set(chg("o1", "B", "2"))})
		if len(got) != 0 {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("unselected rest", func(t *testing.T) {
		got := CheckConflicts(e, cand, map[string]ChangeSet{"": set(chg("o1", "A", "2"))})
		if len(got) != 1 || got[0].OtherRun != "" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("deterministic order", func(t *testing.T) {
		active := map[string]ChangeSet{
			"r2": set(chg("o2", "A", "2")),
			"r1": set(chg("o1", "A", "2")),
			"":   set(chg("o0", "A", "2")),
		}
		for i := 0; i < 20; i++ {
			got := CheckConflicts(e, cand, active)
			if len(got) != 3 || got[0].OtherRun != "" || got[1].OtherRun != "r1" || got[2].OtherRun != "r2" {
				t.Fatalf("got %+v", got)
			}
		}
	})
}

func TestLockRefusesOtherRun(t *testing.T) {
	var s ChangeSet
	mustPut(t, &s, chg("a", "A", "1"))
	mustPut(t, &s, chg("b", "B", "1"))
	if err := s.Lock([]string{"a"}, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Lock([]string{"b", "a"}, "r2"); !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v", err)
	}
	a, _ := s.Get("a")
	b, _ := s.Get("b")
	if a.RunID != "r1" || b.RunID != "" {
		t.Errorf("a=%+v b=%+v", a, b)
	}
}

func TestCorruptQuarantineNeverOverwrites(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	quarantineNow = func() time.Time { return fixed }
	t.Cleanup(func() { quarantineNow = time.Now })
	path := filepath.Join(t.TempDir(), "changeset.json")
	var moved []string
	for _, body := range []string{"{first", "{second"} {
		os.WriteFile(path, []byte(body), 0o600)
		_, err := Load(path)
		var ce *CorruptError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v", err)
		}
		moved = append(moved, ce.MovedTo)
	}
	if moved[0] == moved[1] || !strings.HasSuffix(moved[0], ".corrupt-20260102-030405") {
		t.Fatalf("moved = %v", moved)
	}
	for i, want := range []string{"{first", "{second"} {
		if b, err := os.ReadFile(moved[i]); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", moved[i], b, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("original remains")
	}
}

// A change a run skipped as drifted is re-edited against what the user sees
// now: the merge takes the new base and clears the flag.
func TestPutReEditOfSkippedTakesNewBase(t *testing.T) {
	var s ChangeSet
	mustPut(t, &s, chg("id1", "A", "1"))
	s.Changes[0].Flag = "skipped: changed in Railway since the change was made"
	c2 := chg("id2", "A", "2")
	c2.Base = Base{Fingerprint: "f1"}
	mustPut(t, &s, c2)
	got := s.Changes[0]
	if got.Base.Fingerprint != "f1" || got.Flag != "" || got.ID != "id1" || string(got.New) != "2" {
		t.Errorf("merged = %+v, want base f1, no flag, id1, new 2", got)
	}

	// Any other flag keeps the older base.
	s.Changes[0].Flag = "written, not deployed"
	c3 := chg("id3", "A", "3")
	c3.Base = Base{Fingerprint: "f2"}
	mustPut(t, &s, c3)
	if got := s.Changes[0]; got.Base.Fingerprint != "f1" {
		t.Errorf("base = %q, want f1 kept", got.Base.Fingerprint)
	}
}

func TestPutKeepsGroup(t *testing.T) {
	var s ChangeSet
	c1 := chg("id1", "A", "1")
	c1.Group = "rotate:fp1"
	c1.Origin = &Origin{Producer: "rotate", Row: "fp1", Letter: "a"}
	mustPut(t, &s, c1)

	// A newer change without a group (an edit in Variables) keeps both.
	mustPut(t, &s, chg("id2", "A", "2"))
	got := s.Changes[0]
	if got.Group != "rotate:fp1" || got.Origin == nil || *got.Origin != *c1.Origin || string(got.New) != "2" {
		t.Errorf("merged = %+v, want group and origin kept, new 2", got)
	}

	// A newer change with its own group replaces both.
	c3 := chg("id3", "A", "3")
	c3.Group = "rotate:fp2"
	c3.Origin = &Origin{Producer: "rotate", Row: "fp2"}
	mustPut(t, &s, c3)
	got = s.Changes[0]
	if got.Group != "rotate:fp2" || got.Origin == nil || got.Origin.Row != "fp2" {
		t.Errorf("merged = %+v, want group rotate:fp2", got)
	}
}

// Clone is a deep copy: no write to either side reaches the other.
func TestCloneIsIndependent(t *testing.T) {
	var s ChangeSet
	c := chg("id1", "A", `{"value":"1"}`)
	c.Requires = []string{"r1"}
	c.Origin = &Origin{Producer: "rotate", Row: "row", Letter: "a"}
	c.Base = Base{Fingerprint: "f0", Value: "v0"}
	mustPut(t, &s, c)
	s.ToggleRedeploy("place-1")
	want := s.Clone()
	if !reflect.DeepEqual(want.Changes, s.Changes) || !reflect.DeepEqual(want.RedeployOff, s.RedeployOff) || want.Rev != s.Rev {
		t.Fatalf("clone differs: %+v vs %+v", want, s)
	}
	cl := s.Clone()

	// Writes to the original do not reach the clone...
	s.Changes[0].New[1] = 'X'
	s.Changes[0].Requires[0] = "changed"
	s.Changes[0].Origin.Row = "changed"
	s.Changes[0].Base.Value = "changed"
	s.Changes[0].RunID = "run"
	s.RedeployOff[0] = "changed"
	s.Rev++
	mustPut(t, &s, chg("id2", "B", "2"))
	if !reflect.DeepEqual(cl.Changes, want.Changes) || !reflect.DeepEqual(cl.RedeployOff, want.RedeployOff) || cl.Rev != want.Rev {
		t.Errorf("clone changed with the original: %+v", cl)
	}
	// ... nor the clone's to the original.
	before := s.Clone()
	cl.Changes[0].New[1] = 'Y'
	cl.Changes[0].Requires[0] = "clone"
	cl.Changes[0].Origin.Letter = "clone"
	cl.RedeployOff[0] = "clone"
	if !reflect.DeepEqual(s.Changes, before.Changes) || !reflect.DeepEqual(s.RedeployOff, before.RedeployOff) {
		t.Errorf("original changed with the clone: %+v", s)
	}

	var empty ChangeSet
	if e := empty.Clone(); e.Changes != nil || e.RedeployOff != nil {
		t.Errorf("clone of empty = %+v", e)
	}
}

// Flag sets the flags of the run's changes, keeps them held and bumps Rev.
func TestFlagKeepsHeldAndBumpsRev(t *testing.T) {
	var s ChangeSet
	mustPut(t, &s, chg("id1", "A", "1"))
	mustPut(t, &s, chg("id2", "B", "2"))
	if err := s.Lock([]string{"id1"}, "run-1"); err != nil {
		t.Fatal(err)
	}
	rev := s.Rev
	s.Flag("run-1", map[string]string{"id1": "failed: x", "id2": "not this run"})
	a, _ := s.Get("id1")
	b, _ := s.Get("id2")
	if a.Flag != "failed: x" || a.RunID != "run-1" || b.Flag != "" {
		t.Errorf("after Flag: %+v %+v", a, b)
	}
	if s.Rev <= rev {
		t.Errorf("Rev not bumped")
	}
}

// UnlockChanges releases only the named changes the run holds.
func TestUnlockChangesScoped(t *testing.T) {
	var s ChangeSet
	mustPut(t, &s, chg("id1", "A", "1"))
	mustPut(t, &s, chg("id2", "B", "2"))
	mustPut(t, &s, chg("id3", "C", "3"))
	if err := s.Lock([]string{"id1", "id2"}, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Lock([]string{"id3"}, "run-2"); err != nil {
		t.Fatal(err)
	}
	rev := s.Rev
	s.UnlockChanges("run-1", []string{"id1", "id3"}, map[string]string{"id1": "skipped: x", "id3": "no"})
	a, _ := s.Get("id1")
	b, _ := s.Get("id2")
	c, _ := s.Get("id3")
	if a.RunID != "" || a.Flag != "skipped: x" || b.RunID != "run-1" || c.RunID != "run-2" || c.Flag != "" {
		t.Errorf("after UnlockChanges: %+v %+v %+v", a, b, c)
	}
	if s.Rev <= rev {
		t.Errorf("Rev not bumped")
	}
}

// A Variables edit (no Group, Origin or Requires) of a database password
// place keeps its Requires; an automatic rewrite so edited is a decision.
// Value rotation groups and changes carrying their own Group, Origin or Requires
// are unchanged.
func TestPutKeepsPasswordRequires(t *testing.T) {
	var s ChangeSet
	c1 := chg("id1", "A", "1")
	c1.Group, c1.Requires = "rotate-db:e1/db:root", []string{"dbpw"}
	c1.Origin = &Origin{Producer: "rotate", Row: c1.Group, Letter: LetterRewrite}
	mustPut(t, &s, c1)
	mustPut(t, &s, chg("id2", "A", "2"))
	got := s.Changes[0]
	if got.Group != c1.Group || !slices.Equal(got.Requires, []string{"dbpw"}) || got.Origin == nil || got.Origin.Letter != "" || got.Origin.Row != c1.Group {
		t.Errorf("password edit = %+v %+v", got, got.Origin)
	}

	// Value rotation: the Requires of the newer change win (none).
	var s4 ChangeSet
	c4 := chg("id1", "B", "1")
	c4.Group, c4.Requires = "rotate:fp1", []string{"x"}
	c4.Origin = &Origin{Producer: "rotate", Row: "fp1", Letter: "a"}
	mustPut(t, &s4, c4)
	mustPut(t, &s4, chg("id2", "B", "2"))
	if got := s4.Changes[0]; len(got.Requires) != 0 || got.Origin.Letter != "a" {
		t.Errorf("value rotation edit = %+v", got)
	}

	// A producer's own decision (with a Group) keeps its own Requires.
	x := chg("id3", "A", "3")
	x.Group, x.Origin = c1.Group, &Origin{Producer: "rotate", Row: c1.Group, Letter: "x"}
	mustPut(t, &s, x)
	if got := s.Changes[0]; len(got.Requires) != 0 || got.Origin.Letter != "x" {
		t.Errorf("decision = %+v", got)
	}

	// A newer change with Requires of its own keeps them.
	var s5 ChangeSet
	mustPut(t, &s5, c1)
	r := chg("id4", "A", "4")
	r.Requires = []string{"other"}
	mustPut(t, &s5, r)
	if got := s5.Changes[0]; !slices.Equal(got.Requires, []string{"other"}) {
		t.Errorf("own requires = %v", got.Requires)
	}
}

// A quarantined change set holds staged secrets: PruneCorrupt removes the
// copies older than age, and nothing else.
func TestPruneCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "changeset.json")
	now := time.Now()
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("changeset.json.corrupt-20260101-000000", 40*24*time.Hour)
	fresh := write("changeset.json.corrupt-20261020-000000", time.Hour)
	other := write("changeset.json.bak", 40*24*time.Hour)
	if err := os.Symlink(other, filepath.Join(dir, "changeset.json.corrupt-link")); err != nil {
		t.Fatal(err)
	}
	n, err := PruneCorrupt(path, 30*24*time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("PruneCorrupt = %d, %v", n, err)
	}
	for p, want := range map[string]bool{old: false, fresh: true, other: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

package rotate_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/rotate"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

// Place screen fixture values: none may ever reach a frame.
const (
	jwtB    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	jwtC    = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	otherV  = "other-literal-value-0011"
	pasted  = "pasted-value-for-test-0009"
	fileV   = "file-value-for-test-0010"
	shortV  = "short-old-0012"
	planned = "planned-new-value-0013-xyz"
)

var psecrets = []string{jwtV, jwtB, jwtC, otherV, pasted, fileV, shortV, planned}

// The places fixture: JWT_SECRET holds value a in production (3 places)
// and value b in staging (3 places) and dev (changed after the leak).
var pcatalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{
		{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"api", "web", "worker"}},
		{ID: "e2", Name: "staging", CanAccess: true, ServiceIDs: []string{"api", "web", "cron"}},
		{ID: "e3", Name: "dev", CanAccess: true, ServiceIDs: []string{"api"}},
	},
	Services: []railway.Service{{ID: "api", Name: "api"}, {ID: "web", Name: "web"}, {ID: "worker", Name: "worker"}, {ID: "cron", Name: "cron"}},
}}}

func pvars() []railway.VarRecord {
	return []railway.VarRecord{
		rec("e1", "api", "JWT_SECRET", jwtV, "2026-10-01T09:30:00"),
		rec("e1", "web", "JWT_SECRET", jwtV, "2026-10-01T09:30:00"),
		rec("e1", "worker", "JWT_SECRET", jwtV, "2026-10-01T09:30:00"),
		rec("e2", "api", "JWT_SECRET", jwtB, "2026-10-02T11:00:00"),
		rec("e2", "web", "JWT_SECRET", jwtB, "2026-10-02T11:00:00"),
		rec("e2", "cron", "JWT_SECRET", jwtB, "2026-10-02T11:00:00"),
		rec("e3", "api", "JWT_SECRET", jwtB, "2026-10-20T15:10:00"),
		rec("e3", "api", "OTHER_VALUE", otherV, "2026-10-21T00:00:00"),
		rec("e1", "web", "USES_JWT", "${{worker.JWT_SECRET}}", "2026-10-07T10:00:00"),
	}
}

func pctx(w, h int) *tui.Ctx {
	return pctxWith(w, h, pvars())
}

func pctxWith(w, h int, vs []railway.VarRecord) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(pcatalog)
	replaceVars(st, vs)
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views: &view.Views{Store: st, Reg: kinds.NewRegistry()},
		Settings: &settings.Settings{
			Projects: []string{"p1"}, WriteEnvs: []string{"e1", "e2", "e3"},
			Since: "2026-10-20T14:00:00Z",
		},
		Theme: tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Filters: map[string]string{},
		Now:     func() time.Time { return now },
	}
}

// openRow opens the places screen of the row labelled label the way the
// list does: enter on it.
func openRow(t *testing.T, c *tui.Ctx, label string) tui.Screen {
	t.Helper()
	l := to(t, c, open(t, c), label)
	_, ms := press(c, l, enter)
	if len(ms) != 1 {
		t.Fatalf("enter: %#v", ms)
	}
	o, ok := ms[0].(tui.OpenMsg)
	if !ok || o.Name != "rotate.places" {
		t.Fatalf("enter: %#v", ms[0])
	}
	s := rotate.BuildPlaces(o.Args...)
	if s == nil {
		t.Fatalf("BuildPlaces(%v) = nil", o.Args)
	}
	s, _ = settle(c, s, s.Init(c))
	return s
}

// at moves the place screen's cursor to the line of loc.
func at(t *testing.T, c *tui.Ctx, s tui.Screen, loc string) tui.Screen {
	t.Helper()
	for range 2 {
		for range 30 {
			if rotate.PlaceCursor(c, s) == loc {
				return s
			}
			s, _ = press(c, s, down)
		}
		for range 30 {
			s, _ = press(c, s, up)
		}
	}
	t.Fatalf("no line %q reached; cursor on %q", loc, rotate.PlaceCursor(c, s))
	return s
}

var up = tea.KeyMsg{Type: tea.KeyUp}

// root applies the change intents to the change set, as the root does.
func root(t *testing.T, c *tui.Ctx, ms []tea.Msg) {
	t.Helper()
	for _, m := range ms {
		switch x := m.(type) {
		case tui.PutChangeMsg:
			put(t, c, x.Change)
		case tui.RemoveChangeMsg:
			if err := c.CS.Remove(x.ID); err != nil {
				t.Fatal(err)
			}
		case tui.ChangesMsg:
			for _, id := range x.Remove {
				if err := c.CS.Remove(id); err != nil {
					t.Fatal(err)
				}
			}
			for _, ch := range x.Put {
				put(t, c, ch)
			}
		}
	}
}

func removed(ms []tea.Msg) []string {
	var out []string
	for _, m := range ms {
		switch x := m.(type) {
		case tui.RemoveChangeMsg:
			out = append(out, x.ID)
		case tui.ChangesMsg:
			out = append(out, x.Remove...)
		}
	}
	return out
}

func flashText(ms []tea.Msg) string {
	var out []string
	for _, f := range flashes(ms) {
		out = append(out, string(f.Kind)+" "+f.Text)
	}
	return strings.Join(out, " | ")
}

// typed is a masked entry: the value, enter, the value again, enter.
func typed(v string) []tea.KeyMsg {
	return []tea.KeyMsg{r(v), enter, r(v), enter}
}

func locOf(ch changeset.Change) string {
	return domain.LocationID(ch.Place.EnvID, ch.Place.ServiceID, ch.Target)
}

func TestPlacesBuild(t *testing.T) {
	if rotate.BuildPlaces() != nil || rotate.BuildPlaces(42) != nil {
		t.Error("BuildPlaces accepts bad arguments")
	}
	c := pctx(80, 30)
	s := rotate.BuildPlaces("no-such-row")
	s, _ = settle(c, s, s.Init(c))
	if got := frame(c, s); !strings.Contains(got, "this row is gone") {
		t.Errorf("gone row:\n%s", got)
	}
}

func TestPlacesGolden(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := pctx(w, 22)
		s := openRow(t, c, "JWT_SECRET")
		// production · api and web: generated; staging · api: delete;
		// staging · web: pasted; staging · cron: copied from dev · OTHER_VALUE.
		s = at(t, c, s, "e1/api/JWT_SECRET")
		s, _ = press(c, s, space, down, space)
		s, ms := press(c, s, r("g"))
		root(t, c, ms)
		s = at(t, c, s, "e2/api/JWT_SECRET")
		s, ms = press(c, s, r("x"))
		root(t, c, ms)
		s = at(t, c, s, "e2/web/JWT_SECRET")
		s, ms = press(c, s, append([]tea.KeyMsg{r("p")}, typed(pasted)...)...)
		root(t, c, ms)
		cp, err := kinds.CopyVariable(c.Store, c.CS.Changes, pl("e2", "cron"), "JWT_SECRET", pl("e3", "api"), "OTHER_VALUE", now)
		if err != nil {
			t.Fatal(err)
		}
		put(t, c, cp)
		s = at(t, c, s, "e1/worker/JWT_SECRET")
		got := frame(c, s)
		tuitest.NoSecrets(t, got, append(psecrets, generated(c)...)...)
		tuitest.Golden(t, "places_"+itoa(w)+".golden", got)
	}
}

// generated is every value the change set carries.
func generated(c *tui.Ctx) []string {
	var out []string
	for _, ch := range c.CS.Changes {
		if v, ok := kinds.SetValue(ch); ok {
			out = append(out, v)
		}
		if _, v, ok := kinds.PasswordValue(ch); ok {
			out = append(out, v)
		}
	}
	return out
}

func TestPlacesTitleAndNote(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	if s.Title() != "Rotate · JWT_SECRET" {
		t.Errorf("title %q", s.Title())
	}
	if n := s.(tui.Noter).Note(c); n != "7 places · 2 values" {
		t.Errorf("note %q", n)
	}
	got := frame(c, s)
	if !strings.Contains(got, "0 of 7 places change · 7 keep the old value") {
		t.Errorf("coverage missing:\n%s", got)
	}
	// dev changed after the leak but still holds staging's old value
	if strings.Contains(got, "✓ rotated") {
		t.Errorf("fresh line of a leaked group marked rotated:\n%s", got)
	}
	c = pctxWith(80, 30, append(pvars()[:6], rec("e3", "api", "JWT_SECRET", jwtC, "2026-10-20T15:10:00")))
	s = openRow(t, c, "JWT_SECRET")
	if got := frame(c, s); strings.Count(got, "✓ rotated") != 1 {
		t.Errorf("fresh line of a group rotated everywhere not marked:\n%s", got)
	}
}

func TestPlacesGenerate(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, ms := press(c, s, r("g"))
	ch := puts(ms)
	if len(ch) != 1 || locOf(ch[0]) != "e1/api/JWT_SECRET" {
		t.Fatalf("puts %+v", ch)
	}
	v := value(t, ch[0])
	if len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" {
		t.Errorf("not 64 hex (len %d)", len(v))
	}
	g := fp(c, "e1/api/JWT_SECRET")
	if ch[0].Group != rotation.GroupKey(g) || ch[0].Origin == nil || ch[0].Origin.Producer != "rotate" || ch[0].Origin.Letter == "" {
		t.Errorf("group %q origin %+v", ch[0].Group, ch[0].Origin)
	}
	if got := flashText(ms); got != "✓ 1 place: new value" {
		t.Errorf("flash %q", got)
	}
	root(t, c, ms)
	got := frame(c, s)
	tuitest.NoSecrets(t, got, append(psecrets, v)...)
	if !strings.Contains(got, "◆ GEN 64 chars") || !strings.Contains(got, "1 of 7 places change · 6 keep the old value") {
		t.Errorf("frame:\n%s", got)
	}
}

// Every place of a letter gets the same value, across
// actions; undo removes the chosen places' changes only.
func TestPlacesGenerateShares(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, ms := press(c, s, r("g"))
	first := puts(ms)
	// a second g before the root has put the first: same value
	s = at(t, c, s, "e1/web/JWT_SECRET")
	s, ms2 := press(c, s, r("g"))
	if value(t, puts(ms2)[0]) != value(t, first[0]) {
		t.Error("in-flight generate gave another value")
	}
	root(t, c, ms)
	root(t, c, ms2)
	s = at(t, c, s, "e1/worker/JWT_SECRET")
	s, ms = press(c, s, r("g"))
	root(t, c, ms)
	if value(t, puts(ms)[0]) != value(t, first[0]) {
		t.Error("later generate gave the letter another value")
	}
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, ms = press(c, s, r("u"))
	if rm := removed(ms); len(rm) != 1 || rm[0] != first[0].ID {
		t.Errorf("undo removed %v", rm)
	}
	if got := flashText(ms); got != "✓ 1 place: decision undone" {
		t.Errorf("flash %q", got)
	}
	root(t, c, ms)
	// a later g on the undone place still gets the letter's value
	_, ms = press(c, s, r("g"))
	if value(t, puts(ms)[0]) != value(t, first[0]) {
		t.Error("value of the letter not reused after undo of one place")
	}
}

// Undo of every change of a group forgets the planned value.
func TestPlacesUndoForgets(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, ms := press(c, s, r("g"))
	root(t, c, ms)
	old := value(t, puts(ms)[0])
	s, ms = press(c, s, r("u"))
	root(t, c, ms)
	_, ms = press(c, s, r("g"))
	if value(t, puts(ms)[0]) == old {
		t.Error("undone value reused")
	}
}

func TestPlacesSelect(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, _ = press(c, s, space, down, space, down)
	if got := frame(c, s); strings.Count(got, "*") != 2 {
		t.Errorf("selection marks:\n%s", got)
	}
	s, ms := press(c, s, r("g"))
	ch := puts(ms)
	if len(ch) != 2 || locOf(ch[0]) != "e1/api/JWT_SECRET" || locOf(ch[1]) != "e1/web/JWT_SECRET" {
		t.Fatalf("puts %+v", ch)
	}
	if got := flashText(ms); got != "✓ 2 places: new value" {
		t.Errorf("flash %q", got)
	}
	root(t, c, ms)
	if got := frame(c, s); strings.Contains(got, "*") {
		t.Errorf("selection not cleared:\n%s", got)
	}
	// space twice deselects
	s, _ = press(c, s, space, space)
	if got := frame(c, s); strings.Contains(got, "*") {
		t.Errorf("toggle:\n%s", got)
	}
}

func TestPlacesLetterSelect(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	for _, k := range []string{"A", "B"} {
		_, ms := press(c, s, r(k), r("g"))
		ch := puts(ms)
		want := strings.ToLower(k)
		if len(ch) != 3 && len(ch) != 4 {
			t.Fatalf("%s: puts %d", k, len(ch))
		}
		for _, x := range ch {
			if x.Origin.Letter != want || x.Group != ch[0].Group {
				t.Errorf("%s: %s letter %q group %q", k, locOf(x), x.Origin.Letter, x.Group)
			}
		}
	}
	// a letter the row does not have selects nothing
	_, ms := press(c, s, r("Z"))
	if len(puts(ms)) != 0 {
		t.Error("Z put changes")
	}
}

func TestPlacesSealedShareOneValue(t *testing.T) {
	c := pctxWith(80, 30, append(pvars(),
		railway.VarRecord{EnvID: "e1", ServiceID: "api", Name: "COOKIE_PASSWORD", Sealed: true},
		railway.VarRecord{EnvID: "e1", ServiceID: "web", Name: "COOKIE_PASSWORD", Sealed: true},
		railway.VarRecord{EnvID: "e2", ServiceID: "api", Name: "COOKIE_PASSWORD", Sealed: true},
	))
	s := openRow(t, c, "COOKIE_PASSWORD")
	s = at(t, c, s, "e1/api/COOKIE_PASSWORD")
	s, ms := press(c, s, r("g"))
	root(t, c, ms)
	a := puts(ms)
	s = at(t, c, s, "e2/api/COOKIE_PASSWORD")
	s, _ = press(c, s, space)
	s = at(t, c, s, "e1/web/COOKIE_PASSWORD")
	_, ms = press(c, s, space, r("g"))
	b := puts(ms)
	if len(a) != 1 || len(b) != 2 {
		t.Fatalf("puts %d %d", len(a), len(b))
	}
	for _, x := range b {
		if value(t, x) != value(t, a[0]) {
			t.Errorf("%s got another value", locOf(x))
		}
		if x.Origin.Letter != "" {
			t.Errorf("sealed letter %q", x.Origin.Letter)
		}
	}
	if len(value(t, a[0])) != 24 {
		t.Errorf("sealed value length %d", len(value(t, a[0])))
	}
}

func TestPlacesPaste(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, _ = press(c, s, space, down, space, r("p"), r(pasted))
	got := frame(c, s)
	tuitest.NoSecrets(t, got, psecrets...)
	if !strings.Contains(got, "JWT_SECRET · 2 places: ") {
		t.Errorf("masked input:\n%s", got)
	}
	s, ms := press(c, s, enter, r(pasted), enter)
	ch := puts(ms)
	if len(ch) != 2 || value(t, ch[0]) != pasted || value(t, ch[1]) != pasted {
		t.Fatalf("puts %+v", ch)
	}
	if ch[0].Group != rotation.GroupKey(fp(c, "e1/api/JWT_SECRET")) || ch[0].Origin.Producer != "rotate" {
		t.Errorf("group %q origin %+v", ch[0].Group, ch[0].Origin)
	}
	if got := flashText(ms); got != "✓ 2 places: new value" {
		t.Errorf("flash %q", got)
	}
	root(t, c, ms)
	tuitest.NoSecrets(t, frame(c, s), psecrets...)

	// esc cancels, nothing put
	_, ms = press(c, s, r("p"), r("x"), tea.KeyMsg{Type: tea.KeyEsc})
	if len(puts(ms)) != 0 {
		t.Error("esc put a change")
	}
	// an empty value is refused
	_, ms = press(c, s, r("p"), enter, enter)
	if len(puts(ms)) != 0 || flashText(ms) != "✗ the value is empty" {
		t.Errorf("empty: %q", flashText(ms))
	}
}

// A value equal to an old value of the row, of any letter,
// is refused; a group rotated everywhere after the leak is exempt.
func TestPlacesRefusesLeaked(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	for _, v := range []string{jwtV, jwtB} {
		var ms []tea.Msg
		s, ms = press(c, s, append([]tea.KeyMsg{r("p")}, typed(v)...)...)
		if len(puts(ms)) != 0 || flashText(ms) != "✗ that is the current, leaked value" {
			t.Errorf("leaked value accepted: %q", flashText(ms))
		}
	}

	// short values: the fingerprint holds the name, so each name is checked
	c = pctxWith(80, 30, append(pvars(),
		rec("e1", "api", "API_SECRET", shortV, "2026-10-01T09:30:00"),
		rec("e1", "web", "API_SECRET", shortV, "2026-10-01T09:30:00"),
	))
	s = openRow(t, c, "API_SECRET")
	_, ms := press(c, s, append([]tea.KeyMsg{r("p")}, typed(shortV)...)...)
	if len(puts(ms)) != 0 || flashText(ms) != "✗ that is the current, leaked value" {
		t.Errorf("short leaked value accepted: %q", flashText(ms))
	}

	// dev's own value c is fresh everywhere: exempt
	c = pctxWith(80, 30, append(pvars()[:6], rec("e3", "api", "JWT_SECRET", jwtC, "2026-10-20T15:10:00")))
	s = openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	_, ms = press(c, s, append([]tea.KeyMsg{r("p")}, typed(jwtC)...)...)
	if len(puts(ms)) != 1 {
		t.Errorf("value of a group rotated everywhere refused: %q", flashText(ms))
	}
}

func TestPlacesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("val.txt", "  "+fileV+"\n")
	write("empty.txt", " \n")
	write("big.txt", strings.Repeat("x", 64*1024+1))
	write("leak.txt", jwtB+"\n")

	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, _ = press(c, s, r("f"), r("~/val"))
	got := frame(c, s)
	if !strings.Contains(got, "val.txt") {
		t.Errorf("picker:\n%s", got)
	}
	tuitest.NoSecrets(t, got, psecrets...)
	s, ms := press(c, s, enter)
	ch := puts(ms)
	if len(ch) != 1 || value(t, ch[0]) != fileV {
		t.Fatalf("file: %q %+v", flashText(ms), ch)
	}
	root(t, c, ms)
	tuitest.NoSecrets(t, frame(c, s), psecrets...)

	for name, want := range map[string]string{
		"empty.txt": "✗ the file is empty",
		"big.txt":   "✗ file too large",
		"leak.txt":  "✗ that is the current, leaked value",
	} {
		var ms []tea.Msg
		s, ms = press(c, s, r("f"), r("~/"+name), enter)
		if len(puts(ms)) != 0 || flashText(ms) != want {
			t.Errorf("%s: %q", name, flashText(ms))
		}
	}
	// esc closes the picker
	s, ms = press(c, s, r("f"), tea.KeyMsg{Type: tea.KeyEsc})
	if len(ms) != 0 || strings.Contains(frame(c, s), "file:") {
		t.Errorf("esc: %#v", ms)
	}
}

func TestPlacesFileUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads anything")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "locked.txt")
	if err := os.WriteFile(p, []byte(fileV), 0o000); err != nil {
		t.Fatal(err)
	}
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	_, ms := press(c, s, r("f"), r("~/locked"), enter)
	if got := flashText(ms); len(puts(ms)) != 0 || got != "✗ cannot read the file: permission denied" {
		t.Errorf("unreadable: %q", got)
	}
}

func TestPlacesCopy(t *testing.T) {
	c := pctxWith(80, 30, append(pvars()[:6], rec("e3", "api", "JWT_SECRET", jwtC, "2026-10-20T15:10:00")))
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, _ = press(c, s, space, down, space, r("c"))
	got := frame(c, s)
	if !strings.Contains(got, "copy to 2 places") {
		t.Errorf("choose mode:\n%s", got)
	}
	s = at(t, c, s, "e3/api/JWT_SECRET")
	s, ms := press(c, s, enter)
	ch := puts(ms)
	if len(ch) != 2 || value(t, ch[0]) != jwtC {
		t.Fatalf("copy: %q %+v", flashText(ms), ch)
	}
	if src, ok := kinds.CopySource(ch[0]); !ok || src != "e3/api/JWT_SECRET" {
		t.Errorf("copy source %q", src)
	}
	if ch[0].Group != rotation.GroupKey(fp(c, "e1/api/JWT_SECRET")) || ch[0].Origin.Producer != "rotate" {
		t.Errorf("group %q origin %+v", ch[0].Group, ch[0].Origin)
	}
	if got := flashText(ms); got != "✓ 2 places: copied from dev · api" {
		t.Errorf("flash %q", got)
	}
	root(t, c, ms)
	got = frame(c, s)
	tuitest.NoSecrets(t, got, append(psecrets, jwtC)...)
	// the source is listed: a bracket joins the copies to it
	if strings.Count(got, "⇉ COPY") != 2 || lineOf(got, "dev         api") == "" || !strings.Contains(lineOf(got, "dev         api"), "┐") {
		t.Errorf("copy decision:\n%s", got)
	}
}

// Two sources whose brackets overlap get a lane each, so they never cross.
func TestPlacesCopyLanes(t *testing.T) {
	c := pctxWith(80, 30, append(pvars()[:6], rec("e3", "api", "JWT_SECRET", jwtC, "2026-10-20T15:10:00")))
	s := openRow(t, c, "JWT_SECRET")
	// production api and web copy dev · api
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, _ = press(c, s, space, down, space, r("c"))
	s = at(t, c, s, "e3/api/JWT_SECRET")
	s, ms := press(c, s, enter)
	root(t, c, ms)
	// staging · cron copies staging · api's generated value
	s = at(t, c, s, "e2/api/JWT_SECRET")
	s, ms = press(c, s, r("g"))
	root(t, c, ms)
	s = at(t, c, s, "e2/cron/JWT_SECRET")
	s, _ = press(c, s, r("c"))
	s = at(t, c, s, "e2/api/JWT_SECRET")
	s, ms = press(c, s, enter)
	root(t, c, ms)
	got := frame(c, s)
	for _, w := range []struct{ line, want string }{
		{"dev         api", "┐"},
		{"staging     api", "│ ┐"},
		{"staging     cron", "│ ┘"},
		{"staging     web", "│"},
		{"production  web", "┘"},
	} {
		if l := lineOf(got, w.line); !strings.HasSuffix(strings.TrimRight(strings.TrimSuffix(strings.TrimRight(l, " "), "┃"), " "), w.want) {
			t.Errorf("%s ends %q, want %q:\n%s", w.line, l, w.want, got)
		}
	}
	if !strings.Contains(lineOf(got, "staging     api"), "◆ GEN 64 chars") {
		t.Errorf("generated source not labelled:\n%s", got)
	}
}

// A place whose value is only planned can be copied from: the copies take
// that value, link to it, and undoing the source asks about them first.
func TestPlacesCopyPlanned(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, ms := press(c, s, r("g"))
	root(t, c, ms)
	gen := value(t, puts(ms)[0])
	srcID := puts(ms)[0].ID

	s = at(t, c, s, "e2/api/JWT_SECRET")
	s, _ = press(c, s, space, down, space, r("c"))
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, ms = press(c, s, enter)
	ch := puts(ms)
	if len(ch) != 2 || value(t, ch[0]) != gen || value(t, ch[1]) != gen {
		t.Fatalf("copy: %q %d changes", flashText(ms), len(ch))
	}
	if got := flashText(ms); got != "✓ 2 places: copied from production · api · follows its new value" {
		t.Errorf("flash %q", got)
	}
	root(t, c, ms)
	if got := frame(c, s); strings.Count(got, "⇉ COPY") != 2 || !strings.Contains(lineOf(got, "◆ GEN"), "┘") {
		t.Errorf("copy decision:\n%s", got)
	}

	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, ms = press(c, s, r("u"))
	if len(ms) != 0 || !strings.Contains(frame(c, s), "undo? 2 copies of it go too: staging · api, staging · ") {
		t.Fatalf("no question: %q\n%s", flashText(ms), frame(c, s))
	}
	_, ms = press(c, s, r("y"))
	if rm := removed(ms); len(rm) != 1 || rm[0] != srcID {
		t.Errorf("removed %v, want the source (the root takes its copies)", rm)
	}
	tuitest.NoSecrets(t, frame(c, s), append(psecrets, gen)...)
}

func TestPlacesCopyRefusals(t *testing.T) {
	refuse := func(t *testing.T, c *tui.Ctx, label, target, source, want string) {
		t.Helper()
		s := openRow(t, c, label)
		s = at(t, c, s, target)
		s, _ = press(c, s, r("c"))
		s = at(t, c, s, source)
		_, ms := press(c, s, enter)
		if len(puts(ms)) != 0 || flashText(ms) != want {
			t.Errorf("%s: %q", source, flashText(ms))
		}
	}
	t.Run("target", func(t *testing.T) {
		refuse(t, pctx(80, 30), "JWT_SECRET", "e1/api/JWT_SECRET", "e1/api/JWT_SECRET", "✗ copy source is one of the chosen places")
	})
	t.Run("not changed after the leak", func(t *testing.T) {
		refuse(t, pctx(80, 30), "JWT_SECRET", "e1/api/JWT_SECRET", "e2/api/JWT_SECRET", "✗ likely holds the leaked value")
	})
	t.Run("leaked", func(t *testing.T) {
		// dev is changed after the leak but still holds staging's old value
		refuse(t, pctx(80, 30), "JWT_SECRET", "e1/api/JWT_SECRET", "e3/api/JWT_SECRET", "✗ that is the current, leaked value")
	})
	t.Run("no leak time", func(t *testing.T) {
		// refused at c, before choosing a source
		c := pctx(80, 30)
		c.Settings.Since = ""
		s := openRow(t, c, "JWT_SECRET")
		s = at(t, c, s, "e1/api/JWT_SECRET")
		s, ms := press(c, s, r("c"))
		if got := flashText(ms); got != "! copy needs a leak time · Settings l" {
			t.Errorf("flash %q", got)
		}
		if strings.Contains(frame(c, s), "copy to") {
			t.Error("choose mode entered without a leak time")
		}
	})
	t.Run("leak time cleared while choosing", func(t *testing.T) {
		c := pctx(80, 30)
		s := openRow(t, c, "JWT_SECRET")
		s = at(t, c, s, "e1/api/JWT_SECRET")
		s, _ = press(c, s, r("c"))
		c.Settings.Since = ""
		s = at(t, c, s, "e2/api/JWT_SECRET")
		_, ms := press(c, s, enter)
		if len(puts(ms)) != 0 || flashText(ms) != "! copy needs a leak time · Settings l" {
			t.Errorf("flash %q", flashText(ms))
		}
	})
	t.Run("sealed", func(t *testing.T) {
		c := pctxWith(80, 30, append(pvars(),
			railway.VarRecord{EnvID: "e1", ServiceID: "api", Name: "COOKIE_PASSWORD", Sealed: true, UpdatedAt: "2026-10-22T00:00:00"},
			railway.VarRecord{EnvID: "e1", ServiceID: "web", Name: "COOKIE_PASSWORD", Sealed: true},
		))
		refuse(t, c, "COOKIE_PASSWORD", "e1/web/COOKIE_PASSWORD", "e1/api/COOKIE_PASSWORD", "✗ copy source is sealed")
	})
	t.Run("esc", func(t *testing.T) {
		c := pctx(80, 30)
		s := openRow(t, c, "JWT_SECRET")
		s, _ = press(c, s, r("c"), tea.KeyMsg{Type: tea.KeyEsc})
		if strings.Contains(frame(c, s), "copy to") {
			t.Error("esc left choose mode on")
		}
	})
}

func TestPlacesDelete(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, ms := press(c, s, r("x"))
	ch := puts(ms)
	if len(ch) != 1 || ch[0].Kind != "variable.delete" || locOf(ch[0]) != "e1/api/JWT_SECRET" {
		t.Fatalf("delete %+v", ch)
	}
	if ch[0].Group != rotation.GroupKey(fp(c, "e1/api/JWT_SECRET")) || ch[0].Origin == nil || ch[0].Origin.Producer != "rotate" {
		t.Errorf("group %q origin %+v", ch[0].Group, ch[0].Origin)
	}
	if got := flashText(ms); got != "✓ 1 place: delete" {
		t.Errorf("flash %q", got)
	}
	root(t, c, ms)
	if !strings.Contains(frame(c, s), "✗ delete") {
		t.Errorf("frame:\n%s", frame(c, s))
	}
	// the worker's variable is referenced by USES_JWT
	s = at(t, c, s, "e1/worker/JWT_SECRET")
	_, ms = press(c, s, r("x"))
	if len(puts(ms)) != 0 || flashText(ms) != "✗ referenced by 1 variables: delete or change them first" {
		t.Errorf("referenced: %q", flashText(ms))
	}
}

func TestPlacesReadOnlyRefused(t *testing.T) {
	c := pctx(80, 30)
	c.Settings.WriteEnvs = []string{"e1"}
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e2/api/JWT_SECRET")
	for _, k := range []string{"g", "p", "f", "c", "x"} {
		var ms []tea.Msg
		s, ms = press(c, s, r(k))
		if len(puts(ms)) != 0 || flashText(ms) != "✗ shop/staging is not in write environments · Settings w" {
			t.Errorf("%s: %q", k, flashText(ms))
		}
		if s.(tui.Capturer).Capturing() {
			t.Errorf("%s opened an input", k)
		}
	}
	if got := frame(c, s); !strings.Contains(got, "read only") {
		t.Errorf("frame:\n%s", got)
	}
}

func TestPlacesUndoAnyOrigin(t *testing.T) {
	c := pctx(80, 30)
	// a Variables edit (no rotate origin) on production · web
	ed, err := kinds.SetVariable(c.Store, nil, pl("e1", "web"), "JWT_SECRET", planned, now)
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, ed)
	keep := rotateSet(t, c, pl("e1", "api"), "JWT_SECRET", planned, fp(c, "e1/api/JWT_SECRET"), "a")
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/web/JWT_SECRET")
	_, ms := press(c, s, r("u"))
	if rm := removed(ms); len(rm) != 1 || rm[0] != ed.ID || rm[0] == keep.ID {
		t.Errorf("undo removed %v", rm)
	}
	s = at(t, c, s, "e1/worker/JWT_SECRET")
	_, ms = press(c, s, r("u"))
	if flashText(ms) != "! nothing to undo here" {
		t.Errorf("flash %q", flashText(ms))
	}
}

// A refresh keeps the selected lines that still exist.
func TestPlacesRefreshKeepsSelection(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	s, _ = press(c, s, space, down, space)
	// production · web's JWT_SECRET is gone after a refresh
	vs := pvars()
	replaceVars(c.Store, append(vs[:1:1], vs[2:]...))
	s, _ = press(c, s, down) // any message
	_, ms := press(c, s, r("g"))
	ch := puts(ms)
	if len(ch) != 1 || locOf(ch[0]) != "e1/api/JWT_SECRET" {
		t.Errorf("puts %+v", ch)
	}
}

// A decision is re-resolved on submit, never written to a
// vanished place.
func TestPlacesSubmitRevalidates(t *testing.T) {
	vs := pvars()
	gone := append(vs[:1:1], vs[2:]...)
	t.Run("paste", func(t *testing.T) {
		c := pctx(80, 30)
		s := openRow(t, c, "JWT_SECRET")
		s = at(t, c, s, "e1/api/JWT_SECRET")
		s, _ = press(c, s, space, down, space, r("p"), r(pasted), enter, r(pasted))
		replaceVars(c.Store, gone)
		_, ms := press(c, s, enter)
		if len(puts(ms)) != 0 || flashText(ms) != "! the places changed since p · p again" {
			t.Errorf("%q", flashText(ms))
		}
	})
	t.Run("file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.WriteFile(filepath.Join(home, "val.txt"), []byte(fileV), 0o600); err != nil {
			t.Fatal(err)
		}
		c := pctx(80, 30)
		s := openRow(t, c, "JWT_SECRET")
		s = at(t, c, s, "e1/web/JWT_SECRET")
		s, _ = press(c, s, r("f"), r("~/val"))
		replaceVars(c.Store, gone)
		_, ms := press(c, s, enter)
		if len(puts(ms)) != 0 || flashText(ms) != "! the places changed since f · f again" {
			t.Errorf("%q", flashText(ms))
		}
	})
	t.Run("copy", func(t *testing.T) {
		c := pctxWith(80, 30, append(pvars()[:6], rec("e3", "api", "JWT_SECRET", jwtC, "2026-10-20T15:10:00")))
		s := openRow(t, c, "JWT_SECRET")
		s = at(t, c, s, "e1/web/JWT_SECRET")
		s, _ = press(c, s, r("c"))
		s = at(t, c, s, "e3/api/JWT_SECRET")
		vs := append(pvars()[:6], rec("e3", "api", "JWT_SECRET", jwtC, "2026-10-20T15:10:00"))
		replaceVars(c.Store, append(vs[:1:1], vs[2:]...))
		_, ms := press(c, s, enter)
		if len(puts(ms)) != 0 || flashText(ms) != "! the places changed since c · c again" {
			t.Errorf("%q", flashText(ms))
		}
	})
	t.Run("confirm", func(t *testing.T) {
		c := pctxWith(80, 30, append(pvars(),
			rec("e1", "api", "ENCRYPTION_SECRET_KEY", encV, "2026-10-03T10:00:00"),
			rec("e1", "web", "ENCRYPTION_SECRET_KEY", encV, "2026-10-03T10:00:00"),
		))
		s := openRow(t, c, "ENCRYPTION_SECRET_KEY")
		s = at(t, c, s, "e1/web/ENCRYPTION_SECRET_KEY")
		s, ms := press(c, s, r("g"))
		if len(puts(ms)) != 0 || !s.(tui.Capturer).Capturing() {
			t.Fatalf("off by default did not ask: %#v", ms)
		}
		replaceVars(c.Store, pvars())
		_, ms = press(c, s, r("y"))
		if len(puts(ms)) != 0 || flashText(ms) != "! the places changed since g · g again" {
			t.Errorf("%q", flashText(ms))
		}
	})
}

func TestPlacesOffByDefaultAsks(t *testing.T) {
	c := pctxWith(80, 30, append(pvars(),
		rec("e1", "api", "ENCRYPTION_SECRET_KEY", encV, "2026-10-03T10:00:00"),
	))
	s := openRow(t, c, "ENCRYPTION_SECRET_KEY")
	s, _ = press(c, s, r("g"))
	if got := frame(c, s); !strings.Contains(got, "(y/n)") {
		t.Errorf("no question:\n%s", got)
	}
	_, ms := press(c, s, r("y"))
	if len(puts(ms)) != 1 {
		t.Errorf("yes: %q", flashText(ms))
	}
	s, _ = press(c, s, r("g"), r("n"))
	if s.(tui.Capturer).Capturing() {
		t.Error("n left the question up")
	}
	// x is a decision too
	s, ms = press(c, s, r("x"))
	if len(puts(ms)) != 0 || !s.(tui.Capturer).Capturing() {
		t.Fatalf("x did not ask: %#v", ms)
	}
	_, ms = press(c, s, r("y"))
	if ch := puts(ms); len(ch) != 1 || ch[0].Kind != "variable.delete" {
		t.Errorf("x yes: %q", flashText(ms))
	}
}

func TestPlacesClaimsLetters(t *testing.T) {
	c := pctxWith(80, 30, append(pvars()[:6], rec("e3", "api", "JWT_SECRET", jwtC, "2026-10-20T15:10:00")))
	s := openRow(t, c, "JWT_SECRET")
	k := s.(tui.KeyClaimer)
	if !k.Claims("C") || !k.Claims("A") || k.Claims("g") || k.Claims("ctrl+c") || k.Claims("esc") {
		t.Error("claims")
	}
	_, ms := press(c, s, r("C"), r("g"))
	ch := puts(ms)
	if len(ch) == 0 {
		t.Fatalf("C g: %q", flashText(ms))
	}
	for _, x := range ch {
		if x.Origin.Letter != "c" {
			t.Errorf("%s letter %q", locOf(x), x.Origin.Letter)
		}
	}
}

// A refresh that adds a value sorting first changes the row's key: the
// screen follows its row through the group keys.
func TestPlacesFollowsRowKey(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	first := fp(c, "e1/api/JWT_SECRET")
	if b := fp(c, "e2/api/JWT_SECRET"); b < first {
		first = b
	}
	var v string
	for i := 0; ; i++ {
		v = "rotated-dev-value-padding-" + strconv.Itoa(i)
		if domain.Fingerprint("JWT_SECRET", v, false, true, "e3", "api") < first {
			break
		}
	}
	vs := pvars()
	vs[6] = rec("e3", "api", "JWT_SECRET", v, "2026-10-20T15:10:00")
	replaceVars(c.Store, vs)
	got := frame(c, s)
	if strings.Contains(got, "this row is gone") || s.(tui.Noter).Note(c) != "7 places · 3 values" {
		t.Errorf("row lost after its key changed:\n%s", got)
	}
	tuitest.NoSecrets(t, got, append(psecrets, v)...)
}

// One value per added variable: g on an added row reuses the add's value.
func TestPlacesAddedRowShares(t *testing.T) {
	c := pctx(80, 30)
	s := addTo(t, c, "NEW_SECRET", 1, 3)
	_, ms := press(c, s, r("g"))
	root(t, c, ms)
	v := value(t, puts(ms)[0])
	p := openRow(t, c, "NEW_SECRET")
	p = at(t, c, p, "e1/api/NEW_SECRET")
	_, ms = press(c, p, r("g"))
	ch := puts(ms)
	if len(ch) != 1 || value(t, ch[0]) != v || ch[0].Group != rotation.AddGroupKey("NEW_SECRET") {
		t.Errorf("added row g: %q %+v", flashText(ms), ch)
	}
}

func TestPlacesReveal(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	s = at(t, c, s, "e1/api/JWT_SECRET")
	before := frame(c, s)
	s, _ = press(c, s, r("v"))
	got := frame(c, s)
	if !strings.Contains(got, jwtV[:20]) || !strings.Contains(got, "╭─ JWT_SECRET · production · api") {
		t.Errorf("not revealed in a panel:\n%s", got)
	}
	// the panel floats: the cursor line and the lines above it do not move
	bl, gl := strings.Split(before, "\n"), strings.Split(got, "\n")
	if len(bl) != len(gl) {
		t.Fatalf("frame height %d, was %d", len(gl), len(bl))
	}
	for i := range 4 {
		if bl[i] != gl[i] {
			t.Errorf("line %d moved:\n%s\nwas\n%s", i, gl[i], bl[i])
		}
	}
	s, _ = press(c, s, down)
	tuitest.NoSecrets(t, frame(c, s), psecrets...)

	// a refresh hides it, and it stays hidden when the data comes back
	s, _ = press(c, s, tea.KeyMsg{Type: tea.KeyUp}, r("v"))
	if !strings.Contains(frame(c, s), jwtV[:20]) {
		t.Fatal("not revealed again")
	}
	c.Store.ReplaceVariables("p1", pvars())
	tuitest.NoSecrets(t, frame(c, s), psecrets...)
	// so does another screen covering this one
	s, _ = press(c, s, r("v"))
	s.(interface{ Suspend(*tui.Ctx) }).Suspend(c)
	tuitest.NoSecrets(t, frame(c, s), psecrets...)

	c.IsTTY = false
	_, ms := press(c, s, r("v"))
	if flashText(ms) != "! reveal needs a terminal" {
		t.Errorf("%q", flashText(ms))
	}
}

// A line changed after the leak whose value is older elsewhere is marked
// still leaked, and the note under the list says why.
func TestPlacesStillLeaked(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	got := frame(c, s)
	dev := lineOf(got, "10-20 15:10")
	if !strings.Contains(dev, "↻ still leaked") {
		t.Errorf("dev · api not marked: %q", dev)
	}
	if strings.Contains(lineOf(got, "10-02 11:00"), "still leaked") {
		t.Error("a line unchanged since the leak is marked still leaked")
	}
	if strings.Contains(got, "changed after the leak") {
		t.Error("the note shows with the cursor elsewhere")
	}
	s = at(t, c, s, "e3/api/JWT_SECRET")
	got = frame(c, s)
	for _, want := range []string{
		"↻ dev · api changed after the leak, but it is still the leaked value",
		"  same value a at staging · ",
		"unchanged since 10-02 11:00 · rotate all a",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("note misses %q:\n%s", want, got)
		}
	}
}

// lineOf is the first line of frame holding sub; "" when none.
func lineOf(frame, sub string) string {
	for _, l := range strings.Split(frame, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

// No pasted, file, copied or generated value reaches a frame, in any mode.
func TestPlacesNoSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "val.txt"), []byte(fileV), 0o600); err != nil {
		t.Fatal(err)
	}
	c := pctxWith(80, 30, append(pvars()[:6], rec("e3", "api", "JWT_SECRET", jwtC, "2026-10-20T15:10:00")))
	s := openRow(t, c, "JWT_SECRET")
	check := func(when string) {
		t.Helper()
		got := frame(c, s)
		for i, v := range append(append([]string{}, psecrets...), generated(c)...) {
			if strings.Contains(got, v) {
				t.Errorf("%s: frame holds value #%d", when, i)
			}
		}
	}
	steps := []struct {
		name string
		loc  string
		keys []tea.KeyMsg
	}{
		{"generate", "e1/api/JWT_SECRET", []tea.KeyMsg{r("g")}},
		{"paste", "e1/web/JWT_SECRET", append([]tea.KeyMsg{r("p")}, typed(pasted)...)},
		{"file", "e1/worker/JWT_SECRET", []tea.KeyMsg{r("f"), r("~/val"), enter}},
		{"copy", "e2/api/JWT_SECRET", []tea.KeyMsg{r("c")}},
	}
	for _, st := range steps {
		s = at(t, c, s, st.loc)
		for _, k := range st.keys {
			var ms []tea.Msg
			s, ms = press(c, s, k)
			root(t, c, ms)
			check(st.name)
		}
	}
	s = at(t, c, s, "e3/api/JWT_SECRET")
	s, ms := press(c, s, enter)
	root(t, c, ms)
	check("copied")
	if len(c.CS.Changes) < 4 {
		t.Errorf("changes %d, want at least 4", len(c.CS.Changes))
	}
}

func TestPlacesKeys(t *testing.T) {
	c := pctx(80, 30)
	s := openRow(t, c, "JWT_SECRET")
	var got []string
	for _, h := range s.Keys() {
		got = append(got, h.Key)
	}
	want := "g p f c x u space A–Z v ↑/↓ esc"
	if strings.Join(got, " ") != want {
		t.Errorf("keys %q, want %q", strings.Join(got, " "), want)
	}
}

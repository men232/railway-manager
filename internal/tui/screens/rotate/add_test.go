package rotate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

var esc = tea.KeyMsg{Type: tea.KeyEsc}

// addTo starts an add on the list, names it and picks the places at the
// given indexes of the place list (dev · shared is 0).
func addTo(t *testing.T, c *tui.Ctx, name string, picks ...int) tui.Screen {
	t.Helper()
	s, ms := press(c, open(t, c), r("a"), r(name), enter)
	if len(ms) != 0 {
		t.Fatalf("name: %q", flashText(ms))
	}
	at := 0
	for _, p := range picks {
		for ; at < p; at++ {
			s, _ = press(c, s, down)
		}
		s, _ = press(c, s, space)
	}
	s, ms = press(c, s, enter)
	if len(ms) != 0 {
		t.Fatalf("places: %q", flashText(ms))
	}
	return s
}

func TestAddGolden(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := pctx(w, 24)
		s, _ := press(c, open(t, c), r("a"), r("NEW_SECRET"), enter, down, space, down, down, space)
		got := frame(c, s)
		tuitest.NoSecrets(t, got, psecrets...)
		tuitest.Golden(t, "add_"+itoa(w)+".golden", got)
	}
}

func TestAddFlow(t *testing.T) {
	c := pctx(80, 30)
	s, _ := press(c, open(t, c), r("a"))
	if !s.(tui.Capturer).Capturing() || !strings.Contains(frame(c, s), "name: ") {
		t.Fatalf("no name input:\n%s", frame(c, s))
	}
	s, _ = press(c, s, r("NEW_SECRET"), enter)
	// no place chosen yet
	s, ms := press(c, s, enter)
	if flashText(ms) != "! choose at least one place · space selects" {
		t.Errorf("empty pick: %q", flashText(ms))
	}
	s, _ = press(c, s, down, space, down, down, space, enter)
	got := frame(c, s)
	for _, k := range []string{"g generate", "p paste", "f file", "r reference"} {
		if !strings.Contains(got, k) {
			t.Errorf("value step lacks %q:\n%s", k, got)
		}
	}
	_, ms = press(c, s, r("g"))
	ch := puts(ms)
	if len(ch) != 2 {
		t.Fatalf("puts %q %+v", flashText(ms), ch)
	}
	if locOf(ch[0]) != "e3/api/NEW_SECRET" || locOf(ch[1]) != "e1/api/NEW_SECRET" {
		t.Errorf("places %s %s", locOf(ch[0]), locOf(ch[1]))
	}
	v := value(t, ch[0])
	if len(v) != 24 || strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" || value(t, ch[1]) != v {
		t.Errorf("generated value not 24 letters and digits shared (len %d)", len(v))
	}
	for _, x := range ch {
		want := changeset.Origin{Producer: "rotate", Row: rotation.AddGroupKey("NEW_SECRET")}
		if x.Group != rotation.AddGroupKey("NEW_SECRET") || x.Origin == nil || *x.Origin != want {
			t.Errorf("group %q origin %+v", x.Group, x.Origin)
		}
	}
	if got := flashText(ms); got != "✓ 2 places: NEW_SECRET added" {
		t.Errorf("flash %q", got)
	}
	root(t, c, ms)
	got = frame(c, open(t, c))
	tuitest.NoSecrets(t, got, append(psecrets, v)...)
	if !strings.Contains(got, "added (1)") || !strings.Contains(got, "NEW_SECRET") {
		t.Errorf("added row missing:\n%s", got)
	}
}

func TestAddPasteAndFile(t *testing.T) {
	c := pctx(80, 30)
	s := addTo(t, c, "NEW_SECRET", 1)
	s, ms := press(c, s, r("p"), r(pasted))
	tuitest.NoSecrets(t, frame(c, s), psecrets...)
	s, ms = press(c, s, enter, r(pasted), enter)
	if ch := puts(ms); len(ch) != 1 || value(t, ch[0]) != pasted {
		t.Fatalf("paste: %q", flashText(ms))
	}
	tuitest.NoSecrets(t, frame(c, s), psecrets...)

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "val.txt"), []byte(fileV+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c = pctx(80, 30)
	s = addTo(t, c, "NEW_SECRET", 1)
	s, _ = press(c, s, r("f"), r("~/val"))
	tuitest.NoSecrets(t, frame(c, s), psecrets...)
	s, ms = press(c, s, enter)
	if ch := puts(ms); len(ch) != 1 || value(t, ch[0]) != fileV {
		t.Fatalf("file: %q", flashText(ms))
	}
	tuitest.NoSecrets(t, frame(c, s), psecrets...)
}

func TestAddRefusesExisting(t *testing.T) {
	c := pctx(80, 30)
	c.Settings.WriteEnvs = []string{"e1"}
	for name, want := range map[string]string{
		"JWT_SECRET":  "! JWT_SECRET exists · rotate it instead",
		"OTHER_VALUE": "! OTHER_VALUE exists · rotate it instead", // only in dev, not writable
		"RAILWAY_X":   "✗ RAILWAY_X is managed by Railway",
		"1BAD":        `✗ invalid variable name "1BAD": use letters, digits and _, not starting with a digit`,
	} {
		s, ms := press(c, open(t, c), r("a"), r(name), enter)
		if flashText(ms) != want {
			t.Errorf("%s: %q", name, flashText(ms))
		}
		if !s.(tui.Capturer).Capturing() {
			t.Errorf("%s: the name input closed", name)
		}
	}
	// a pending add of the name counts too
	s := addTo(t, c, "NEW_SECRET", 0)
	_, ms := press(c, s, r("g"))
	root(t, c, ms)
	_, ms = press(c, open(t, c), r("a"), r("NEW_SECRET"), enter)
	if flashText(ms) != "! NEW_SECRET exists · rotate it instead" {
		t.Errorf("pending add: %q", flashText(ms))
	}
	// only write environments are offered
	s, _ = press(c, open(t, c), r("a"), r("OTHER_NEW"), enter)
	if got := frame(c, s); strings.Contains(got, "dev") || strings.Contains(got, "staging") {
		t.Errorf("read-only environments offered:\n%s", got)
	}
	c.Settings.WriteEnvs = nil
	_, ms = press(c, open(t, c), r("a"), r("OTHER_NEW"), enter)
	if flashText(ms) != "! no write environment · Settings w" {
		t.Errorf("no write env: %q", flashText(ms))
	}
}

func TestAddReference(t *testing.T) {
	c := pctx(80, 30)
	s := addTo(t, c, "NEW_SECRET", 0) // dev · shared
	s, _ = press(c, s, r("r"), r("${{api.OTHER_VALUE}}"))
	if !strings.Contains(frame(c, s), "reference: ${{api.OTHER_VALUE}}") {
		t.Errorf("reference input:\n%s", frame(c, s))
	}
	_, ms := press(c, s, enter)
	ch := puts(ms)
	if len(ch) != 1 || value(t, ch[0]) != "${{api.OTHER_VALUE}}" || ch[0].Group != rotation.AddGroupKey("NEW_SECRET") {
		t.Fatalf("reference: %q %+v", flashText(ms), ch)
	}

	s = addTo(t, c, "NEW_TWO", 0)
	s, ms = press(c, s, r("r"), r("plain"), enter)
	if len(puts(ms)) != 0 || flashText(ms) != "✗ not a reference · p pastes a value" {
		t.Errorf("literal: %q", flashText(ms))
	}
	if !s.(tui.Capturer).Capturing() {
		t.Error("refused reference closed the add")
	}
	_, ms = press(c, s, r("r"), r("${{web.NOPE}}"), enter)
	if len(puts(ms)) != 0 || flashText(ms) != "✗ no service web in dev" {
		t.Errorf("unresolved: %q", flashText(ms))
	}
}

func TestAddRevalidates(t *testing.T) {
	c := pctx(80, 30)
	s := addTo(t, c, "NEW_SECRET", 1)
	s, _ = press(c, s, r("p"), r(pasted), enter, r(pasted))
	replaceVars(c.Store, append(pvars(), rec("e2", "api", "NEW_SECRET", "x", "2026-10-22T00:00:00")))
	_, ms := press(c, s, enter)
	if len(puts(ms)) != 0 || flashText(ms) != "! the places changed since a · a again" {
		t.Errorf("%q", flashText(ms))
	}
}

func TestAddEscCancels(t *testing.T) {
	c := pctx(80, 30)
	for _, keys := range [][]tea.KeyMsg{
		{r("a"), esc},
		{r("a"), r("NEW_SECRET"), enter, esc},
		{r("a"), r("NEW_SECRET"), enter, space, enter, esc},
	} {
		s, ms := press(c, open(t, c), keys...)
		if len(ms) != 0 || s.(tui.Capturer).Capturing() {
			t.Errorf("esc: %#v capturing %v", ms, s.(tui.Capturer).Capturing())
		}
	}
}

func TestRotateKeysListAdd(t *testing.T) {
	c := ctx(80, 30)
	found := false
	for _, h := range open(t, c).Keys() {
		if h.Key == "a" && h.Label == "add" {
			found = true
		}
	}
	if !found {
		t.Error("a add not in the list keys")
	}
}

// Add never offers a place of a database service.
func TestAddSkipsDBPlaces(t *testing.T) {
	c := ctx(80, 30)
	s, ms := press(c, open(t, c), r("a"), r("NEW_SECRET"), enter)
	if len(ms) != 0 {
		t.Fatalf("name: %q", flashText(ms))
	}
	got := frame(c, s)
	if !strings.Contains(got, "api") || !strings.Contains(got, "web") {
		t.Fatalf("places lack api or web:\n%s", got)
	}
	if strings.Contains(got, "mongo") {
		t.Errorf("add offers the database place mongo:\n%s", got)
	}
}

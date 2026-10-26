package tui

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

func testCtx(w, h int) *Ctx {
	return &Ctx{
		Theme:   NewTheme(termenv.Ascii),
		Icons:   Icons{},
		Account: "fake@example.test",
		Width:   w,
		Height:  h,
		IsTTY:   true,
	}
}

func change(id, env, svc string) changeset.Change {
	return changeset.Change{ID: id, Kind: "variable", Place: domain.Place{ProjectID: "p", EnvID: env, ServiceID: svc}, Target: "X"}
}

// render is Frame without the SGR attributes an Ascii theme still emits.
func render(ctx *Ctx, title, note, body string, f Footer) string {
	return ansi.Strip(Frame(ctx, title, note, body, f))
}

func lines(s string) []string { return strings.Split(s, "\n") }

func TestFrameNeverWraps(t *testing.T) {
	ctx := testCtx(60, 20)
	ctx.CS = &changeset.ChangeSet{Changes: []changeset.Change{change("1", "e1", "s1")}}
	out := Frame(ctx, strings.Repeat("T", 200), strings.Repeat("n", 100),
		strings.Repeat("B", 300)+"\n日本語のテキスト😀😀 with\ttab\nshort", Footer{
			Flash: FlashMsg{Kind: '!', Text: strings.Repeat("f", 300)},
			Hints: []KeyHint{{Key: "enter", Label: "open", Always: true}},
		})
	ls := lines(out)
	if len(ls) != ctx.Height {
		t.Errorf("line count = %d, want %d", len(ls), ctx.Height)
	}
	for i, l := range ls {
		if w := lipgloss.Width(l); w != ctx.Width {
			t.Errorf("line %d width %d != %d: %q", i, w, ctx.Width, l)
		}
	}
}

func TestFooterIndicator(t *testing.T) {
	ctx := testCtx(100, 14)
	ctx.CS = &changeset.ChangeSet{Changes: []changeset.Change{
		change("1", "e1", "s1"), change("2", "e1", "s1"), change("3", "e1", "s2"),
		change("4", "e2", "s1"), change("5", "e2", "s1"),
	}}
	out := render(ctx, "t", "", "body", Footer{})
	if !strings.Contains(out, "● 5 changes · 3 services · 2 envs") {
		t.Errorf("indicator missing:\n%s", out)
	}
	ctx.CS = &changeset.ChangeSet{}
	if out := render(ctx, "t", "", "body", Footer{}); strings.Contains(out, "●") {
		t.Errorf("empty set shows indicator:\n%s", out)
	}
	ctx.CS = nil
	if out := render(ctx, "t", "", "body", Footer{}); strings.Contains(out, "●") {
		t.Errorf("nil set shows indicator")
	}
}

func TestHintsMore(t *testing.T) {
	ctx := testCtx(50, 12)
	hints := []KeyHint{
		{Key: "q", Label: "quit", Always: true},
		{Key: "?", Label: "help", Always: true},
		{Key: "a", Label: "alpha action", Rank: 1},
		{Key: "b", Label: "bravo action", Rank: 2},
		{Key: "c", Label: "charlie action", Rank: 3},
		{Key: "d", Label: "delta action", Rank: 4},
	}
	out := render(ctx, "t", "", "body", Footer{Hints: hints})
	for _, want := range []string{"? more", "q quit", "alpha action"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "delta action") {
		t.Errorf("hint that does not fit is shown:\n%s", out)
	}
	wide := testCtx(200, 12)
	if out := render(wide, "t", "", "body", Footer{Hints: hints}); strings.Contains(out, "? more") {
		t.Errorf("? more shown although all fit")
	}
}

func TestFrameSanitizesTitle(t *testing.T) {
	ctx := testCtx(80, 12)
	raw := Frame(ctx, "bad\x1b[2Jtitle", "no\x1b]0;x\x07te", "body",
		Footer{Flash: FlashMsg{Kind: '✗', Text: "fl\x1b[2Jash"}})
	if strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "\x1b]") || strings.Contains(raw, "\x07") {
		t.Errorf("injected escape in frame: %q", raw)
	}
	out := ansi.Strip(raw)
	if !strings.Contains(out, "badtitle") || !strings.Contains(out, "note") {
		t.Errorf("sanitized text missing:\n%s", out)
	}
}

// The frame never shows the account: Home puts it in its header note.
func TestFrameHasNoAccountLine(t *testing.T) {
	ctx := testCtx(80, 12)
	out := ansi.Strip(Frame(ctx, "Services", "", "body", Footer{}))
	if strings.Contains(out, ctx.Account) {
		t.Errorf("frame shows the account:\n%s", out)
	}
	if n := strings.Count(out, "\n") + 1; n != 12 {
		t.Errorf("frame is %d lines, want 12", n)
	}
}

func sampleFrame(w int) string {
	ctx := testCtx(w, 14)
	ctx.CS = &changeset.ChangeSet{Changes: []changeset.Change{change("1", "e1", "s1"), change("2", "e2", "s2")}}
	body := "  api            production   3 variables\n> worker         production   1 variable\n  web            staging      12 variables"
	return Frame(ctx, "Services", "synced 12s ago", body, Footer{
		Flash: FlashMsg{Kind: '✓', Text: "saved settings"},
		Hints: []KeyHint{
			{Key: "enter", Label: "open", Always: true},
			{Key: "esc", Label: "back", Always: true},
			{Key: "e", Label: "edit variable", Rank: 1},
			{Key: "a", Label: "apply changes", Rank: 2},
			{Key: "s", Label: "settings", Rank: 3},
		},
	})
}

func TestFrameGolden(t *testing.T) {
	tuitest.Golden(t, "frame_80.golden", sampleFrame(80))
	tuitest.Golden(t, "frame_60.golden", sampleFrame(60))
}

func TestFlashUnknownKind(t *testing.T) {
	ctx := testCtx(80, 12)
	raw := Frame(ctx, "t", "", "body", Footer{Flash: FlashMsg{Text: "plain message"}})
	if strings.Contains(raw, "\x00") {
		t.Errorf("NUL in frame")
	}
	if !strings.Contains(ansi.Strip(raw), "┃ plain message") {
		t.Errorf("flash text missing or has a glyph:\n%s", ansi.Strip(raw))
	}
	raw = Frame(ctx, "t", "", "body", Footer{Flash: FlashMsg{Kind: 'x', Text: "m"}})
	if strings.Contains(raw, "x m") || strings.Contains(raw, "\x00") {
		t.Errorf("unknown kind rendered a glyph")
	}
}

func TestThemeSelectedVisible(t *testing.T) {
	for _, p := range []termenv.Profile{termenv.Ascii, termenv.ANSI, termenv.ANSI256, termenv.TrueColor} {
		th := NewTheme(p)
		if got := th.Selected.Render("x"); got == "x" {
			t.Errorf("profile %v: Selected renders as plain text", p)
		}
	}
	// NO_COLOR: attributes only, no color sequences.
	got := NewTheme(termenv.Ascii).Selected.Render("x")
	if strings.Contains(got, "38;") || strings.Contains(got, "48;") || strings.Contains(got, "\x1b[3") || strings.Contains(got, "\x1b[4") {
		t.Errorf("Ascii selection carries a color: %q", got)
	}
}

func TestOptionalHintNeverChangesTheOthers(t *testing.T) {
	ctx := testCtx(60, 12)
	base := []KeyHint{
		{Key: "q", Label: "quit", Always: true},
		{Key: "a", Label: "alpha action", Rank: 1},
		{Key: "b", Label: "bravo action", Rank: 2},
		{Key: "c", Label: "charlie action", Rank: 3},
		{Key: "d", Label: "delta action", Rank: 4},
		{Key: "e", Label: "echo action", Rank: 5},
	}
	opt := KeyHint{Key: "/", Label: "filter", Rank: 9, Optional: true}
	// widths: everything fits / a "? more" is needed with no room left /
	// a "? more" is needed with room left
	for _, w := range []int{200, 50, 56, 40} {
		without := ansi.Strip(hintLine(ctx, base, w))
		with := ansi.Strip(hintLine(ctx, append(slices.Clone(base), opt), w))
		shown := strings.Contains(with, "/ filter")
		stripped := strings.Replace(strings.Replace(with, "  / filter", "", 1), "/ filter  ", "", 1)
		if !shown && with != without {
			t.Errorf("w=%d: a left-out optional hint changed the line:\n%q\n%q", w, without, with)
		}
		if shown && strings.Contains(without, "? more") != strings.Contains(with, "? more") {
			t.Errorf("w=%d: optional hint changed the ? more decision:\n%q\n%q", w, without, with)
		}
		if shown && stripped != without {
			t.Errorf("w=%d: shown optional hint evicted another:\n%q\n%q", w, without, with)
		}
		if ansi.StringWidth(with) > w {
			t.Errorf("w=%d: line too wide: %q", w, with)
		}
	}
	if got := ansi.Strip(hintLine(ctx, append(slices.Clone(base), opt), 200)); !strings.Contains(got, "/ filter") {
		t.Errorf("optional hint should show when there is room: %q", got)
	}
}

// A Nerd Font glyph gets two spaces before its text; a plain one, one.
func TestIconsGap(t *testing.T) {
	if g := (Icons{Nerd: true}).Gap(); g != "  " {
		t.Errorf("nerd gap %q", g)
	}
	if g := (Icons{}).Gap(); g != " " {
		t.Errorf("plain gap %q", g)
	}
}

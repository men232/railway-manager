package components

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/tui"
)

func fkey(f *Filter, c *tui.Ctx, k tea.KeyMsg) bool {
	h, _ := f.Update(c, k)
	return h
}

func TestFilterTypingFilters(t *testing.T) {
	c := &tui.Ctx{}
	f := NewFilter("k")
	if f.Active() || f.Editing() || !f.Match("anything") || f.View(40, 3, 3) != "" {
		t.Fatal("a new filter must be inert")
	}
	if !fkey(f, c, typ("/")) || !f.Editing() {
		t.Fatal("/ should start editing")
	}
	if got := f.View(40, 3, 3); !strings.HasPrefix(got, "/ ▏") || !strings.Contains(got, "3/3") {
		t.Errorf("empty editing view = %q", got)
	}
	fkey(f, c, typ("Mo"))
	if !f.Active() || !f.Match("a MOnitor row") || f.Match("api") {
		t.Errorf("match is case-insensitive substring")
	}
	if got := f.View(40, 1, 3); got != "/ Mo▏  1/3" {
		t.Errorf("view = %q", got)
	}
	if c.Filters["k"] != "Mo" {
		t.Errorf("saved = %q", c.Filters["k"])
	}
}

func TestFilterArrowsLeaveEditingKeepText(t *testing.T) {
	for _, k := range []tea.KeyType{tea.KeyUp, tea.KeyDown} {
		c := &tui.Ctx{}
		f := NewFilter("k")
		fkey(f, c, typ("/"))
		fkey(f, c, typ("mo"))
		if fkey(f, c, tea.KeyMsg{Type: k}) {
			t.Errorf("%v must be passed on", k)
		}
		if f.Editing() || !f.Active() || c.Filters["k"] != "mo" {
			t.Errorf("%v: editing=%v active=%v saved=%q", k, f.Editing(), f.Active(), c.Filters["k"])
		}
		if got := f.View(40, 1, 3); got != "/ mo  1/3" {
			t.Errorf("view without caret = %q", got)
		}
	}
}

func TestFilterEnterLeavesEditing(t *testing.T) {
	c := &tui.Ctx{}
	f := NewFilter("k")
	fkey(f, c, typ("/"))
	fkey(f, c, typ("a"))
	if !fkey(f, c, enter) || f.Editing() || !f.Active() {
		t.Error("enter keeps the text and leaves editing")
	}
}

func TestFilterSlashReentersEditing(t *testing.T) {
	c := &tui.Ctx{}
	f := NewFilter("k")
	fkey(f, c, typ("/"))
	fkey(f, c, typ("ab"))
	fkey(f, c, enter)
	if fkey(f, c, typ("x")) {
		t.Error("other keys are not the filter's when not editing")
	}
	if !fkey(f, c, typ("/")) || !f.Editing() {
		t.Fatal("/ should re-enter editing")
	}
	fkey(f, c, typ("c"))
	if got := f.View(40, 0, 0); !strings.HasPrefix(got, "/ abc▏") {
		t.Errorf("text kept: %q", got)
	}
}

func TestFilterClearRemoves(t *testing.T) {
	for name, keys := range map[string][]tea.KeyMsg{
		"backspace": {{Type: tea.KeyBackspace}, {Type: tea.KeyBackspace}},
		"ctrl+u":    {{Type: tea.KeyCtrlU}},
		"esc":       {esc},
	} {
		c := &tui.Ctx{}
		f := NewFilter("k")
		fkey(f, c, typ("/"))
		fkey(f, c, typ("ab"))
		for _, k := range keys {
			fkey(f, c, k)
		}
		if f.Active() || f.Editing() || f.View(40, 1, 1) != "" || c.Filters["k"] != "" {
			t.Errorf("%s: filter not removed (active=%v editing=%v view=%q)", name, f.Active(), f.Editing(), f.View(40, 1, 1))
		}
	}
}

func TestFilterLoadAndSeparateKeys(t *testing.T) {
	c := &tui.Ctx{Filters: map[string]string{"a": "xy"}}
	f := NewFilter("a")
	f.Load(c)
	if !f.Active() || f.Editing() || !f.Match("..XY..") {
		t.Error("Load restores the saved text, not editing")
	}
	g := NewFilter("b")
	g.Load(c)
	if g.Active() {
		t.Error("another key has its own text")
	}
}

func TestFilterViewSanitizesAndTruncates(t *testing.T) {
	c := &tui.Ctx{}
	f := NewFilter("k")
	fkey(f, c, typ("/"))
	fkey(f, c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\x1b[31mb")})
	if strings.Contains(f.View(40, 1, 1), "\x1b") {
		t.Error("escape sequence leaked")
	}
	fkey(f, c, typ(strings.Repeat("z", 100)))
	if got := f.View(20, 1, 1); len([]rune(got)) > 20 {
		t.Errorf("not cut to width: %q", got)
	}
}

func TestFilterEscWhenNotEditingIsNotHandledAndKeepsText(t *testing.T) {
	c := &tui.Ctx{}
	f := NewFilter("k")
	fkey(f, c, typ("/"))
	fkey(f, c, typ("ab"))
	fkey(f, c, enter)
	if fkey(f, c, esc) {
		t.Error("esc while not editing belongs to the screen (back)")
	}
	if !f.Active() || c.Filters["k"] != "ab" {
		t.Error("filter text must be kept")
	}
}

func TestFilterMatchSanitizesRowText(t *testing.T) {
	c := &tui.Ctx{}
	f := NewFilter("k")
	fkey(f, c, typ("/"))
	fkey(f, c, typ("amo"))
	if !f.Match("a\x1b[31mmo\x1b[0mnitor") {
		t.Error("escape sequences must not break a match")
	}
	if f.Match("a\x1b[31m") {
		t.Error("sequence text must not match")
	}
}

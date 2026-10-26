package components

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

func typ(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	enter = tea.KeyMsg{Type: tea.KeyEnter}
	esc   = tea.KeyMsg{Type: tea.KeyEsc}
)

func run(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

func TestMaskedInputRepeat(t *testing.T) {
	in := &MaskedInput{Repeat: true}
	in.Update(typ("abc"))
	if v := in.View(40); strings.Contains(v, "abc") || !strings.Contains(v, "•••") || !strings.Contains(v, "(3)") {
		t.Errorf("view = %q", v)
	}
	if msg := run(in.Update(enter)); msg != nil {
		t.Fatalf("first enter emitted %#v", msg)
	}
	in.Update(typ("abd"))
	msg := run(in.Update(enter))
	f, ok := msg.(tui.FlashMsg)
	if !ok || f.Kind != '✗' || !strings.Contains(f.Text, "the two entries differ") {
		t.Fatalf("mismatch msg = %#v", msg)
	}
	// restarted: matching entries submit
	in.Update(typ("abc"))
	in.Update(enter)
	in.Update(typ("abc"))
	msg = run(in.Update(enter))
	if s, ok := msg.(SubmitMsg); !ok || s.Value != "abc" {
		t.Fatalf("match msg = %#v", msg)
	}
	if strings.Contains(in.View(40), "abc") {
		t.Errorf("view leaks value")
	}
}

func TestMaskedInputZeroesAndNoLeak(t *testing.T) {
	secret := "pässwörd✓"
	in := &MaskedInput{}
	in.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(secret), Paste: true})
	buf := in.ed.buf
	if v := in.View(60); strings.Contains(v, "pässwörd") || strings.Contains(v, "✓") {
		t.Errorf("view leaks: %q", v)
	}
	if s, ok := run(in.Update(enter)).(SubmitMsg); !ok || s.Value != secret {
		t.Fatalf("submit = %#v", s)
	}
	for i, r := range buf {
		if r != 0 {
			t.Fatalf("buffer rune %d not zeroed after submit", i)
		}
	}
	if len(in.ed.buf) != 0 {
		t.Errorf("buffer kept after submit")
	}

	in = &MaskedInput{Repeat: true}
	in.Update(typ("abc"))
	first := in.ed.buf
	in.Update(enter)
	in.Update(typ("abd"))
	second := in.ed.buf
	in.Update(enter)
	for _, b := range [][]rune{first, second} {
		for _, r := range b {
			if r != 0 {
				t.Fatalf("repeat buffers not zeroed after mismatch")
			}
		}
	}

	in = &MaskedInput{}
	in.Update(typ("abc"))
	buf = in.ed.buf
	in.Update(esc)
	for _, r := range buf {
		if r != 0 {
			t.Fatalf("buffer not zeroed after cancel")
		}
	}
}

func TestLabelsSanitized(t *testing.T) {
	for _, v := range []string{
		(&TextInput{Label: "a\x1b[2Jb"}).View(40),
		(&MaskedInput{Label: "a\x1b[2Jb"}).View(40),
		(&Confirm{Prompt: "a\x1b[2Jb"}).View(40),
		(&Confirm{Prompt: "a\x1b[2Jb", Word: "w\x1b[2J"}).View(80),
	} {
		if strings.Contains(v, "\x1b") || !strings.Contains(v, "ab") {
			t.Errorf("view = %q", v)
		}
	}
}

func TestMaskedInputSingle(t *testing.T) {
	in := &MaskedInput{}
	in.Update(typ("pw"))
	in.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	in.Update(typ("x"))
	if s, ok := run(in.Update(enter)).(SubmitMsg); !ok || s.Value != "px" {
		t.Errorf("submit = %#v", s)
	}
	if _, ok := run(in.Update(esc)).(CancelMsg); !ok {
		t.Errorf("esc did not cancel")
	}
}

// A pasted multi-line secret reaches the submit whole: the terminal's "\r"
// line breaks become "\n", nothing is sanitized away, and nothing is drawn.
func TestMaskedInputKeepsPastedLines(t *testing.T) {
	in := &MaskedInput{}
	pem := "-----BEGIN KEY-----\rfixture-line\tone\r\n-----END KEY-----"
	in.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pem), Paste: true})
	if strings.Contains(in.View(80), "fixture-line") {
		t.Error("the masked input drew the value")
	}
	want := "-----BEGIN KEY-----\nfixture-line\tone\n-----END KEY-----"
	if s, ok := run(in.Update(enter)).(SubmitMsg); !ok || s.Value != want {
		t.Errorf("submit = %q, want %q", s.Value, want)
	}
}

func TestTextInput(t *testing.T) {
	in := &TextInput{}
	in.Update(typ("héy\x1b[2J"))
	in.Update(tea.KeyMsg{Type: tea.KeyLeft})
	in.Update(typ("l"))
	if s, ok := run(in.Update(enter)).(SubmitMsg); !ok || s.Value != "hély" {
		t.Errorf("submit = %#v", s)
	}
	if _, ok := run(in.Update(esc)).(CancelMsg); !ok {
		t.Errorf("esc did not cancel")
	}
}

func TestConfirmWord(t *testing.T) {
	c := &Confirm{Prompt: "Apply 3 changes?", Word: "apply"}
	c.Update(typ("apply"))
	if _, ok := run(c.Update(enter)).(ConfirmedMsg); !ok {
		t.Errorf("apply+enter not confirmed")
	}
	c = &Confirm{Prompt: "Apply?", Word: "apply"}
	c.Update(typ("aply"))
	if _, ok := run(c.Update(enter)).(CancelMsg); !ok {
		t.Errorf("aply+enter not cancelled")
	}
	c = &Confirm{Prompt: "Delete?"}
	if _, ok := run(c.Update(typ("y"))).(ConfirmedMsg); !ok {
		t.Errorf("y not confirmed")
	}
	c = &Confirm{Prompt: "Delete?"}
	if _, ok := run(c.Update(typ("x"))).(CancelMsg); !ok {
		t.Errorf("other key did not cancel")
	}
	if v := (&Confirm{Prompt: "Delete?"}).View(40); !strings.Contains(v, "y/n") {
		t.Errorf("view = %q", v)
	}
}

func TestListMoveWindow(t *testing.T) {
	var l List
	l.Move(10, 1, 3)
	l.Move(10, 1, 3)
	l.Move(10, 1, 3)
	if l.Cursor != 3 || l.Offset != 1 {
		t.Errorf("cursor/offset = %d/%d", l.Cursor, l.Offset)
	}
	if from, to := l.Window(10, 3); from != 1 || to != 4 {
		t.Errorf("window = %d..%d", from, to)
	}
	l.Move(10, -100, 3)
	if l.Cursor != 0 || l.Offset != 0 {
		t.Errorf("after up: %d/%d", l.Cursor, l.Offset)
	}
	l.Move(10, 100, 3)
	if l.Cursor != 9 || l.Offset != 7 {
		t.Errorf("after down: %d/%d", l.Cursor, l.Offset)
	}
	// list shrinks
	if from, to := l.Window(4, 3); from != 1 || to != 4 {
		t.Errorf("shrunk window = %d..%d", from, to)
	}
	l.Move(0, 1, 3)
	if l.Cursor != 0 || l.Offset != 0 {
		t.Errorf("empty: %d/%d", l.Cursor, l.Offset)
	}
	if from, to := l.Window(5, 0); from != to {
		t.Errorf("zero height window = %d..%d", from, to)
	}
}

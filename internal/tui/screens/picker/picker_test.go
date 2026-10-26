package picker_test

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/picker"
	"github.com/men232/railway-manager/internal/tui/tuitest"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

type doneMsg struct{ ids []string }

var items = []picker.Item{
	{ID: "i1", Label: "shop", Note: "ws-one"},
	{ID: "i2", Label: "blog", Note: "ws-one"},
	{ID: "i3", Label: "docs", Note: "ws-two"},
}

func ctx(w, h int) *tui.Ctx {
	return &tui.Ctx{Theme: tui.NewTheme(tuitest.Profile()), Width: w, Height: h}
}

func press(c *tui.Ctx, s tui.Screen, ks ...tea.KeyMsg) (tui.Screen, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range ks {
		s, cmd = s.Update(c, k)
	}
	return s, cmd
}

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	down  = tea.KeyMsg{Type: tea.KeyDown}
	space = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	enter = tea.KeyMsg{Type: tea.KeyEnter}
)

// run executes a command, flattening tea.Sequence into the messages in order.
func run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	v := reflect.ValueOf(msg)
	if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Func {
		var out []tea.Msg
		for i := range v.Len() {
			out = append(out, run(v.Index(i).Interface().(tea.Cmd))...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func newPicker(sel map[string]bool) tui.Screen {
	return picker.New("Projects", items, sel, func(ids []string) tea.Msg { return doneMsg{ids} })
}

func TestPickerToggleAndConfirm(t *testing.T) {
	c := ctx(80, 20)
	s, cmd := press(c, newPicker(nil), down, space, enter)
	msgs := run(cmd)
	if len(msgs) != 2 {
		t.Fatalf("msgs = %#v", msgs)
	}
	if _, ok := msgs[0].(tui.PopMsg); !ok {
		t.Errorf("first message %#v, want PopMsg", msgs[0])
	}
	if d, ok := msgs[1].(doneMsg); !ok || !reflect.DeepEqual(d.ids, []string{"i2"}) {
		t.Errorf("done = %#v", msgs[1])
	}
	_ = s
}

func TestPickerAllNoneAndSeed(t *testing.T) {
	c := ctx(80, 20)
	_, cmd := press(c, newPicker(map[string]bool{"i3": true, "gone": true}), enter)
	if d := run(cmd)[1].(doneMsg); !reflect.DeepEqual(d.ids, []string{"i3"}) {
		t.Errorf("seed = %v", d.ids)
	}
	_, cmd = press(c, newPicker(nil), r("a"), enter)
	if d := run(cmd)[1].(doneMsg); len(d.ids) != 3 {
		t.Errorf("all = %v", d.ids)
	}
	_, cmd = press(c, newPicker(map[string]bool{"i1": true}), r("n"), enter)
	if d := run(cmd)[1].(doneMsg); len(d.ids) != 0 {
		t.Errorf("none = %v", d.ids)
	}
}

func TestPickerCancel(t *testing.T) {
	c := ctx(80, 20)
	for _, k := range []tea.KeyMsg{r("q"), {Type: tea.KeyEsc}} {
		_, cmd := press(c, newPicker(nil), space, k)
		msgs := run(cmd)
		if len(msgs) != 1 {
			t.Fatalf("msgs = %#v", msgs)
		}
		if _, ok := msgs[0].(tui.PopMsg); !ok {
			t.Errorf("cancel = %#v", msgs[0])
		}
	}
}

func TestPickerGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 14)
		s, _ := press(c, newPicker(map[string]bool{"i1": true}), down)
		out := tuitest.Plain(tui.Frame(c, s.Title(), "", s.View(c, w-tui.ChromeCols, 14-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
		tuitest.Golden(t, "picker_"+strconv.Itoa(w)+".golden", out)
	}
}

func TestBuildBadArgs(t *testing.T) {
	if s := picker.Build("t", 42); s != nil {
		t.Errorf("Build with a bad arg = %#v, want nil", s)
	}
	if s := picker.Build("t", items, map[string]bool{}, func([]string) tea.Msg { return nil }); s == nil {
		t.Error("Build with good args = nil")
	}
}

func TestPickerFilterNarrowsAndToggles(t *testing.T) {
	c := ctx(80, 20)
	s, _ := press(c, newPicker(nil), r("/"), r("lo"), enter)
	out := ansi.Strip(s.View(c, 70, 10))
	if strings.Contains(out, "shop") || !strings.Contains(out, "blog") || !strings.Contains(out, "1/3") {
		t.Errorf("filtered:\n%s", out)
	}
	s, cmd := press(c, s, space, enter)
	_ = s
	if d := run(cmd)[1].(doneMsg); !reflect.DeepEqual(d.ids, []string{"i2"}) {
		t.Errorf("toggle acts on the filtered row, got %v", d.ids)
	}
	if c.Filters["picker:Projects"] != "lo" {
		t.Errorf("saved under %v", c.Filters)
	}
}

func TestPickerAllNoneActOnFilteredItems(t *testing.T) {
	c := ctx(80, 20)
	// "ws-one" matches shop and blog by note; docs stays out
	_, cmd := press(c, newPicker(nil), r("/"), r("ws-one"), enter, r("a"), enter)
	if d := run(cmd)[1].(doneMsg); !reflect.DeepEqual(d.ids, []string{"i1", "i2"}) {
		t.Errorf("a with a filter = %v", d.ids)
	}
	c = ctx(80, 20) // the first filter is remembered in c
	_, cmd = press(c, newPicker(map[string]bool{"i1": true, "i3": true}), r("/"), r("ws-one"), enter, r("n"), enter)
	if d := run(cmd)[1].(doneMsg); !reflect.DeepEqual(d.ids, []string{"i3"}) {
		t.Errorf("n with a filter = %v", d.ids)
	}
}

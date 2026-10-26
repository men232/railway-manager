package services_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/services"
)

// coveringSeries is the api's series at step over the whole span: every
// value above zero, so no cell of a chart that fills is blank.
func coveringSeries(step int, span time.Duration) []railway.MetricSeries {
	n := int(span/time.Second) / step
	at := func(m string, f func(int) float64) railway.MetricSeries {
		s := railway.MetricSeries{EnvID: "e1", ServiceID: "s1", Measurement: m}
		for i := range n {
			s.Points = append(s.Points, railway.MetricPoint{TS: now.Unix() - int64(step*(n-1-i)), Value: f(i)})
		}
		return s
	}
	return []railway.MetricSeries{
		at("CPU_USAGE", func(i int) float64 { return 1 + float64(i%3) }),
		at("CPU_LIMIT", func(int) float64 { return 32 }),
		at("MEMORY_USAGE_GB", func(i int) float64 { return 1 + float64(i%2) }),
		at("MEMORY_LIMIT_GB", func(int) float64 { return 32 }),
		at("NETWORK_RX_GB", func(i int) float64 { return float64(i+1) / 1024 }),
		at("NETWORK_TX_GB", func(i int) float64 { return float64(i+1) / 2048 }),
	}
}

// leadingBlank counts the blank Braille cells (U+2800) a chart starts with.
func leadingBlank(l string) int {
	n := 0
	started := false
	for _, r := range l {
		if r < 0x2800 || r > 0x28FF {
			if started {
				break
			}
			continue
		}
		started = true
		if r != 0x2800 {
			break
		}
		n++
	}
	return n
}

// chartLines are the line that starts with a chart's label and the next.
func chartLines(out, label string) (string, string) {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, label) && i+1 < len(lines) {
			return l, lines[i+1]
		}
	}
	return "", ""
}

// chartBlank counts the leading columns blank on both lines of a two-line
// chart (or on the one line of a one-line chart).
func chartBlank(l1, l2 string) int {
	b := leadingBlank(l1)
	if len(brailleRuns(l2)) == 0 {
		return b
	}
	return min(b, leadingBlank(l2))
}

// panelTitle is the panel's first line: the one under the separator.
func panelTitle(out string) string {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "──") && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return ""
}

// The panel charts fill their width when the series covers the span, at
// every level and width: the span follows the chart cells.
func TestPanelChartsFillTheirWidth(t *testing.T) {
	for i, lv := range loaders.Levels {
		for _, w := range []int{60, 80, 100, 120, 150, 200, 260} {
			c := panelCtx(w, panelHeight)
			bw := w - tui.ChromeCols
			span := services.PanelSpanAt(bw, lv.Step)
			c.Store.SetPanelSeries(apiPlace, lv.Step, span, coveringSeries(lv.Step, span))
			s := tui.Screen(services.New())
			for range i {
				s, _ = s.Update(c, r1("]"))
			}
			out := ansi.Strip(body(c, s))
			if title := strings.TrimRight(panelTitle(out), " "); !strings.HasSuffix(title, " "+lv.Name) {
				t.Fatalf("%d %s: title %q", w, lv.Name, title)
			}
			for _, name := range []string{"cpu ", "mem ", "net "} {
				l, l2 := chartLines(out, name)
				runs := brailleRuns(l)
				if len(runs) != 1 || runs[0] != services.ChartCells(bw) {
					t.Errorf("%d %s %s: chart runs %v, want one of %d: %q", w, lv.Name, name, runs, services.ChartCells(bw), l)
				}
				if b := chartBlank(l, l2); b > 1 {
					t.Errorf("%d %s %s: %d leading blank cells: %q", w, lv.Name, name, b, l)
				}
			}
		}
	}
}

// A service with less history than the span leaves the left blank.
func TestPanelChartShortHistoryLeftBlank(t *testing.T) {
	c := panelCtx(150, panelHeight)
	span := services.PanelSpanAt(150-tui.ChromeCols, 60)
	half := coveringSeries(60, span)
	for i := range half {
		half[i].Points = half[i].Points[len(half[i].Points)/2:]
	}
	c.Store.SetPanelSeries(apiPlace, 60, span, half)
	l, l2 := chartLines(ansi.Strip(body(c, services.New())), "cpu ")
	cells := services.ChartCells(150 - tui.ChromeCols)
	if b := chartBlank(l, l2); b < cells/2-1 || b > cells/2+1 {
		t.Errorf("half the history: %d of %d cells blank: %q", b, cells, l)
	}
}

// resizeTo sets the terminal size as the root does and hands the screen the
// size message.
func resizeTo(r *rig, s tui.Screen, w, h int) (tui.Screen, []services.Scheduled, panelLoads) {
	r.c.Width, r.c.Height = w, h
	s, cmd := s.Update(r.c, tea.WindowSizeMsg{Width: w, Height: h})
	_, tms, other := run(cmd)
	return s, tms, panelOf(other)
}

// A resize that changes the chart cells reads the panel metrics again once
// the resizing rests; one that keeps them reads nothing.
func TestPanelResizeReloadsOnCellChange(t *testing.T) {
	r := panelRig(t)
	s, _ := r.rested(services.New())

	// same width: nothing
	s, tms, p := resizeTo(r, s, 80, 44)
	if len(tms) != 0 || len(p.panels) != 0 {
		t.Fatalf("height-only resize: timers %v loads %#v", tms, p)
	}

	// a cell change waits for the resizing to rest
	s, first, p := resizeTo(r, s, 120, 44)
	if len(first) != 1 || first[0].D != loaders.DetailDelay || len(p.panels) != 0 {
		t.Fatalf("resize: timers %v loads %#v", first, p)
	}
	s, second, _ := resizeTo(r, s, 140, 44)
	if len(second) != 1 {
		t.Fatalf("second resize: timers %v", second)
	}
	// the first timer is stale
	s, cmd := r.deliver(s, first[0].Msg)
	if _, tms, other := run(cmd); len(tms) != 0 || len(panelOf(other).panels) != 0 {
		t.Errorf("stale resize timer: %v %v", tms, other)
	}
	want := services.PanelSpanAt(140-tui.ChromeCols, 60)
	s, cmd = r.deliver(s, second[0].Msg)
	_, tms, other := run(cmd)
	if p := panelOf(other).panels; len(p) != 1 || p[0].Span != want || p[0].Step != 60 || p[0].Place != apiPlace {
		t.Errorf("rested resize: %#v, want span %v", p, want)
	}
	if len(scheduled(tms, loaders.MetricsEvery)) != 1 {
		t.Errorf("the poll restarts: %v", tms)
	}

	// there and back before the timer: the timer finds the cells unchanged
	s, tms, _ = resizeTo(r, s, 80, 44)
	if len(tms) != 1 {
		t.Fatalf("resize to 80: %v", tms)
	}
	s, _, _ = resizeTo(r, s, 140, 44)
	s, cmd = r.deliver(s, tms[0].Msg)
	if _, tms, other := run(cmd); len(tms) != 0 || len(panelOf(other).panels) != 0 {
		t.Errorf("back to the read width before the timer: %v %v", tms, other)
	}

	// past the cell cap the span stays: nothing to read
	s, tms, _ = resizeTo(r, s, 300, 44)
	if len(tms) != 1 {
		t.Fatalf("resize to 300: %v", tms)
	}
	s, cmd = r.deliver(s, tms[0].Msg)
	if p := panelOf(runOther(cmd)).panels; len(p) != 1 || p[0].Span != loaders.PanelSpan(60, loaders.MaxPanelCells) {
		t.Fatalf("at 300: %#v", p)
	}
	s, tms, _ = resizeTo(r, s, 320, 44)
	if len(tms) != 0 {
		t.Errorf("a resize past the cap reads again: %v", tms)
	}
	s.(tui.Suspender).Suspend(r.c)
}

func runOther(cmd tea.Cmd) []tea.Msg {
	_, _, other := run(cmd)
	return other
}

// The poll reads the current span.
func TestPanelPollUsesCurrentSpan(t *testing.T) {
	r := panelRig(t)
	r.c.Width = 200
	s, poll := r.rested(services.New())
	want := services.PanelSpanAt(200-tui.ChromeCols, 60)
	s, cmd := r.deliver(s, poll.Msg)
	if p := panelOf(runOther(cmd)).panels; len(p) != 1 || p[0].Span != want {
		t.Errorf("poll: %#v, want span %v", p, want)
	}
	s.(tui.Suspender).Suspend(r.c)
}

// A result for another span of the same step does not show.
func TestPanelOldSpanResultDropped(t *testing.T) {
	c := panelCtx(150, panelHeight)
	cur := services.PanelSpanAt(150-tui.ChromeCols, 60)
	old := services.PanelSpanAt(80-tui.ChromeCols, 60)
	ser := []railway.MetricSeries{{EnvID: "e1", ServiceID: "s1", Measurement: "CPU_USAGE",
		Points: []railway.MetricPoint{{TS: now.Unix(), Value: 9.87}}}}
	loaders.PanelMetricsMsg{Place: apiPlace, Step: 60, Span: old, Series: ser, At: now}.ApplyTo(c.Store)
	if out := ansi.Strip(body(c, services.New())); strings.Contains(out, "9.87") {
		t.Errorf("a result over %v shows at %v:\n%s", old, cur, out)
	}
	loaders.PanelMetricsMsg{Place: apiPlace, Step: 60, Span: cur, Series: ser, At: now}.ApplyTo(c.Store)
	if out := ansi.Strip(body(c, services.New())); !strings.Contains(out, "9.87") {
		t.Errorf("the current span's result should show:\n%s", out)
	}
}

// A muted line of ─ separates the table from the panel; the panel keeps
// to the bottom of the body.
func TestPanelSeparator(t *testing.T) {
	c := panelCtx(80, panelHeight)
	lines := strings.Split(ansi.Strip(body(c, services.New())), "\n")
	for i, l := range lines {
		if strings.Contains(l, "● ONLINE  api") {
			if i == 0 || lines[i-1] != strings.Repeat("─", 80-tui.ChromeCols) {
				t.Errorf("line above the panel = %q", lines[max(0, i-1)])
			}
			// blank lines pad a short table so the panel keeps to the bottom
			j := i - 2
			for j > 0 && lines[j] == "" {
				j--
			}
			if j < 0 || !strings.Contains(lines[j], "site") {
				t.Errorf("the table's last row should be the last line above the separator: %q", lines[max(0, j)])
			}
			return
		}
	}
	t.Errorf("no panel:\n%s", strings.Join(lines, "\n"))
}

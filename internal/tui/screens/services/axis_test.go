package services_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/services"
)

// Ticks sit every 5 cells back from the newest cell; each label is centred
// on its tick (an even-width label leans right), on every tick when the
// widest label fits with a blank between, else on every second, third...
// tick. "now" follows the chart in the captions' column.
func TestAxisTicks(t *testing.T) {
	for _, tc := range []struct {
		step int
		want string
	}{
		// 5 cells are 10 minutes; "-1h10" is too wide for every tick
		{60, "        -1h       -40m      -20m        " + "  now"},
		// 5 cells are 50 minutes
		{300, "        -5h      -3h20     -1h40        " + "  now"},
		// 5 cells are 2.5 hours
		{900, "        -15h      -10h      -5h         " + "  now"},
		// 5 cells are 10 hours
		{3600, "        -60h      -40h      -20h        " + "  now"},
	} {
		if got := services.Axis(tc.step, 40); got != tc.want {
			t.Errorf("step %d:\n got %q\nwant %q", tc.step, got, tc.want)
		}
	}
}

var axisLabel = regexp.MustCompile(`^(-[0-9]+m|-[0-9]+h([0-5][0-9])?)$`)

// Labels never touch, sit centred on the 5-cell grid at an even spacing
// within the chart, and "now" follows the chart.
func TestAxisLabelsEvenAndApart(t *testing.T) {
	for _, lv := range loaders.Levels {
		for w := 40; w <= 200; w++ {
			cells := services.ChartCells(w)
			got := services.Axis(lv.Step, cells)
			chart, rest, ok := strings.Cut(got, "  now")
			if !ok || rest != "" || ansi.StringWidth(chart) != cells {
				t.Fatalf("%s %d: want %d chart cells then \"  now\": %q", lv.Name, cells, cells, got)
			}
			var centres []int
			for _, m := range regexp.MustCompile(`\S+`).FindAllStringIndex(chart, -1) {
				tok := chart[m[0]:m[1]]
				if !axisLabel.MatchString(tok) {
					t.Errorf("%s %d: labels run together %q in %q", lv.Name, cells, tok, got)
				}
				centres = append(centres, m[0]+(len(tok)-1)/2)
			}
			for i, c := range centres {
				if (cells-1-c)%5 != 0 {
					t.Errorf("%s %d: label %d off the 5-cell grid: %q", lv.Name, cells, i, got)
				}
				if i > 1 && c-centres[i-1] != centres[1]-centres[0] {
					t.Errorf("%s %d: uneven label spacing: %q", lv.Name, cells, got)
				}
			}
		}
	}
}

// The panel draws the axis under the charts in two-line mode, from the
// chart's first cell, and shows two deployments to make room; in one-line
// mode there is no axis and three deployments.
func TestPanelAxisLine(t *testing.T) {
	c := panelCtx(150, panelHeight)
	out := ansi.Strip(body(c, services.New()))
	lines := strings.Split(out, "\n")
	cells := services.ChartCells(150 - tui.ChromeCols)
	want := strings.Repeat(" ", 9) + services.Axis(60, cells)
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, "net ") {
			if i+2 >= len(lines) || strings.TrimRight(lines[i+2], " ") != strings.TrimRight(want, " ") {
				t.Errorf("axis line = %q\nwant %q", lines[min(i+2, len(lines)-1)], want)
			}
			// "now" lines up with the captions' "now", two blanks past the chart
			if i+2 < len(lines) {
				at := strings.LastIndex(lines[i+2], "now")
				capAt := strings.Index(lines[i], "now")
				if at < 0 || ansi.StringWidth(lines[i+2][:at]) != 9+cells+2 || ansi.StringWidth(lines[i+2][:at]) != ansi.StringWidth(lines[i][:capAt]) {
					t.Errorf("axis now is not under the captions' now (%d): %q", 9+cells+2, lines[i+2])
				}
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no net chart:\n%s", out)
	}
	if strings.Contains(out, "○ deploy") || !strings.Contains(out, "✗ redeploy") {
		t.Errorf("two deployments in two-line mode:\n%s", out)
	}

	c = panelCtx(60, panelHeight)
	out = ansi.Strip(body(c, services.New()))
	for _, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(strings.TrimRight(l, " "), " now") {
			t.Errorf("one-line mode draws an axis: %q", l)
		}
	}
	if !strings.Contains(out, "○ deploy") {
		t.Errorf("three deployments in one-line mode:\n%s", out)
	}
}

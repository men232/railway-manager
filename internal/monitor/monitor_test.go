package monitor

import (
	"math"
	"slices"
	"testing"

	"github.com/men232/railway-manager/internal/railway"
)

func TestStatusOf(t *testing.T) {
	ds := func(active, latest string, inProgress int) railway.DeploymentState {
		s := railway.DeploymentState{ActiveStatus: active, LatestStatus: latest}
		for range inProgress {
			s.InProgress = append(s.InProgress, "d")
		}
		return s
	}
	tests := []struct {
		name  string
		st    railway.DeploymentState
		read  bool
		kind  Kind
		label string
	}{
		{"not read", ds("", "", 0), false, Reading, "… reading"},
		{"online", ds("SUCCESS", "SUCCESS", 0), true, Online, "● ONLINE"},
		{"online deploying", ds("SUCCESS", "SUCCESS", 1), true, OnlineDeploying, "◐ ONLINE / DEPLOYING"},
		{"newer failed keeps online", ds("SUCCESS", "FAILED", 0), true, Online, "● ONLINE"},
		{"deploying", ds("", "", 1), true, Deploying, "◌ DEPLOYING"},
		{"latest failed", ds("", "FAILED", 0), true, Failed, "✗ OFFLINE / FAILED"},
		{"latest crashed", ds("", "CRASHED", 0), true, Failed, "✗ OFFLINE / FAILED"},
		{"latest removed", ds("", "REMOVED", 0), true, Failed, "✗ OFFLINE / FAILED"},
		{"active crashed", ds("CRASHED", "CRASHED", 0), true, Failed, "✗ OFFLINE / FAILED"},
		{"offline", ds("", "", 0), true, Offline, "○ OFFLINE"},
		{"sleeping status", ds("SLEEPING", "SLEEPING", 0), true, Sleeping, "☾ SLEEPING"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StatusOf(tt.st, tt.read)
			if got.Kind != tt.kind || got.Label != tt.label || got.Icon+" "+got.Text != tt.label || got.Icon == "" || got.Text == "" {
				t.Errorf("got %+v, want %v %q", got, tt.kind, tt.label)
			}
		})
	}
}

func str(lines [][]Cell) []string {
	var out []string
	for _, l := range lines {
		var rs []rune
		for _, c := range l {
			rs = append(rs, c.R)
		}
		out = append(out, string(rs))
	}
	return out
}

func TestBrailleFillsFromBottom(t *testing.T) {
	// peak 4: 1 -> one dot row, 2 -> two, 4 -> full; two samples per cell.
	got := str(Braille([]float64{1, 1, 2, 2, 4, 4}, 3, 1, 4, 4))
	want := []string{"⣀⣤⣿"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// left column only: bottom-left dot is 0x40.
	if got := str(Braille([]float64{1, 0}, 1, 1, 4, 4)); !slices.Equal(got, []string{"⡀"}) {
		t.Errorf("left only: %v", got)
	}
}

func TestBrailleOwnPeak(t *testing.T) {
	cells := Braille([]float64{0.4, 0.4}, 1, 1, 0.4, 32)
	if cells[0][0].R != '⣿' {
		t.Errorf("rune %q, want full", cells[0][0].R)
	}
	if l := cells[0][0].Level; math.Abs(l-0.0125) > 1e-9 {
		t.Errorf("level %v, want 0.0125", l)
	}
}

func TestBrailleLevelUsesHigherValueAndClamps(t *testing.T) {
	c := Braille([]float64{1, 2}, 1, 1, 2, 4)[0][0]
	if c.Level != 0.5 {
		t.Errorf("level %v, want 0.5", c.Level)
	}
	if c := Braille([]float64{9, 1}, 1, 1, 9, 3)[0][0]; c.Level != 1 {
		t.Errorf("clamp: %v", c.Level)
	}
	if c := Braille([]float64{1, 2}, 1, 1, 2, 0)[0][0]; c.Level != 1 {
		t.Errorf("limit 0 uses peak: %v", c.Level)
	}
}

func TestBrailleTwoRows(t *testing.T) {
	// 8 dot rows; a value at half the peak fills the bottom line, the top is blank.
	got := str(Braille([]float64{2, 2, 4, 4, 3, 3}, 3, 2, 4, 4))
	want := []string{"⠀⣿⣤", "⣿⣿⣿"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBrailleTakesNewestThatFit(t *testing.T) {
	vals := make([]float64, 100)
	for i := range vals {
		vals[i] = float64(i + 1) // rising
	}
	cells := Braille(vals, 5, 1, 100, 100)
	if len(cells[0]) != 5 {
		t.Fatalf("width %d", len(cells[0]))
	}
	// newest 10 are 91..100: last cell is the highest, levels are 0.92.. .
	if l := cells[0][4].Level; l != 1 {
		t.Errorf("last level %v, want 1", l)
	}
	if l := cells[0][0].Level; math.Abs(l-0.92) > 1e-9 {
		t.Errorf("first level %v, want 0.92 (value 92)", l)
	}
}

func TestBrailleShortSeriesPadsLeft(t *testing.T) {
	got := str(Braille([]float64{4, 4}, 3, 1, 4, 4))
	if !slices.Equal(got, []string{"⠀⠀⣿"}) {
		t.Errorf("got %v", got)
	}
}

func TestChartEmptySeries(t *testing.T) {
	for _, c := range []struct {
		vals []float64
		peak float64
	}{{nil, 0}, {nil, 5}, {[]float64{1, 2}, 0}, {[]float64{0, 0}, 0}} {
		got := str(Braille(c.vals, 3, 2, c.peak, 0))
		if !slices.Equal(got, []string{"⠀⠀⠀", "⠀⠀⠀"}) {
			t.Errorf("%v peak %v: %v", c.vals, c.peak, got)
		}
	}
	if got := Braille(nil, 0, 1, 0, 0); len(got) != 1 || len(got[0]) != 0 {
		t.Errorf("zero width: %v", got)
	}
}

func TestRateUsesGap(t *testing.T) {
	const gb = 1.0 / (1024 * 1024) // 1 KB in GB
	rx := []railway.MetricPoint{{TS: 100, Value: 30 * gb}, {TS: 130, Value: 30 * gb}, {TS: 190, Value: 60 * gb}}
	tx := []railway.MetricPoint{{TS: 100, Value: 0}, {TS: 130, Value: 0}, {TS: 190, Value: 60 * gb}}
	got := Rate(rx, tx)
	// first gap is the 30 s sample rate; then 30 s; then 60 s with rx+tx = 120 KB.
	want := []float64{1024, 1024, 2048}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-6 {
			t.Errorf("[%d] %v, want %v", i, got[i], want[i])
		}
	}
	if got := Rate(nil, nil); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
	// duplicate stamps are one stamp: rx and tx sum there.
	same := []railway.MetricPoint{{TS: 5, Value: 1}, {TS: 5, Value: 1}}
	if got := Rate(same, nil); len(got) != 1 {
		t.Errorf("same stamp: %v", got)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestRateUnionOfStamps(t *testing.T) {
	const kb = 1.0 / (1024 * 1024)
	// tx misses the stamp 130: rx alone counts there, gaps follow the union.
	rx := []railway.MetricPoint{{TS: 100, Value: 30 * kb}, {TS: 130, Value: 30 * kb}}
	tx := []railway.MetricPoint{{TS: 100, Value: 30 * kb}, {TS: 160, Value: 60 * kb}}
	got := Rate(rx, tx)
	want := []float64{2048, 1024, 2048}
	if len(got) != 3 || !near(got[0], want[0]) || !near(got[1], want[1]) || !near(got[2], want[2]) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRateEmptyTxStillShowsRx(t *testing.T) {
	const kb = 1.0 / (1024 * 1024)
	got := Rate([]railway.MetricPoint{{TS: 0, Value: 30 * kb}, {TS: 60, Value: 60 * kb}}, nil)
	if len(got) != 2 || !near(got[0], 1024) || !near(got[1], 1024) {
		t.Errorf("got %v", got)
	}
}

func TestRateOutOfOrder(t *testing.T) {
	const kb = 1.0 / (1024 * 1024)
	rx := []railway.MetricPoint{{TS: 160, Value: 60 * kb}, {TS: 100, Value: 30 * kb}}
	tx := []railway.MetricPoint{{TS: 130, Value: 30 * kb}}
	got := Rate(rx, tx)
	if len(got) != 3 || !near(got[0], 1024) || !near(got[1], 1024) || !near(got[2], 2048) {
		t.Errorf("got %v", got)
	}
}

func TestNewestRates(t *testing.T) {
	const kb = 1.0 / (1024 * 1024)
	rx := []railway.MetricPoint{{TS: 160, Value: 60 * kb}, {TS: 100, Value: 1}, {TS: 130, Value: 1}}
	tx := []railway.MetricPoint{{TS: 90, Value: 30 * kb}}
	r, x, ok := NewestRates(rx, tx)
	if !ok || !near(r, 2048) || !near(x, 1024) {
		t.Errorf("got %v %v %v", r, x, ok)
	}
	r, x, ok = NewestRates(nil, tx)
	if !ok || r != 0 || !near(x, 1024) {
		t.Errorf("rx empty: %v %v %v", r, x, ok)
	}
	if _, _, ok := NewestRates(nil, nil); ok {
		t.Error("both empty must not be ok")
	}
}

func TestBrailleHostileInput(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	// NaN and negative values draw nothing and never panic.
	got := str(Braille([]float64{nan, -3, 4, nan}, 2, 1, 4, 4))
	if !slices.Equal(got, []string{"⠀⡇"}) {
		t.Errorf("got %v", got)
	}
	// NaN or negative limit: colour against the peak.
	for _, lim := range []float64{nan, -1, 0} {
		if l := Braille([]float64{2, 1}, 1, 1, 4, lim)[0][0].Level; l != 0.5 {
			t.Errorf("limit %v: level %v, want 0.5", lim, l)
		}
	}
	// NaN peak or limit with NaN values: blank, level 0.
	c := Braille([]float64{1, 2}, 1, 1, nan, nan)[0][0]
	if c.R != blank || c.Level != 0 {
		t.Errorf("nan peak: %+v", c)
	}
	// infinite value fills and clamps.
	if c := Braille([]float64{inf, 0}, 1, 1, 4, 4)[0][0]; c.Level != 1 {
		t.Errorf("inf: %+v", c)
	}
	// rows < 1 is one row; negative width is none.
	if g := Braille([]float64{4, 4}, 1, 0, 4, 4); len(g) != 1 || g[0][0].R != '⣿' {
		t.Errorf("rows 0: %v", g)
	}
	if g := Braille([]float64{4, 4}, -2, -1, 4, 4); len(g) != 1 || len(g[0]) != 0 {
		t.Errorf("negative: %v", g)
	}
}

func TestLayoutWidths(t *testing.T) {
	tests := []struct {
		width    int
		chart    int
		iconOnly bool
		pro, env int
		name     int
	}{
		{150, 9, false, 16, 12, 28},
		// charts never squeeze project, env or name: with the full names,
		// 127 is the narrowest width that gives 8 cells (status icon only)
		{127, 8, true, 16, 12, 28},
		// below 8 chart cells the charts go; env takes the room first,
		// then the status label (it needs 19 more), then name and project
		{126, 0, false, 16, 12, 28},
		{120, 0, false, 16, 12, 28},
		{95, 0, true, 11, 12, 28},
		{94, 0, true, 10, 12, 28},
		{80, 0, true, 7, 12, 17},
	}
	for _, tt := range tests {
		c := Layout(tt.width, 40, 40, 40)
		if c.Chart != tt.chart || c.StatusIconOnly != tt.iconOnly || c.Project != tt.pro || c.Env != tt.env || c.Name != tt.name {
			t.Errorf("width %d: %+v", tt.width, c)
		}
		if c.StatusIconOnly != (c.Status == 1) || (!c.StatusIconOnly && c.Status != 20) {
			t.Errorf("width %d: status %d", tt.width, c.Status)
		}
	}
}

// Charts never squeeze project, env or name below their content width: when
// charts are drawn, all three are whole.
func TestLayoutChartsKeepFullNames(t *testing.T) {
	for w := 0; w <= 200; w++ {
		for _, n := range [][3]int{{1, 1, 1}, {7, 10, 6}, {9, 3, 4}, {40, 40, 40}} {
			c := Layout(w, n[0], n[1], n[2])
			full := Layout(1000, n[0], n[1], n[2])
			if c.Chart > 0 && (c.Project != full.Project || c.Env != full.Env || c.Name != full.Name) {
				t.Errorf("Layout(%d, %v) = %+v, names below %+v", w, n, c, full)
			}
		}
	}
}

// A chart narrower than 8 cells is never drawn: at every width the charts
// are 0 or at least 8, and the freed room goes to env, then the status label.
func TestLayoutChartMinimum(t *testing.T) {
	for w := 0; w <= 200; w++ {
		for _, n := range []int{1, 10, 40} {
			if c := Layout(w, n, n, n); c.Chart > 0 && c.Chart < 8 {
				t.Errorf("Layout(%d, %d…) chart = %d", w, n, c.Chart)
			}
		}
	}
	// the charts never shrink env 10: 94 is the narrowest width with charts;
	// below it they go and env is whole, with the status label once it fits
	if c := Layout(94, 7, 10, 6); c.Chart != 8 || c.Env != 10 || !c.StatusIconOnly {
		t.Errorf("94: %+v", c)
	}
	if c := Layout(93, 7, 10, 6); c.Chart != 0 || c.Env != 10 || c.StatusIconOnly {
		t.Errorf("93: %+v", c)
	}
	if c := Layout(86, 7, 10, 6); c.Chart != 0 || c.Env != 10 || c.StatusIconOnly || c.Status != 20 {
		t.Errorf("86: %+v", c)
	}
	if c := Layout(67, 7, 10, 6); c.Chart != 0 || c.Env != 10 || !c.StatusIconOnly {
		t.Errorf("67 (80 columns): %+v", c)
	}
}

func TestLayoutFollowsContentAndNeverNegative(t *testing.T) {
	c := Layout(150, 9, 3, 4)
	if c.Project != 9 || c.Env != 3 || c.Name != 4 {
		t.Errorf("content widths: %+v", c)
	}
	c = Layout(150, 1, 1, 1) // titles are the floor
	if c.Project != 7 || c.Env != 3 || c.Name != 4 {
		t.Errorf("title floor: %+v", c)
	}
	if c := Layout(40, 40, 40, 40); c.Chart != 0 {
		t.Errorf("no room: %+v", c)
	}
	if c := Layout(0, 1, 1, 1); c.Chart != 0 {
		t.Errorf("zero width: %+v", c)
	}
}

func TestFormatBytes(t *testing.T) {
	const mb = 1024 * 1024
	for _, tt := range []struct {
		in   float64
		want string
	}{
		{0, "0.0 B"}, {512, "512.0 B"}, {1536, "1.5 KB"},
		{1023.9 * mb, "1023.9 MB"}, {1023.99 * mb, "1.0 GB"},
		{1024 * mb, "1.0 GB"}, {2.5 * 1024 * mb, "2.5 GB"}, {2048 * 1024 * mb, "2048.0 GB"},
	} {
		if got := FormatBytes(tt.in); got != tt.want {
			t.Errorf("FormatBytes(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatRate(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want string
	}{{0, "0.0 B/s"}, {1024, "1.0 KB/s"}, {1023.9 * 1024, "1023.9 KB/s"}, {1024 * 1024, "1.0 MB/s"}} {
		if got := FormatRate(tt.in); got != tt.want {
			t.Errorf("FormatRate(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatCPU(t *testing.T) {
	for in, want := range map[float64]string{0.25: "0.25", 32: "32.00", 0: "0.00", 0.004: "0.00"} {
		if got := FormatCPU(in); got != want {
			t.Errorf("FormatCPU(%v) = %q, want %q", in, got, want)
		}
	}
}

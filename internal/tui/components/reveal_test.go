package components_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
)

func TestRevealPanel(t *testing.T) {
	th := tui.NewTheme(termenv.Ascii)
	p := components.RevealPanel(th, "TOKEN · production · api · 12 chars", "abcdefghijkl", 30)
	if len(p) != 3 {
		t.Fatalf("lines = %d:\n%s", len(p), strings.Join(p, "\n"))
	}
	for _, l := range p {
		if w := ansi.StringWidth(l); w != 30 {
			t.Errorf("%q is %d wide, want 30", l, w)
		}
	}
	if !strings.HasPrefix(p[0], "╭─ TOKEN") || !strings.HasSuffix(p[0], " v hides ─╮") || !strings.Contains(p[1], "abcdefghijkl") {
		t.Errorf("panel:\n%s", strings.Join(p, "\n"))
	}
	if components.RevealPanel(th, "T", "v", 11) != nil {
		t.Error("a panel too narrow for a value is drawn")
	}
}

// A long value wraps; past RevealLines the rest is counted, not drawn.
func TestRevealPanelLong(t *testing.T) {
	th := tui.NewTheme(termenv.Ascii)
	v := strings.Repeat("x", 16*10) // 10 lines of 16
	p := components.RevealPanel(th, "T", v, 20)
	if len(p) != components.RevealLines+2 {
		t.Fatalf("lines = %d", len(p))
	}
	if last := p[len(p)-2]; !strings.Contains(last, "… 80 more chars") {
		t.Errorf("last value line = %q", last)
	}
}

func TestOverlay(t *testing.T) {
	lines := []string{"0", "1", "2", "3", "4"}
	panel := []string{"A", "B"}
	if got := components.Overlay(lines, panel, 0); !slices.Equal(got, []string{"0", "1", "2", "A", "B"}) {
		t.Errorf("bottom: %v", got)
	}
	if got := components.Overlay(lines, panel, 4); !slices.Equal(got, []string{"A", "B", "2", "3", "4"}) {
		t.Errorf("cursor under the panel: %v", got)
	}
	if !slices.Equal(lines, []string{"0", "1", "2", "3", "4"}) {
		t.Error("Overlay changed its input")
	}
	if got := components.Overlay([]string{"0"}, panel, 0); !slices.Equal(got, []string{"A"}) {
		t.Errorf("taller panel: %v", got)
	}
}

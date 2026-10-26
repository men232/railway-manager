// Package tui holds the shared TUI foundation: theme, icons, frame, footer,
// the screen contract and the intent messages screens send to the root model.
package tui

import (
	"fmt"
	"io"
	"math"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Theme is the palette. Every style is built for one color profile; screens
// never pick colors themselves.
type Theme struct {
	// Renderer builds every style below; screens that need more styles should
	// call Renderer.NewStyle so they follow the same profile.
	Renderer                                                               *lipgloss.Renderer
	Accent, Accent2, Muted, Faint, New, Danger, Warn, OK, Selected, Border lipgloss.Style
	// Levels are the chart gradient, OK through Warn to Danger in LevelSteps
	// steps (index 0..LevelSteps); Level picks one.
	Levels [LevelSteps + 1]lipgloss.Style
	// Letters colour the value letters a, b, c… in turn (cyan, magenta,
	// yellow, green).
	Letters [4]lipgloss.Style
	// Tags colour the Rotate row tags, one per kind of row.
	Tags Tags
	// profile is the colour profile the styles were built for.
	profile termenv.Profile
}

// logoStops are the logo gradient: green, cyan, blue, as the app icon.
var logoStops = [3][3]float64{{0x5f, 0xd7, 0x87}, {0x00, 0xd7, 0xff}, {0x00, 0xaf, 0xff}}

// Logo is the style of the logo at position t (0..1) along its width: a
// green to cyan to blue gradient, the accent on an 8-colour terminal, bold
// alone without colour.
func (t Theme) Logo(pos float64) lipgloss.Style {
	switch t.profile {
	case termenv.Ascii:
		return t.Renderer.NewStyle().Bold(true)
	case termenv.ANSI:
		return t.Accent
	}
	from, to, f := logoStops[0], logoStops[1], pos*2
	if pos > 0.5 {
		from, to, f = logoStops[1], logoStops[2], (pos-0.5)*2
	}
	var c [3]int
	for i := range c {
		c[i] = int(math.Round(from[i] + (to[i]-from[i])*min(max(f, 0), 1)))
	}
	return t.Renderer.NewStyle().Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2]))).Bold(true)
}

// Tags are the styles of the Rotate row tags.
type Tags struct {
	// Gen: generated values; Ask: values the user pastes; New: added
	// variables; DB, User, Key: a database's root password, another
	// user's password, a Mongo KEYFILE.
	Gen, Ask, New, DB, User, Key lipgloss.Style
}

// LevelSteps is how many steps the chart gradient has.
const LevelSteps = 8

// Level is the gradient style of a level 0..1, rounded to the nearest step.
func (t Theme) Level(level float64) lipgloss.Style { return t.Levels[LevelStep(level)] }

// LevelStep is the gradient step (0..LevelSteps) of a level 0..1, rounded to
// the nearest; below 0 or NaN is 0, above 1 is LevelSteps.
func LevelStep(level float64) int {
	if !(level > 0) {
		return 0
	}
	return int(math.Round(min(1, level) * LevelSteps))
}

// gradient colours of the OK (78), Warn (214) and Danger (203) tones.
var gradientStops = [3][3]float64{{0x5f, 0xd7, 0x87}, {0xff, 0xaf, 0x00}, {0xff, 0x5f, 0x5f}}

func gradientHex(t float64) string {
	from, to, f := gradientStops[0], gradientStops[1], t*2
	if t > 0.5 {
		from, to, f = gradientStops[1], gradientStops[2], (t-0.5)*2
	}
	var c [3]int
	for i := range c {
		c[i] = int(math.Round(from[i] + (to[i]-from[i])*f))
	}
	return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2])
}

// NewTheme builds the palette for a color profile. 256 colors and above get an
// explicit black-on-cyan selection; 8-color terminals get the standard colors
// and reverse video; Ascii (NO_COLOR) gets no colors at all, only bold, faint
// and reverse video, so the selection stays visible.
//
// The theme owns its renderer, so the global lipgloss profile does not matter.
// Ascii maps to the ANSI profile with no colors set: NO_COLOR forbids color,
// not attributes, and termenv would strip reverse and bold under Ascii.
func NewTheme(profile termenv.Profile) Theme {
	r := lipgloss.NewRenderer(io.Discard)
	if profile == termenv.Ascii {
		r.SetColorProfile(termenv.ANSI)
	} else {
		r.SetColorProfile(profile)
	}
	fg := func(full, basic string) lipgloss.Style {
		switch profile {
		case termenv.Ascii:
			return r.NewStyle()
		case termenv.ANSI:
			return r.NewStyle().Foreground(lipgloss.Color(basic))
		}
		return r.NewStyle().Foreground(lipgloss.Color(full))
	}
	t := Theme{
		Renderer: r,
		profile:  profile,
		Accent:   fg("45", "6").Bold(true),
		Accent2:  fg("39", "4"),
		Muted:    fg("245", "7"),
		Faint:    fg("240", "8").Faint(true),
		New:      fg("114", "2"),
		Danger:   fg("203", "1").Bold(true),
		Warn:     fg("214", "3"),
		OK:       fg("78", "2"),
	}
	t.Border = t.Accent2
	t.Letters = [4]lipgloss.Style{fg("51", "6"), fg("207", "5"), fg("226", "3"), fg("120", "2")}
	t.Tags = Tags{
		Gen: fg("114", "2"), Ask: fg("221", "3"), New: fg("45", "6"),
		DB: fg("39", "4"), User: fg("141", "5"), Key: fg("207", "5"),
	}
	for i := range t.Levels {
		switch profile {
		case termenv.Ascii:
			t.Levels[i] = r.NewStyle()
		case termenv.ANSI:
			t.Levels[i] = r.NewStyle().Foreground(lipgloss.Color([]string{"2", "3", "1"}[min(2, i*3/(LevelSteps+1))]))
		default:
			t.Levels[i] = r.NewStyle().Foreground(lipgloss.Color(gradientHex(float64(i) / LevelSteps)))
		}
	}
	switch profile {
	case termenv.Ascii, termenv.ANSI:
		t.Selected = r.NewStyle().Reverse(true).Bold(true)
	default:
		t.Selected = r.NewStyle().Foreground(lipgloss.Color("16")).Background(lipgloss.Color("45")).Bold(true)
	}
	return t
}

package main

import (
	"fmt"
	"strconv"
	"strings"
)

// style is the SGR state of one cell.
type style struct {
	fg, bg                             string
	bold, faint, italic, under, invert bool
}

type cell struct {
	r  rune
	st style
}

// colors resolves reverse video: the cell's text and background colors, ""
// meaning the terminal's default.
func (s style) colors() (fg, bg string) {
	fg, bg = s.fg, s.bg
	if s.invert {
		fg, bg = bg, fg
		if fg == "" {
			fg = termBG
		}
		if bg == "" {
			bg = defFG
		}
	}
	return fg, bg
}

// css is the text style of a cell; backgrounds are drawn separately.
func (s style) css() string {
	fg, _ := s.colors()
	var b strings.Builder
	if fg != "" {
		b.WriteString("color:" + fg + ";")
	}
	if s.bold {
		b.WriteString("font-weight:700;")
	}
	if s.faint {
		b.WriteString("opacity:.55;")
	}
	if s.italic {
		b.WriteString("font-style:italic;")
	}
	if s.under {
		b.WriteString("text-decoration:underline;")
	}
	return b.String()
}

// parse splits ANSI text into lines of styled cells. Only SGR sequences are
// understood; other CSI sequences are dropped.
func parse(s string) [][]cell {
	var lines [][]cell
	var cur []cell
	var st style
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b && i+1 < len(rs) && rs[i+1] == '[':
			j := i + 2
			for j < len(rs) && (rs[j] < 0x40 || rs[j] > 0x7e) {
				j++
			}
			if j < len(rs) && rs[j] == 'm' {
				st = sgr(st, string(rs[i+2:j]))
			}
			i = j
		case r == 0x1b && i+1 < len(rs) && rs[i+1] == ']':
			// OSC (hyperlinks): skip to BEL or ESC \.
			j := i + 2
			for j < len(rs) && rs[j] != 0x07 && !(rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\') {
				j++
			}
			if j < len(rs) && rs[j] == 0x1b {
				j++
			}
			i = j
		case r == '\n':
			lines = append(lines, cur)
			cur = nil
		case r == '\r':
		default:
			cur = append(cur, cell{r, st})
		}
	}
	return append(lines, cur)
}

func sgr(st style, params string) style {
	if params == "" {
		return style{}
	}
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		n, _ := strconv.Atoi(ps[i])
		switch {
		case n == 0:
			st = style{}
		case n == 1:
			st.bold = true
		case n == 2:
			st.faint = true
		case n == 3:
			st.italic = true
		case n == 4:
			st.under = true
		case n == 7:
			st.invert = true
		case n == 22:
			st.bold, st.faint = false, false
		case n == 23:
			st.italic = false
		case n == 24:
			st.under = false
		case n == 27:
			st.invert = false
		case n == 39:
			st.fg = ""
		case n == 49:
			st.bg = ""
		case n >= 30 && n <= 37:
			st.fg = xterm(n - 30)
		case n >= 90 && n <= 97:
			st.fg = xterm(n - 90 + 8)
		case n >= 40 && n <= 47:
			st.bg = xterm(n - 40)
		case n >= 100 && n <= 107:
			st.bg = xterm(n - 100 + 8)
		case (n == 38 || n == 48) && i+1 < len(ps):
			var c string
			if ps[i+1] == "5" && i+2 < len(ps) {
				k, _ := strconv.Atoi(ps[i+2])
				c, i = xterm(k), i+2
			} else if ps[i+1] == "2" && i+4 < len(ps) {
				r, _ := strconv.Atoi(ps[i+2])
				g, _ := strconv.Atoi(ps[i+3])
				b, _ := strconv.Atoi(ps[i+4])
				c, i = fmt.Sprintf("#%02x%02x%02x", r, g, b), i+4
			}
			if n == 38 {
				st.fg = c
			} else {
				st.bg = c
			}
		}
	}
	return st
}

// base16 is a dark terminal palette for the 16 standard colors.
var base16 = [16]string{
	"#1b2232", "#ff6b6b", "#5fd787", "#ffd166", "#4aa8ff", "#d38cff", "#00d7ff", "#c9d4e5",
	"#5d6b85", "#ff8787", "#87ffaf", "#ffe08a", "#7cc0ff", "#e2b0ff", "#5ff0ff", "#ffffff",
}

// xterm is the hex color of a 256-color index.
func xterm(n int) string {
	switch {
	case n < 16:
		return base16[max(n, 0)]
	case n < 232:
		n -= 16
		lv := [6]int{0, 95, 135, 175, 215, 255}
		return fmt.Sprintf("#%02x%02x%02x", lv[n/36], lv[n/6%6], lv[n%6])
	case n < 256:
		v := 8 + 10*(n-232)
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
	return ""
}

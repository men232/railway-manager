package monitor

import "math"

// Cell is one terminal cell of a chart: a braille rune and its colour level.
type Cell struct {
	R     rune
	Level float64 // 0..1: the cell's higher value against the limit
}

const blank = 0x2800

// Dot bits of the left and right columns, bottom row first.
var (
	leftBits  = [4]rune{0x40, 0x04, 0x02, 0x01}
	rightBits = [4]rune{0x80, 0x20, 0x10, 0x08}
)

// Braille draws an area chart `rows` lines high (four dot rows each), two
// points per cell, filled from the bottom, the newest width*2 values with the
// newest on the right. Height is value/peak (the series' own peak); Level is
// the cell's higher value over limit (limit 0: peak), clamped to 0..1.
// Lines are returned top first. Empty input or peak 0 gives blank cells.
func Braille(values []float64, width, rows int, peak, limit float64) [][]Cell {
	if width < 0 {
		width = 0
	}
	if rows < 1 {
		rows = 1
	}
	slots := width * 2
	if len(values) > slots {
		values = values[len(values)-slots:]
	}
	padded := make([]float64, slots)
	copy(padded[slots-len(values):], values)
	scale := limit
	if !(scale > 0) {
		scale = peak
	}
	total := rows * 4
	dots := func(v float64) int {
		if !(peak > 0) || !(v > 0) {
			return 0
		}
		h := int(math.Ceil(min(1, v/peak) * float64(total)))
		return max(0, min(total, h))
	}
	out := make([][]Cell, rows)
	for l := range out {
		out[l] = make([]Cell, width)
	}
	for c := 0; c < width; c++ {
		lv, rv := padded[2*c], padded[2*c+1]
		lh, rh := dots(lv), dots(rv)
		level := 0.0
		if hi := max(lv, rv); scale > 0 && hi > 0 {
			level = min(1, hi/scale)
		}
		for l := 0; l < rows; l++ {
			k := rows - 1 - l // line index from the bottom
			bits := rune(0)
			for d := 0; d < 4; d++ {
				if lh > k*4+d {
					bits |= leftBits[d]
				}
				if rh > k*4+d {
					bits |= rightBits[d]
				}
			}
			out[l][c] = Cell{R: blank + bits, Level: level}
		}
	}
	return out
}

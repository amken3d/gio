// SPDX-License-Identifier: Unlicense OR MIT

// Package scan provides a sparse anti-aliased polygon rasterizer.
//
// This is a clean-room implementation of the signed area/coverage
// accumulation algorithm ("cl-aa") used by Anti-Grain Geometry and
// described at
// http://projects.tuxee.net/cl-vectors/section-the-cl-aa-algorithm.
// It was written from the published algorithm description only and
// shares no code with the FreeType smooth module or its derivatives;
// the exported API shape is kept source-compatible with the scanner
// previously vendored here.
//
// Model: closed contours are fed as line segments in 26.6 fixed point.
// Each segment deposits, into every pixel cell it crosses, a signed
// cover (the vertical extent dy it spans within the cell, in subpixel
// units) and a signed area term dy*(fx0+fx1) encoding where within the
// cell it crosses. A left-to-right sweep per scanline then integrates
// cover to produce alpha spans: cells containing edges get
// cover*2*64 - area, gaps between cells get the running cover*2*64.
package scan

import (
	"golang.org/x/image/math/fixed"
)

// SpanFunc consumes one horizontal run of pixels [xi0, xi1) on row yi
// with 16-bit coverage alpha (0..0xffff).
type SpanFunc func(yi, xi0, xi1 int, alpha uint32)

// Spanner is the output sink of the Scanner.
type Spanner interface {
	SetColor(color interface{})
	GetSpanFunc() SpanFunc
}

const (
	subShift = 6                   // 26.6 input coordinates
	subOne   = 1 << subShift       // 64 subpixels per pixel
	subMask  = subOne - 1          //
	covFull  = 2 * subOne * subOne // coverage units of a fully covered cell (8192)
)

// cell accumulates edge contributions for one pixel. Cells for a row
// form a singly linked, x-sorted list through the Scanner's pool.
type cell struct {
	x     int32
	cover int32
	area  int32
	next  int32 // pool index, -1 terminates
}

// Scanner rasterizes closed polygonal contours into coverage spans.
type Scanner struct {
	spanner Spanner
	w, h    int
	maxX    int32 // w << subShift
	maxY    int32 // h << subShift
	winding bool  // true: non-zero fill rule, false: even-odd

	cells   []cell
	rowHead []int32 // per-row list head, -1 empty

	// open accumulator for the cell currently being written, so runs
	// of sub-segments in one cell cost no list traffic
	haveCell     bool
	cellX, cellY int32
	cover, area  int32

	// pen state
	penX, penY     int32
	startX, startY int32
	open           bool
}

// NewScanner returns a Scanner emitting spans into sp, sized for a
// w x h pixel target. The fill rule defaults to non-zero winding.
func NewScanner(sp Spanner, w, h int) *Scanner {
	s := &Scanner{spanner: sp, winding: true}
	s.SetBounds(w, h)
	return s
}

// SetBounds resizes the scan target and discards accumulated state.
func (s *Scanner) SetBounds(w, h int) {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	s.w, s.h = w, h
	s.maxX = int32(w) << subShift
	s.maxY = int32(h) << subShift
	if cap(s.rowHead) < h {
		s.rowHead = make([]int32, h)
	}
	s.rowHead = s.rowHead[:h]
	s.Clear()
}

// SetWinding selects the fill rule: true for non-zero winding, false
// for even-odd.
func (s *Scanner) SetWinding(useNonZeroWinding bool) {
	s.winding = useNonZeroWinding
}

// Clear discards all accumulated cells, keeping capacity and bounds.
func (s *Scanner) Clear() {
	s.cells = s.cells[:0]
	for i := range s.rowHead {
		s.rowHead[i] = -1
	}
	s.haveCell = false
	s.open = false
}

// Start begins a new contour at a, closing any contour left open.
func (s *Scanner) Start(a fixed.Point26_6) {
	s.closeContour()
	s.penX, s.penY = int32(a.X), int32(a.Y)
	s.startX, s.startY = s.penX, s.penY
}

// Line adds a segment from the pen position to b.
func (s *Scanner) Line(b fixed.Point26_6) {
	bx, by := int32(b.X), int32(b.Y)
	s.renderLine(s.penX, s.penY, bx, by)
	s.penX, s.penY = bx, by
	s.open = true
}

// Draw sweeps the accumulated cells and emits coverage spans. It does
// not clear them; call Clear before reuse.
func (s *Scanner) Draw() {
	s.closeContour()
	s.flushCell()
	span := s.spanner.GetSpanFunc()

	for y := 0; y < s.h; y++ {
		ci := s.rowHead[y]
		if ci < 0 {
			continue
		}
		var cover int32
		for ci >= 0 {
			c := s.cells[ci]
			cover += c.cover
			x := int(c.x)

			if a := s.alphaOf(cover<<(subShift+1) - c.area); a > 0 && x < s.w {
				span(y, x, x+1, a)
			}

			ci = c.next
			nextX := s.w
			if ci >= 0 && int(s.cells[ci].x) < nextX {
				nextX = int(s.cells[ci].x)
			}
			if nextX > x+1 {
				if a := s.alphaOf(cover << (subShift + 1)); a > 0 {
					span(y, x+1, nextX, a)
				}
			}
		}
	}
}

// closeContour adds the implicit closing segment of an open contour.
func (s *Scanner) closeContour() {
	if !s.open {
		return
	}
	s.open = false
	if s.penX != s.startX || s.penY != s.startY {
		s.renderLine(s.penX, s.penY, s.startX, s.startY)
		s.penX, s.penY = s.startX, s.startY
	}
}

// alphaOf maps accumulated coverage units (covFull = fully covered)
// to 16-bit alpha under the active fill rule.
func (s *Scanner) alphaOf(c int32) uint32 {
	if c < 0 {
		c = -c
	}
	if s.winding {
		if c > covFull {
			c = covFull
		}
	} else {
		c &= 2*covFull - 1
		if c > covFull {
			c = 2*covFull - c
		}
	}
	return uint32(c) * 0xffff >> 13
}

// renderLine clips a segment vertically to the target, clamps it
// horizontally, and deposits its per-row pieces.
func (s *Scanner) renderLine(ax, ay, bx, by int32) {
	if ay == by || s.h == 0 {
		return // horizontal segments carry no cover
	}
	if (ay <= 0 && by <= 0) || (ay >= s.maxY && by >= s.maxY) {
		return
	}

	// clip y to [0, maxY], interpolating x at the crossings
	oax, oay, obx, oby := ax, ay, bx, by
	xAt := func(y int32) int32 {
		return oax + int32(int64(obx-oax)*int64(y-oay)/int64(oby-oay))
	}
	if ay < 0 {
		ax, ay = xAt(0), 0
	} else if ay > s.maxY {
		ax, ay = xAt(s.maxY), s.maxY
	}
	if by < 0 {
		bx, by = xAt(0), 0
	} else if by > s.maxY {
		bx, by = xAt(s.maxY), s.maxY
	}
	if ay == by {
		return
	}

	// clamp x: out-of-range edges keep their cover at the border, so
	// winding of the visible interior is preserved
	clampX := func(x int32) int32 {
		if x < 0 {
			return 0
		}
		if x > s.maxX {
			return s.maxX
		}
		return x
	}
	ax, bx = clampX(ax), clampX(bx)
	oax, oay, obx, oby = ax, ay, bx, by

	// walk scanline rows, splitting exactly at row boundaries
	cx, cy := ax, ay
	if by > ay { // downward
		row := ay >> subShift
		for {
			bottom := (row + 1) << subShift
			if by <= bottom {
				s.renderRow(row, cx, cy-row<<subShift, bx, by-row<<subShift)
				return
			}
			nx := xAt(bottom)
			s.renderRow(row, cx, cy-row<<subShift, nx, subOne)
			cx, cy = nx, bottom
			row++
		}
	} else { // upward
		row := ay >> subShift
		if ay&subMask == 0 {
			row--
		}
		for {
			top := row << subShift
			if by >= top {
				s.renderRow(row, cx, cy-top, bx, by-top)
				return
			}
			nx := xAt(top)
			s.renderRow(row, cx, cy-top, nx, 0)
			cx, cy = nx, top
			row--
		}
	}
}

// renderRow deposits one row-piece of a segment, splitting exactly at
// pixel column boundaries. fya/fyb are sub-y offsets (0..subOne)
// within the row; x coordinates are 26.6, already clamped.
func (s *Scanner) renderRow(row, xa, fya, xb, fyb int32) {
	dy := fyb - fya
	if dy == 0 {
		return
	}

	exa, exb := xa>>subShift, xb>>subShift
	if exa == exb {
		s.addToCell(exa, row, dy, dy*((xa&subMask)+(xb&subMask)))
		return
	}

	fyAt := func(x int32) int32 {
		return fya + int32(int64(dy)*int64(x-xa)/int64(xb-xa))
	}
	prevX, prevFy := xa, fya
	if xb > xa { // rightward: exit each cell at its right boundary
		for col := exa; col < exb; col++ {
			bx := (col + 1) << subShift
			fyc := fyAt(bx)
			s.addToCell(col, row, fyc-prevFy, (fyc-prevFy)*((prevX-col<<subShift)+subOne))
			prevX, prevFy = bx, fyc
		}
		s.addToCell(exb, row, fyb-prevFy, (fyb-prevFy)*(xb-exb<<subShift))
	} else { // leftward: exit each cell at its left boundary
		for col := exa; col > exb; col-- {
			bx := col << subShift
			fyc := fyAt(bx)
			s.addToCell(col, row, fyc-prevFy, (fyc-prevFy)*(prevX-col<<subShift))
			prevX, prevFy = bx, fyc
		}
		s.addToCell(exb, row, fyb-prevFy, (fyb-prevFy)*(subOne+(xb-exb<<subShift)))
	}
}

// addToCell accumulates into the open cell, flushing when the target
// cell changes.
func (s *Scanner) addToCell(x, row, dcover, darea int32) {
	if s.haveCell && x == s.cellX && row == s.cellY {
		s.cover += dcover
		s.area += darea
		return
	}
	s.flushCell()
	s.haveCell = true
	s.cellX, s.cellY = x, row
	s.cover, s.area = dcover, darea
}

// flushCell merges the open cell into its row's x-sorted list.
func (s *Scanner) flushCell() {
	if !s.haveCell {
		return
	}
	s.haveCell = false
	if s.cover == 0 && s.area == 0 {
		return
	}
	if s.cellY < 0 || int(s.cellY) >= s.h {
		return
	}

	head := &s.rowHead[s.cellY]
	prev, cur := int32(-1), *head
	for cur >= 0 && s.cells[cur].x < s.cellX {
		prev, cur = cur, s.cells[cur].next
	}
	if cur >= 0 && s.cells[cur].x == s.cellX {
		s.cells[cur].cover += s.cover
		s.cells[cur].area += s.area
		return
	}
	idx := int32(len(s.cells))
	s.cells = append(s.cells, cell{x: s.cellX, cover: s.cover, area: s.area, next: cur})
	if prev < 0 {
		*head = idx
	} else {
		s.cells[prev].next = idx
	}
}

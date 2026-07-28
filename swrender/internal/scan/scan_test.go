// SPDX-License-Identifier: Unlicense OR MIT

package scan

import (
	"math"
	"testing"

	"golang.org/x/image/math/fixed"
)

// gridSpanner records per-pixel alpha for assertions.
type gridSpanner struct {
	w, h int
	pix  []uint32
}

func newGrid(w, h int) *gridSpanner {
	return &gridSpanner{w: w, h: h, pix: make([]uint32, w*h)}
}

func (g *gridSpanner) SetColor(interface{}) {}

func (g *gridSpanner) GetSpanFunc() SpanFunc {
	return func(yi, xi0, xi1 int, alpha uint32) {
		for x := xi0; x < xi1; x++ {
			g.pix[yi*g.w+x] += alpha
		}
	}
}

func (g *gridSpanner) at(x, y int) uint32 { return g.pix[y*g.w+x] }

func pt(x, y float64) fixed.Point26_6 {
	return fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(y * 64)}
}

func poly(s *Scanner, pts ...[2]float64) {
	s.Start(pt(pts[0][0], pts[0][1]))
	for _, p := range pts[1:] {
		s.Line(pt(p[0], p[1]))
	}
	s.Line(pt(pts[0][0], pts[0][1]))
}

func TestFullRect(t *testing.T) {
	g := newGrid(10, 10)
	s := NewScanner(g, 10, 10)
	poly(s, [2]float64{2, 2}, [2]float64{8, 2}, [2]float64{8, 8}, [2]float64{2, 8})
	s.Draw()

	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			want := uint32(0)
			if x >= 2 && x < 8 && y >= 2 && y < 8 {
				want = 0xffff
			}
			if got := g.at(x, y); got != want {
				t.Errorf("(%d,%d): got %#x, want %#x", x, y, got, want)
			}
		}
	}
}

func TestOrientationInvariance(t *testing.T) {
	// counter-clockwise must fill identically to clockwise under
	// the non-zero rule
	g := newGrid(10, 10)
	s := NewScanner(g, 10, 10)
	poly(s, [2]float64{2, 2}, [2]float64{2, 8}, [2]float64{8, 8}, [2]float64{8, 2})
	s.Draw()

	if got := g.at(5, 5); got != 0xffff {
		t.Errorf("ccw interior: got %#x, want 0xffff", got)
	}
	if got := g.at(1, 5); got != 0 {
		t.Errorf("ccw exterior: got %#x, want 0", got)
	}
}

func TestHalfPixelEdges(t *testing.T) {
	// rect spanning x in [2.5, 7.5): boundary columns half covered
	g := newGrid(10, 10)
	s := NewScanner(g, 10, 10)
	poly(s, [2]float64{2.5, 2}, [2]float64{7.5, 2}, [2]float64{7.5, 8}, [2]float64{2.5, 8})
	s.Draw()

	if got := g.at(2, 5); got < 0x7f00 || got > 0x8100 {
		t.Errorf("left half pixel: got %#x, want ~0x8000", got)
	}
	if got := g.at(7, 5); got < 0x7f00 || got > 0x8100 {
		t.Errorf("right half pixel: got %#x, want ~0x8000", got)
	}
	if got := g.at(5, 5); got != 0xffff {
		t.Errorf("interior: got %#x, want 0xffff", got)
	}
}

func TestTriangleCoverage(t *testing.T) {
	// right triangle with the hypotenuse crossing pixel diagonals:
	// a diagonal-cut pixel is half covered
	g := newGrid(10, 10)
	s := NewScanner(g, 10, 10)
	poly(s, [2]float64{1, 1}, [2]float64{9, 1}, [2]float64{1, 9})
	s.Draw()

	// pixel (4,5): hypotenuse x+y=10 passes corner-to-corner
	// (from (5,5) to (4,6)), leaving exactly half covered
	if got := g.at(4, 5); got < 0x7000 || got > 0x9000 {
		t.Errorf("diagonal pixel: got %#x, want ~0x8000", got)
	}
	if got := g.at(2, 2); got != 0xffff {
		t.Errorf("interior: got %#x, want 0xffff", got)
	}
	if got := g.at(8, 8); got != 0 {
		t.Errorf("exterior: got %#x, want 0", got)
	}
}

func TestEvenOddRule(t *testing.T) {
	// two nested same-direction squares: even-odd punches a hole,
	// non-zero fills solid
	run := func(winding bool) *gridSpanner {
		g := newGrid(12, 12)
		s := NewScanner(g, 12, 12)
		s.SetWinding(winding)
		poly(s, [2]float64{1, 1}, [2]float64{11, 1}, [2]float64{11, 11}, [2]float64{1, 11})
		poly(s, [2]float64{4, 4}, [2]float64{8, 4}, [2]float64{8, 8}, [2]float64{4, 8})
		s.Draw()
		return g
	}

	if got := run(true).at(6, 6); got != 0xffff {
		t.Errorf("non-zero overlap: got %#x, want 0xffff", got)
	}
	eo := run(false)
	if got := eo.at(6, 6); got != 0 {
		t.Errorf("even-odd hole: got %#x, want 0", got)
	}
	if got := eo.at(2, 6); got != 0xffff {
		t.Errorf("even-odd ring: got %#x, want 0xffff", got)
	}
}

func TestClipping(t *testing.T) {
	// rect far exceeding bounds on all sides: visible area saturates,
	// no panic, no leakage
	g := newGrid(6, 6)
	s := NewScanner(g, 6, 6)
	poly(s, [2]float64{-50, -50}, [2]float64{50, -50}, [2]float64{50, 50}, [2]float64{-50, 50})
	s.Draw()

	for y := 0; y < 6; y++ {
		for x := 0; x < 6; x++ {
			if got := g.at(x, y); got != 0xffff {
				t.Errorf("(%d,%d): got %#x, want 0xffff", x, y, got)
			}
		}
	}
}

func TestPartiallyClipped(t *testing.T) {
	// rect hanging off the left/top: interior correct, boundary safe
	g := newGrid(8, 8)
	s := NewScanner(g, 8, 8)
	poly(s, [2]float64{-3, -3}, [2]float64{4, -3}, [2]float64{4, 4}, [2]float64{-3, 4})
	s.Draw()

	if got := g.at(0, 0); got != 0xffff {
		t.Errorf("clipped corner: got %#x, want 0xffff", got)
	}
	if got := g.at(3, 3); got != 0xffff {
		t.Errorf("inside edge: got %#x, want 0xffff", got)
	}
	if got := g.at(5, 5); got != 0 {
		t.Errorf("outside: got %#x, want 0", got)
	}
}

func TestAutoClose(t *testing.T) {
	// unclosed contour must be closed implicitly by Draw
	g := newGrid(10, 10)
	s := NewScanner(g, 10, 10)
	s.Start(pt(2, 2))
	s.Line(pt(8, 2))
	s.Line(pt(8, 8))
	s.Line(pt(2, 8))
	// no closing Line back to (2,2)
	s.Draw()

	if got := g.at(5, 5); got != 0xffff {
		t.Errorf("auto-closed interior: got %#x, want 0xffff", got)
	}
}

func TestClearAndReuse(t *testing.T) {
	g := newGrid(10, 10)
	s := NewScanner(g, 10, 10)
	poly(s, [2]float64{2, 2}, [2]float64{8, 2}, [2]float64{8, 8}, [2]float64{2, 8})
	s.Draw()
	s.Clear()

	g2 := newGrid(10, 10)
	s.spanner = g2 // not part of the API; direct for test
	poly(s, [2]float64{0, 0}, [2]float64{1, 0}, [2]float64{1, 1}, [2]float64{0, 1})
	s.Draw()

	if got := g2.at(0, 0); got != 0xffff {
		t.Errorf("after clear: got %#x, want 0xffff", got)
	}
	if got := g2.at(5, 5); got != 0 {
		t.Errorf("stale cells leaked: got %#x, want 0", got)
	}
}

func BenchmarkCircleR280(b *testing.B) {
	// matches the historical rasterbench workload: circle radius 280
	const size = 600
	g := newGrid(size, size)
	s := NewScanner(g, size, size)

	const segs = 256
	var pts [][2]float64
	for i := 0; i < segs; i++ {
		th := 2 * math.Pi * float64(i) / segs
		pts = append(pts, [2]float64{300 + 280*math.Cos(th), 300 + 280*math.Sin(th)})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Clear()
		poly(s, pts...)
		s.Draw()
	}
}

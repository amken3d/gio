// SPDX-License-Identifier: Unlicense OR MIT

package swrender

import (
	"image"
	"image/color"
	"image/draw"
	"testing"

	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

func renderOps(o *op.Ops, size image.Point) *image.RGBA {
	dst := image.NewRGBA(image.Rectangle{Max: size})
	New(size).Frame(o, dst)
	return dst
}

// TestImagePaint draws a checkerboard ImageOp offset into the frame.
func TestImagePaint(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	red := color.RGBA{0xff, 0, 0, 0xff}
	blue := color.RGBA{0, 0, 0xff, 0xff}

	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if (x/4+y/4)%2 == 0 {
				src.SetRGBA(x, y, red)
			} else {
				src.SetRGBA(x, y, blue)
			}
		}
	}

	o := new(op.Ops)
	paint.Fill(o, color.NRGBA{A: 0xff})

	func() {
		defer op.Offset(image.Pt(10, 10)).Push(o).Pop()
		paint.NewImageOp(src).Add(o)
		paint.PaintOp{}.Add(o)
	}()

	dst := renderOps(o, image.Pt(32, 32))

	if got := dst.RGBAAt(11, 11); got != red {
		t.Errorf("image top-left quadrant: got %v, want %v", got, red)
	}

	if got := dst.RGBAAt(16, 11); got != blue {
		t.Errorf("image top-right quadrant: got %v, want %v", got, blue)
	}

	if got := dst.RGBAAt(5, 5); (got != color.RGBA{0, 0, 0, 0xff}) {
		t.Errorf("outside image: got %v, want black", got)
	}

	// the image is bounded: nothing may be painted beyond 10+8
	if got := dst.RGBAAt(20, 20); (got != color.RGBA{0, 0, 0, 0xff}) {
		t.Errorf("beyond image bounds: got %v, want black", got)
	}
}

// TestOpacity checks that opacity groups scale paint alpha.
func TestOpacity(t *testing.T) {
	o := new(op.Ops)
	paint.Fill(o, color.NRGBA{A: 0xff}) // black

	func() {
		defer paint.PushOpacity(o, 0.5).Pop()
		c := clip.Rect(image.Rect(0, 0, 16, 16)).Push(o)
		paint.Fill(o, color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
		c.Pop()
	}()

	dst := renderOps(o, image.Pt(32, 32))
	got := dst.RGBAAt(8, 8)

	if got.R < 0x70 || got.R > 0x90 {
		t.Errorf("50%% white over black: got %v, want ~0x80 gray", got)
	}
}

// TestDeferredMacro verifies ops recorded in a macro and executed via
// op.Defer render after (on top of) the main content.
func TestDeferredMacro(t *testing.T) {
	o := new(op.Ops)
	paint.Fill(o, color.NRGBA{A: 0xff})

	// record an overlay macro and defer it
	macro := op.Record(o)
	c := clip.Rect(image.Rect(4, 4, 12, 12)).Push(o)
	paint.Fill(o, color.NRGBA{G: 0xff, A: 0xff})
	c.Pop()
	call := macro.Stop()

	op.Defer(o, call)

	// paint red over the same area in the main sequence; the deferred
	// green overlay must still win
	c2 := clip.Rect(image.Rect(0, 0, 16, 16)).Push(o)
	paint.Fill(o, color.NRGBA{R: 0xff, A: 0xff})
	c2.Pop()

	dst := renderOps(o, image.Pt(32, 32))

	if got := dst.RGBAAt(8, 8); (got != color.RGBA{0, 0xff, 0, 0xff}) {
		t.Errorf("deferred overlay: got %v, want green", got)
	}

	if got := dst.RGBAAt(14, 14); (got != color.RGBA{0xff, 0, 0, 0xff}) {
		t.Errorf("main content: got %v, want red", got)
	}
}

// TestNestedPathClips exercises two stacked path clips (rounded rect
// containing a circle), the multi-mask coverage path.
func TestNestedPathClips(t *testing.T) {
	o := new(op.Ops)
	paint.Fill(o, color.NRGBA{A: 0xff})

	outer := clip.RRect{Rect: image.Rect(0, 0, 32, 32), SE: 8, SW: 8, NE: 8, NW: 8}.Push(o)
	inner := clip.Ellipse(image.Rect(8, 8, 24, 24)).Push(o)
	paint.Fill(o, color.NRGBA{B: 0xff, A: 0xff})
	inner.Pop()
	outer.Pop()

	dst := renderOps(o, image.Pt(32, 32))

	if got := dst.RGBAAt(16, 16); (got != color.RGBA{0, 0, 0xff, 0xff}) {
		t.Errorf("circle center: got %v, want blue", got)
	}

	if got := dst.RGBAAt(4, 4); (got != color.RGBA{0, 0, 0, 0xff}) {
		t.Errorf("outside circle: got %v, want black", got)
	}
}

// TestScissorSubwindow verifies rect clips constrain a full-frame paint.
func TestScissorSubwindow(t *testing.T) {
	o := new(op.Ops)

	white := color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	paint.Fill(o, color.NRGBA{A: 0xff})

	c := clip.Rect(image.Rect(8, 8, 24, 24)).Push(o)
	paint.Fill(o, white)
	c.Pop()

	dst := renderOps(o, image.Pt(32, 32))

	want := image.NewRGBA(image.Rect(0, 0, 32, 32))
	draw.Draw(want, want.Bounds(), image.NewUniform(color.RGBA{0, 0, 0, 0xff}), image.Point{}, draw.Src)
	draw.Draw(want, image.Rect(8, 8, 24, 24), image.NewUniform(color.RGBA{0xff, 0xff, 0xff, 0xff}), image.Point{}, draw.Src)

	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			if dst.RGBAAt(x, y) != want.RGBAAt(x, y) {
				t.Fatalf("pixel (%d,%d): got %v, want %v", x, y, dst.RGBAAt(x, y), want.RGBAAt(x, y))
			}
		}
	}
}

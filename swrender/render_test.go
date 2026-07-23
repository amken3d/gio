// SPDX-License-Identifier: Unlicense OR MIT

package swrender

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

func newTheme() *material.Theme {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	return th
}

func testContext(o *op.Ops, size image.Point) layout.Context {
	return layout.Context{
		Ops:         o,
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(size),
	}
}

// TestRenderScene renders background, rounded-rect card, gradient bar,
// button, and text; then spot-checks pixels and writes a PNG for eyes.
func TestRenderScene(t *testing.T) {
	size := image.Pt(640, 360)
	o := new(op.Ops)
	th := newTheme()
	gtx := testContext(o, size)

	// background
	paint.Fill(o, color.NRGBA{R: 0x12, G: 0x12, B: 0x1a, A: 0xff})

	// rounded-rect card
	card := clip.RRect{Rect: image.Rect(20, 20, 300, 130), SE: 16, SW: 16, NE: 16, NW: 16}.Push(o)
	paint.Fill(o, color.NRGBA{R: 0x2a, G: 0x2f, B: 0x45, A: 0xff})
	card.Pop()

	// gradient bar
	bar := clip.Rect(image.Rect(20, 150, 620, 180)).Push(o)
	paint.LinearGradientOp{
		Stop1:  layout.FPt(image.Pt(20, 0)),
		Stop2:  layout.FPt(image.Pt(620, 0)),
		Color1: color.NRGBA{R: 0xff, G: 0x40, B: 0x40, A: 0xff},
		Color2: color.NRGBA{R: 0x40, G: 0x40, B: 0xff, A: 0xff},
	}.Add(o)
	paint.PaintOp{}.Add(o)
	bar.Pop()

	// text inside the card
	func() {
		defer op.Offset(image.Pt(40, 40)).Push(o).Pop()
		lbl := material.H6(th, "bmx software renderer")
		lbl.Color = color.NRGBA{R: 0xee, G: 0xee, B: 0xf2, A: 0xff}
		lbl.Layout(gtx)
	}()

	// material button
	func() {
		defer op.Offset(image.Pt(40, 200)).Push(o).Pop()
		var btn widget.Clickable
		b := material.Button(th, &btn, "PRINT")
		gtx := gtx
		gtx.Constraints = layout.Exact(image.Pt(160, 48))
		b.Layout(gtx)
	}()

	// stroked circle
	func() {
		defer op.Offset(image.Pt(480, 220)).Push(o).Pop()
		c := clip.Ellipse(image.Rect(0, 0, 100, 100))
		defer clip.Stroke{Path: c.Path(o), Width: 6}.Op().Push(o).Pop()
		paint.Fill(o, color.NRGBA{R: 0x40, G: 0xd0, B: 0x80, A: 0xff})
	}()

	dst := image.NewRGBA(image.Rectangle{Max: size})
	r := New(size)
	r.Frame(o, dst)

	// spot checks
	checks := []struct {
		x, y int
		want color.RGBA
		name string
	}{
		{5, 5, color.RGBA{0x12, 0x12, 0x1a, 0xff}, "background"},
		{150, 75, color.RGBA{0x2a, 0x2f, 0x45, 0xff}, "card interior"},
		{22, 22, color.RGBA{0x12, 0x12, 0x1a, 0xff}, "card corner is rounded away"},
	}

	for _, c := range checks {
		got := dst.RGBAAt(c.x, c.y)
		if got != c.want {
			t.Errorf("%s at (%d,%d): got %v, want %v", c.name, c.x, c.y, got, c.want)
		}
	}

	// gradient midpoint: red and blue roughly equal
	mid := dst.RGBAAt(320, 165)
	if mid.R < 0x60 || mid.R > 0xa0 || mid.B < 0x60 || mid.B > 0xa0 {
		t.Errorf("gradient midpoint: got %v, want mixed red/blue", mid)
	}

	// text must have touched pixels near the label origin with light color
	found := false
	for y := 40; y < 80 && !found; y++ {
		for x := 40; x < 290; x++ {
			p := dst.RGBAAt(x, y)
			if p.R > 0x80 && p.G > 0x80 {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("no text pixels found in label area")
	}

	if out := os.Getenv("SWRENDER_PNG"); out != "" {
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, dst); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", out)
	}
}

// TestMaskCacheFreshOps reproduces the real GUI loop: a dashboard-like
// frame (cards + text) re-encoded every frame — either into a freshly
// Reset buffer or a brand new op.Ops. The content-hashed cache must
// stop growing after the first frame; identity-keyed caching regressed
// here (heap grew ~0.5 MB/frame on hardware, frame time crept up).
func TestMaskCacheFreshOps(t *testing.T) {
	size := image.Pt(640, 360)
	th := newTheme()
	r := New(size)
	dst := image.NewRGBA(image.Rectangle{Max: size})

	frame := func(o *op.Ops) {
		gtx := testContext(o, size)

		paint.Fill(o, color.NRGBA{R: 0x12, G: 0x12, B: 0x1a, A: 0xff})

		card := clip.RRect{Rect: image.Rect(20, 20, 620, 200), SE: 16, SW: 16, NE: 16, NW: 16}.Push(o)
		paint.Fill(o, color.NRGBA{R: 0x2a, G: 0x2f, B: 0x45, A: 0xff})
		card.Pop()

		func() {
			defer op.Offset(image.Pt(40, 40)).Push(o).Pop()
			material.H6(th, "uptime 123.4s heap 2048 KiB").Layout(gtx)
		}()

		r.Frame(o, dst)
	}

	reused := new(op.Ops)

	frame(reused)
	entries, bytes := len(r.cache), r.cacheBytes

	if entries == 0 || bytes == 0 {
		t.Fatal("nothing cached after first frame")
	}

	for i := 0; i < 10; i++ {
		reused.Reset()
		frame(reused)      // Reset buffer (version bump)
		frame(new(op.Ops)) // brand new buffer
	}

	if len(r.cache) != entries || r.cacheBytes != bytes {
		t.Errorf("cache grew across identical frames: %d entries/%d bytes -> %d/%d",
			entries, bytes, len(r.cache), r.cacheBytes)
	}

	if bytes > maxCacheBytes {
		t.Errorf("single frame exceeds cache byte budget: %d", bytes)
	}
}

// TestMaskCache verifies repeated frames hit the mask cache for text.
func TestMaskCache(t *testing.T) {
	size := image.Pt(320, 120)
	th := newTheme()
	r := New(size)
	dst := image.NewRGBA(image.Rectangle{Max: size})

	render := func() {
		o := new(op.Ops)
		gtx := testContext(o, size)
		lbl := material.Body1(th, "cache me")
		lbl.Layout(gtx)
		r.Frame(o, dst)
	}

	render()
	n1 := len(r.cache)
	if n1 == 0 {
		t.Fatal("no masks cached after first frame")
	}

	render()
	n2 := len(r.cache)

	if n2 > n1 {
		t.Errorf("cache grew across identical frames: %d -> %d (glyph masks not reused)", n1, n2)
	}
}

func BenchmarkFrame(b *testing.B) {
	size := image.Pt(1280, 720)
	th := newTheme()
	r := New(size)
	dst := image.NewRGBA(image.Rectangle{Max: size})

	frame := func(uptime int) {
		o := new(op.Ops)
		gtx := testContext(o, size)

		paint.Fill(o, color.NRGBA{R: 0x12, G: 0x12, B: 0x1a, A: 0xff})

		card := clip.RRect{Rect: image.Rect(20, 20, 620, 200), SE: 16, SW: 16, NE: 16, NW: 16}.Push(o)
		paint.Fill(o, color.NRGBA{R: 0x2a, G: 0x2f, B: 0x45, A: 0xff})
		card.Pop()

		func() {
			defer op.Offset(image.Pt(40, 40)).Push(o).Pop()
			material.H4(th, "bmx dashboard").Layout(gtx)
		}()

		func() {
			defer op.Offset(image.Pt(40, 120)).Push(o).Pop()
			material.Body1(th, "uptime 123.4s   heap 2048 KiB   12 goroutines").Layout(gtx)
		}()

		r.Frame(o, dst)
	}

	frame(0)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		frame(i)
	}
}

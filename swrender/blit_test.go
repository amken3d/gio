// SPDX-License-Identifier: Unlicense OR MIT

package swrender

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"
	"testing"

	"gioui.org/internal/f32"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// blitTestImage builds a deterministic premultiplied RGBA test card.
// kind selects the alpha structure: opaque, translucent everywhere, or
// mixed (opaque / translucent / fully transparent regions).
func blitTestImage(w, h int, kind string) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var a uint8
			switch kind {
			case "opaque":
				a = 255
			case "translucent":
				a = uint8(1 + rng.Intn(254))
			case "mixed":
				switch (x / 7) % 3 {
				case 0:
					a = 255
				case 1:
					a = uint8(rng.Intn(256))
				default:
					a = 0
				}
			}

			// premultiplied: channels never exceed alpha
			c := color.RGBA{A: a}
			if a > 0 {
				c.R = uint8(rng.Intn(int(a) + 1))
				c.G = uint8(rng.Intn(int(a) + 1))
				c.B = uint8(rng.Intn(int(a) + 1))
			}
			img.SetRGBA(x, y, c)
		}
	}

	return img
}

// dstBackground fills a destination with a deterministic non-uniform
// background, so blending against existing content is exercised.
func dstBackground(w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range dst.Pix {
		dst.Pix[i] = uint8(i*7 + i>>3)
	}
	for i := 3; i < len(dst.Pix); i += 4 {
		dst.Pix[i] = 255
	}
	return dst
}

// TestBlitImageMatchesGeneric pins the identity-transform fast path
// byte-identical to fillGeneric across alpha structures and offsets,
// including offsets that push the clip against and past the image edges.
func TestBlitImageMatchesGeneric(t *testing.T) {
	const W, H = 64, 48

	offsets := []f32.Point{
		{X: 0, Y: 0},
		{X: 5, Y: 9},
		{X: -3, Y: -2},
		{X: 2.5, Y: 7.5},
		{X: 0.25, Y: -0.75},
	}

	r := New(image.Pt(W, H))

	for _, kind := range []string{"opaque", "translucent", "mixed"} {
		img := blitTestImage(20, 15, kind)

		for _, off := range offsets {
			tr := f32.Affine2D{}.Offset(off)

			if !identityScale(tr) {
				t.Fatalf("offset transform not identity-scale")
			}

			// clip: image extent under the transform, intersected with the
			// viewport -- what paint() computes; widen it past the image on
			// purpose for the clamp segments.
			cl := image.Rect(0, 0, W, H)

			want := dstBackground(W, H)
			mat := &materialState{typ: matImage, img: img}
			r.fillGeneric(want, cl, nil, mat, tr, 1)

			got := dstBackground(W, H)
			blitImage(got, cl, img, tr)

			if !bytes.Equal(want.Pix, got.Pix) {
				for i := range want.Pix {
					if want.Pix[i] != got.Pix[i] {
						t.Fatalf("kind=%s off=%v: first mismatch at byte %d (pixel %d,%d): want %d got %d",
							kind, off, i, (i/4)%W, i/4/W, want.Pix[i], got.Pix[i])
					}
				}
			}
		}
	}
}

// TestBlitImageViaFrame checks the fast path engages through the public
// Frame API and produces the same output as a reference rasterization of
// the same scene rendered before the fast path existed (the generic
// path, forced via a hair of scale).
func TestBlitImageOpaqueRowCopy(t *testing.T) {
	const W, H = 32, 24

	img := blitTestImage(W, H, "opaque")
	dst := dstBackground(W, H)

	blitImage(dst, image.Rect(0, 0, W, H), img, f32.Affine2D{})

	if !bytes.Equal(dst.Pix, img.Pix) {
		t.Fatalf("opaque identity blit at origin must reproduce the source exactly")
	}
}

// TestBlitEngagesThroughFrame renders the exact op sequence a
// viewport-fitted video frame produces -- rect clip, integer offset,
// ImageOp, PaintOp -- and asserts the fast path actually took it. This
// is the regression guard for the engagement condition: a future change
// that sneaks a scale or a mask into that path shows up here, not as a
// silent 10x on the target.
func TestBlitEngagesThroughFrame(t *testing.T) {
	const W, H = 64, 48

	img := blitTestImage(40, 30, "opaque")

	o := new(op.Ops)
	cl := clip.Rect{Max: image.Pt(W, H)}.Push(o)
	off := op.Offset(image.Pt(12, 9)).Push(o)
	paint.NewImageOp(img).Add(o)
	paint.PaintOp{}.Add(o)
	off.Pop()
	cl.Pop()

	r := New(image.Pt(W, H))
	dst := image.NewRGBA(image.Rect(0, 0, W, H))
	r.Frame(o, dst)

	if n := r.BlitCount(); n != 1 {
		t.Fatalf("identity image paint did not take the fast path (BlitCount=%d)", n)
	}

	// and the pixels really are the source rows at the offset
	for y := 0; y < 30; y++ {
		got := dst.Pix[(y+9)*dst.Stride+12*4 : (y+9)*dst.Stride+(12+40)*4]
		want := img.Pix[y*img.Stride : y*img.Stride+40*4]
		if !bytes.Equal(got, want) {
			t.Fatalf("row %d: blitted pixels differ from source", y)
		}
	}
}

func BenchmarkBlitImageOpaque(b *testing.B) {
	img := blitTestImage(1024, 576, "opaque")
	dst := dstBackground(1024, 600)
	cl := image.Rect(0, 0, 1024, 576)

	b.SetBytes(int64(1024 * 576 * 4))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		blitImage(dst, cl, img, f32.Affine2D{})
	}
}

func BenchmarkFillGenericImage(b *testing.B) {
	img := blitTestImage(1024, 576, "opaque")
	dst := dstBackground(1024, 600)
	cl := image.Rect(0, 0, 1024, 576)

	r := New(image.Pt(1024, 600))
	mat := &materialState{typ: matImage, img: img}

	b.SetBytes(int64(1024 * 576 * 4))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		r.fillGeneric(dst, cl, nil, mat, f32.Affine2D{}, 1)
	}
}

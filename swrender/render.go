// SPDX-License-Identifier: Unlicense OR MIT

/*
Package swrender is a pure-Go software renderer for Gio op lists. It walks
the internal op stream (the same stream gpu.Frame consumes) and rasterizes
it into a caller-provided *image.RGBA, with no GPU, cgo, or OS dependency.

It exists for bare-metal targets (GOOS=tamago) where neither the GL/Vulkan
backends nor the cgo-based gioui.org/cpu fallback can run. Paths are filled
with an AGG-style sparse coverage rasterizer (see internal/scan) and
rasterized path masks are cached across frames keyed by geometry content
(a hash of the encoded path) and transform, which makes all repeated
shapes — text glyphs, widget outlines, panels — cheap after their first
frame regardless of how the UI rebuilds its op buffers.

Deviations from the GPU renderer, chosen for simplicity on small targets:
opacity layers are approximated by multiplying alpha per paint (differs for
overlapping siblings under one opacity group); gradient interpolation is in
sRGB space rather than linear; image sampling is nearest-neighbor; painting
under rotation/shear bounds images by their transformed bounding box.
*/
package swrender

import (
	"image"
	"image/color"
	"math"
	"runtime"

	"encoding/binary"

	"gioui.org/internal/f32"
	"gioui.org/internal/ops"
	"gioui.org/internal/scene"
	"gioui.org/internal/stroke"
	"gioui.org/op"
	"gioui.org/swrender/internal/scan"
	"golang.org/x/image/math/fixed"
)

// preemptYield gives the Go scheduler a cooperative preemption point roughly
// every 64 rows inside the long per-row fill loops. On a bare-metal single core
// these loops otherwise run with no call-safepoint and can monopolize the core
// for tens of milliseconds (memmove/blend are nosplit), stalling interactive
// goroutines; yielding here lets the scheduler time-slice the software frame.
//
//go:noinline
func preemptYield(y int) {
	if y&63 == 0 {
		runtime.Gosched()
	}
}

// Renderer rasterizes Gio op lists into an RGBA image. It retains a path
// mask cache between frames; reuse one Renderer per output surface.
type Renderer struct {
	scanner *scan.Scanner
	mspan   *maskSpanner
	scanW   int
	scanH   int

	blits uint64

	cache      map[maskKey]*maskEntry
	cacheBytes int
	frame      uint32

	reader     ops.Reader
	transStack []f32.Affine2D
	opacity    []float32
	states     []f32.Affine2D
	clipNodes  []clipNode

	flat flattener
}

type materialType uint8

const (
	matColor materialType = iota
	matGradient
	matImage
)

type materialState struct {
	typ    materialType
	color  color.NRGBA
	stop1  f32.Point
	stop2  f32.Point
	color1 color.NRGBA
	color2 color.NRGBA
	img    *image.RGBA
}

type clipNode struct {
	parent  *clipNode
	rect    image.Rectangle // device clip, intersected with ancestors
	mask    *image.Alpha    // nil for rectangular clips
	maskMin image.Point     // device position of mask origin
}

type drawState struct {
	t     f32.Affine2D
	cpath *clipNode
	mat   materialState
}

// maskKey identifies a rasterized path mask by geometry content (a hash
// of the encoded path data) and transform. Content hashing — rather than
// op identity — makes the cache stable across frames: UIs re-encode
// identical paths into freshly Reset op buffers every frame, so identity
// keys would miss (and leak) on every frame.
type maskKey struct {
	hash                   uint64
	sx, hx, ox, hy, sy, oy float32
	outline                bool
	strokeWidth            float32
}

type maskEntry struct {
	alpha *image.Alpha
	rect  image.Rectangle // device bounds before integer offset
	frame uint32
}

// Cache bounds: unused entries are evicted at frame end once either is
// exceeded. The byte budget matters most — mask sizes span three orders
// of magnitude (glyphs vs panels).
const (
	maxCacheEntries = 2048
	maxCacheBytes   = 8 << 20
)

// fnv1a hashes b (FNV-1a, 64 bit).
func fnv1a(seed uint64, b []byte) uint64 {
	h := seed

	if h == 0 {
		h = 0xcbf29ce484222325
	}

	for _, c := range b {
		h ^= uint64(c)
		h *= 0x100000001b3
	}

	return h
}

// New creates a Renderer for output surfaces up to size.
func New(size image.Point) *Renderer {
	mspan := &maskSpanner{}
	sc := scan.NewScanner(mspan, size.X, size.Y)
	sc.SetWinding(true)

	return &Renderer{
		scanner: sc,
		mspan:   mspan,
		scanW:   size.X,
		scanH:   size.Y,
		cache:   make(map[maskKey]*maskEntry),
	}
}

// Frame renders root into dst. dst's bounds must start at (0,0).
func (r *Renderer) Frame(root *op.Ops, dst *image.RGBA) {
	r.frame++
	r.transStack = r.transStack[:0]
	r.opacity = r.opacity[:0]
	r.clipNodes = r.clipNodes[:0]

	viewport := dst.Bounds()

	var state drawState

	reset := func() {
		state = drawState{
			t:   f32.AffineId(),
			mat: materialState{typ: matColor, color: color.NRGBA{A: 0xff}},
		}
	}
	reset()

	var (
		pathAux     []byte
		strokeWidth float32
	)

	r.reader.Reset(&root.Internal)

loop:
	for encOp, ok := r.reader.Decode(); ok; encOp, ok = r.reader.Decode() {
		switch ops.OpType(encOp.Data[0]) {
		case ops.TypeTransform:
			dop, push := ops.DecodeTransform(encOp.Data)
			if push {
				r.transStack = append(r.transStack, state.t)
			}
			state.t = state.t.Mul(dop)

		case ops.TypePopTransform:
			n := len(r.transStack)
			state.t = r.transStack[n-1]
			r.transStack = r.transStack[:n-1]

		case ops.TypePushOpacity:
			r.opacity = append(r.opacity, ops.DecodeOpacity(encOp.Data))

		case ops.TypePopOpacity:
			r.opacity = r.opacity[:len(r.opacity)-1]

		case ops.TypeStroke:
			strokeWidth = decodeStrokeOp(encOp.Data)

		case ops.TypePath:
			encOp, ok = r.reader.Decode()
			if !ok {
				break loop
			}
			pathAux = encOp.Data[ops.TypeAuxLen:]

		case ops.TypeClip:
			var cop ops.ClipOp
			cop.Decode(encOp.Data)

			parentRect := viewport
			if state.cpath != nil {
				parentRect = state.cpath.rect
			}

			node := r.newClip(state.cpath)

			if len(pathAux) > 0 {
				trans, off := splitTransform(state.t)
				m := r.maskFor(pathAux, trans, cop.Outline, strokeWidth)

				if m == nil {
					node.rect = image.Rectangle{}
				} else {
					dr := m.rect.Add(off)
					node.rect = parentRect.Intersect(dr)
					node.mask = m.alpha
					node.maskMin = dr.Min
				}
			} else if axisAligned(state.t) {
				node.rect = parentRect.Intersect(transformRect(f32.FRect(cop.Bounds), state.t).Round())
			} else {
				// Rect clip under rotation/shear: rasterize the
				// transformed quad as a polygon mask.
				trans, off := splitTransform(state.t)
				m := r.maskForQuad(f32.FRect(cop.Bounds), trans)

				if m == nil {
					node.rect = image.Rectangle{}
				} else {
					dr := m.rect.Add(off)
					node.rect = parentRect.Intersect(dr)
					node.mask = m.alpha
					node.maskMin = dr.Min
				}
			}

			state.cpath = node
			pathAux = nil
			strokeWidth = 0

		case ops.TypePopClip:
			state.cpath = state.cpath.parent

		case ops.TypeColor:
			state.mat = materialState{typ: matColor, color: decodeColorOp(encOp.Data)}

		case ops.TypeLinearGradient:
			g := decodeLinearGradientOp(encOp.Data)
			state.mat = materialState{
				typ:    matGradient,
				stop1:  g.stop1,
				stop2:  g.stop2,
				color1: g.color1,
				color2: g.color2,
			}

		case ops.TypeImage:
			state.mat = materialState{typ: matImage}
			if img, ok := encOp.Refs[0].(*image.RGBA); ok && encOp.Refs[1] != nil {
				state.mat.img = img
			}

		case ops.TypePaint:
			r.paint(dst, viewport, &state)

		case ops.TypeSave:
			id := ops.DecodeSave(encOp.Data)
			for len(r.states) <= id {
				r.states = append(r.states, f32.AffineId())
			}
			r.states[id] = state.t

		case ops.TypeLoad:
			reset()
			id := ops.DecodeLoad(encOp.Data)
			state.t = r.states[id]
		}
	}

	r.evict()
}

func (r *Renderer) newClip(parent *clipNode) *clipNode {
	// nodes are bump-allocated per frame; parents always precede children
	// so appends never move a node still referenced by pointer
	if len(r.clipNodes) == cap(r.clipNodes) {
		// grow without moving existing nodes: chain a fresh block
		r.clipNodes = make([]clipNode, 0, 2*cap(r.clipNodes)+64)
	}

	r.clipNodes = append(r.clipNodes, clipNode{parent: parent})

	return &r.clipNodes[len(r.clipNodes)-1]
}

func (r *Renderer) evict() {
	if len(r.cache) <= maxCacheEntries && r.cacheBytes <= maxCacheBytes {
		return
	}

	for k, e := range r.cache {
		if e == nil {
			delete(r.cache, k)
			continue
		}

		if e.frame != r.frame {
			r.cacheBytes -= len(e.alpha.Pix)
			delete(r.cache, k)
		}
	}
}

// paint fills the current clip with the current material.
func (r *Renderer) paint(dst *image.RGBA, viewport image.Rectangle, state *drawState) {
	cl := viewport

	if state.cpath != nil {
		cl = cl.Intersect(state.cpath.rect)
	}

	mat := &state.mat

	if mat.typ == matImage {
		if mat.img == nil {
			return
		}

		sz := mat.img.Bounds().Size()
		fr := f32.Rectangle{Max: f32.Point{X: float32(sz.X), Y: float32(sz.Y)}}

		if axisAligned(state.t) {
			cl = cl.Intersect(transformRect(fr, state.t).Round())
		} else {
			cl = cl.Intersect(transformBounds(fr, state.t).Round())
		}
	}

	if cl.Empty() {
		return
	}

	opacity := float32(1)
	for _, o := range r.opacity {
		opacity *= o
	}

	// collect the path masks active along the clip chain (innermost first)
	var masks [8]*clipNode
	nmasks := 0

	for n := state.cpath; n != nil && nmasks < len(masks); n = n.parent {
		if n.mask != nil {
			masks[nmasks] = n
			nmasks++
		}
	}

	r.fill(dst, cl, masks[:nmasks], mat, state.t, opacity)
}

// fill blends the material over cl, modulated by the given path masks.
func (r *Renderer) fill(dst *image.RGBA, cl image.Rectangle, masks []*clipNode, mat *materialState, t f32.Affine2D, opacity float32) {
	switch {
	case mat.typ == matColor && len(masks) == 0:
		fillRectSolid(dst, cl, mat.color, opacity)
	case mat.typ == matColor && len(masks) == 1:
		fillMaskSolid(dst, cl, masks[0], mat.color, opacity)
	case mat.typ == matImage && mat.img != nil && len(masks) == 0 &&
		opacity == 1 && identityScale(t):
		r.blits++
		blitImage(dst, cl, mat.img, t)
	default:
		r.fillGeneric(dst, cl, masks, mat, t, opacity)
	}
}

// BlitCount reports how many image paints have taken the identity blit
// fast path since the renderer was created. It exists to answer, from a
// live target with no profiler, whether the hot image on screen is
// actually hitting the fast path (the count advances every frame) or
// silently falling to the generic sampler (it stays put).
func (r *Renderer) BlitCount() uint64 {
	return r.blits
}

// identityScale reports whether t only translates: unit scale, no
// rotation or shear. Such transforms are what an image drawn at its own
// size produces (a video frame converted at viewport size, a screenshot),
// and sampling under them degenerates to row copies.
func identityScale(t f32.Affine2D) bool {
	sx, hx, _, hy, sy, _ := t.Elems()
	return sx == 1 && sy == 1 && hx == 0 && hy == 0
}

// blitImage is the identity-transform image fast path: with unit scale,
// full opacity and no path masks, the generic path's per-pixel inverse
// transform, clamp and unpremultiply reduce to three row segments --
// left edge clamp, straight copy, right edge clamp. For an opaque image
// (a camera frame) the middle segment is a memmove, which is the whole
// point: a fullscreen video frame stops costing per-pixel float math.
//
// The output is byte-identical to fillGeneric over the same state: the
// source index derivation mirrors imageAt's truncation, and the
// translucent fallback reproduces its unpremultiply/re-premultiply
// rounding exactly.
func blitImage(dst *image.RGBA, cl image.Rectangle, img *image.RGBA, t f32.Affine2D) {
	_, _, ox, _, _, oy := t.Elems()

	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()

	if sw == 0 || sh == 0 || cl.Empty() {
		return
	}

	// imageAt samples source pixel int(float32(x)+.5-ox). For integer x
	// and non-negative results, truncation is floor, and the map is
	// x + floor(.5-o) with a constant offset per axis.
	kx := int(math.Floor(0.5 - float64(ox)))
	ky := int(math.Floor(0.5 - float64(oy)))

	opaque := imageOpaque(img)

	// Interior x range where the source index needs no clamping.
	xlo, xhi := cl.Min.X, cl.Max.X
	if -kx > xlo {
		xlo = -kx
	}
	if sw-kx < xhi {
		xhi = sw - kx
	}
	if xlo > cl.Max.X {
		xlo = cl.Max.X
	}
	if xhi < xlo {
		xhi = xlo
	}

	for y := cl.Min.Y; y < cl.Max.Y; y++ {
		preemptYield(y)

		sy := y + ky
		if sy < 0 {
			sy = 0
		} else if sy >= sh {
			sy = sh - 1
		}

		srow := img.Pix[sy*img.Stride:]
		drow := dst.Pix[(y-dst.Rect.Min.Y)*dst.Stride:]

		// left clamp: source pixel 0; right clamp: source pixel sw-1
		for x := cl.Min.X; x < xlo; x++ {
			blitPix(drow[(x-dst.Rect.Min.X)*4:], srow[0:], opaque)
		}

		if xhi > xlo {
			si := (xlo + kx) * 4
			di := (xlo - dst.Rect.Min.X) * 4
			n := (xhi - xlo) * 4

			if opaque {
				copy(drow[di:di+n], srow[si:si+n])
			} else {
				for x := 0; x < n; x += 4 {
					blitPix(drow[di+x:], srow[si+x:], false)
				}
			}
		}

		for x := xhi; x < cl.Max.X; x++ {
			blitPix(drow[(x-dst.Rect.Min.X)*4:], srow[(sw-1)*4:], opaque)
		}
	}
}

// blitPix transfers one premultiplied source pixel, reproducing the
// generic path's arithmetic: opaque pixels store, translucent ones go
// through the same unpremultiply (truncating) and re-premultiply
// (rounding) sequence imageAt+blendPix perform, so the two paths cannot
// be told apart by output.
func blitPix(d, s []byte, opaque bool) {
	pa := s[3]

	if opaque || pa == 255 {
		d[0], d[1], d[2], d[3] = s[0], s[1], s[2], 255
		return
	}

	if pa == 0 {
		return
	}

	a := uint32(pa)
	cr := uint32(s[0]) * 255 / a
	cg := uint32(s[1]) * 255 / a
	cb := uint32(s[2]) * 255 / a

	blendPix(d[0:4:4], mul255(cr, a), mul255(cg, a), mul255(cb, a), a)
}

// imageOpaque reports whether every pixel of img is fully opaque. The
// scan is a sequential alpha-byte read -- around 2% of the work the
// per-pixel path would spend on the same image, paid per paint.
func imageOpaque(img *image.RGBA) bool {
	b := img.Bounds()
	w := b.Dx() * 4

	for y := 0; y < b.Dy(); y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w]

		for i := 3; i < len(row); i += 4 {
			if row[i] != 0xff {
				return false
			}
		}
	}

	return true
}

// coverage returns the combined mask coverage at device pixel (x, y),
// in the range [0, 255].
func coverage(masks []*clipNode, x, y int) uint32 {
	cov := uint32(255)

	for _, m := range masks {
		mx, my := x-m.maskMin.X, y-m.maskMin.Y
		b := m.mask.Bounds()

		if mx < 0 || my < 0 || mx >= b.Dx() || my >= b.Dy() {
			return 0
		}

		a := uint32(m.mask.Pix[my*m.mask.Stride+mx])
		cov = mul255(cov, a)

		if cov == 0 {
			return 0
		}
	}

	return cov
}

func (r *Renderer) fillGeneric(dst *image.RGBA, cl image.Rectangle, masks []*clipNode, mat *materialState, t f32.Affine2D, opacity float32) {
	var inv f32.Affine2D

	if mat.typ != matColor {
		inv = t.Invert()
	}

	op8 := uint32(opacity*255 + .5)

	for y := cl.Min.Y; y < cl.Max.Y; y++ {
		preemptYield(y)
		row := dst.Pix[(y-dst.Rect.Min.Y)*dst.Stride:]

		for x := cl.Min.X; x < cl.Max.X; x++ {
			cov := coverage(masks, x, y)

			if cov == 0 {
				continue
			}

			cov = mul255(cov, op8)

			var c color.NRGBA

			switch mat.typ {
			case matColor:
				c = mat.color
			case matGradient:
				c = gradientAt(mat, inv, x, y)
			case matImage:
				c = imageAt(mat, inv, x, y)
			}

			a := mul255(uint32(c.A), cov)

			if a == 0 {
				continue
			}

			i := (x - dst.Rect.Min.X) * 4
			blendPix(row[i:i+4:i+4], mul255(uint32(c.R), a), mul255(uint32(c.G), a), mul255(uint32(c.B), a), a)
		}
	}
}

func gradientAt(mat *materialState, inv f32.Affine2D, x, y int) color.NRGBA {
	p := inv.Transform(f32.Point{X: float32(x) + .5, Y: float32(y) + .5})

	dx, dy := mat.stop2.X-mat.stop1.X, mat.stop2.Y-mat.stop1.Y
	den := dx*dx + dy*dy

	var u float32

	if den > 0 {
		u = ((p.X-mat.stop1.X)*dx + (p.Y-mat.stop1.Y)*dy) / den
	}

	if u < 0 {
		u = 0
	} else if u > 1 {
		u = 1
	}

	lerp := func(a, b uint8) uint8 {
		return uint8(float32(a) + (float32(b)-float32(a))*u + .5)
	}

	return color.NRGBA{
		R: lerp(mat.color1.R, mat.color2.R),
		G: lerp(mat.color1.G, mat.color2.G),
		B: lerp(mat.color1.B, mat.color2.B),
		A: lerp(mat.color1.A, mat.color2.A),
	}
}

func imageAt(mat *materialState, inv f32.Affine2D, x, y int) color.NRGBA {
	p := inv.Transform(f32.Point{X: float32(x) + .5, Y: float32(y) + .5})

	b := mat.img.Bounds()
	sx, sy := int(p.X), int(p.Y)

	if sx < 0 {
		sx = 0
	} else if sx >= b.Dx() {
		sx = b.Dx() - 1
	}

	if sy < 0 {
		sy = 0
	} else if sy >= b.Dy() {
		sy = b.Dy() - 1
	}

	i := sy*mat.img.Stride + sx*4
	pr, pg, pb, pa := mat.img.Pix[i], mat.img.Pix[i+1], mat.img.Pix[i+2], mat.img.Pix[i+3]

	// un-premultiply (paint.ImageOp sources are premultiplied RGBA)
	if pa == 0 {
		return color.NRGBA{}
	}

	if pa == 255 {
		return color.NRGBA{R: pr, G: pg, B: pb, A: 255}
	}

	return color.NRGBA{
		R: uint8(uint32(pr) * 255 / uint32(pa)),
		G: uint8(uint32(pg) * 255 / uint32(pa)),
		B: uint8(uint32(pb) * 255 / uint32(pa)),
		A: pa,
	}
}

// fillRectSolid fills cl with a solid color using row replication.
func fillRectSolid(dst *image.RGBA, cl image.Rectangle, c color.NRGBA, opacity float32) {
	a := uint32(float32(c.A)*opacity + .5)

	if a == 0 {
		return
	}

	sr := mul255(uint32(c.R), a)
	sg := mul255(uint32(c.G), a)
	sb := mul255(uint32(c.B), a)

	w := cl.Dx()

	if a == 255 {
		// opaque: build one row, copy to the rest
		first := (cl.Min.Y-dst.Rect.Min.Y)*dst.Stride + (cl.Min.X-dst.Rect.Min.X)*4
		row := dst.Pix[first : first+w*4]
		row[0], row[1], row[2], row[3] = uint8(sr), uint8(sg), uint8(sb), 255

		for n := 4; n < len(row); n *= 2 {
			copy(row[n:], row[:n])
		}

		for y := cl.Min.Y + 1; y < cl.Max.Y; y++ {
			preemptYield(y)
			i := (y-dst.Rect.Min.Y)*dst.Stride + (cl.Min.X-dst.Rect.Min.X)*4
			copy(dst.Pix[i:i+w*4], row)
		}

		return
	}

	for y := cl.Min.Y; y < cl.Max.Y; y++ {
		preemptYield(y)
		i := (y-dst.Rect.Min.Y)*dst.Stride + (cl.Min.X-dst.Rect.Min.X)*4

		for x := 0; x < w; x++ {
			blendPix(dst.Pix[i:i+4:i+4], sr, sg, sb, a)
			i += 4
		}
	}
}

// fillMaskSolid blends a solid color through a single path mask — the hot
// path for text glyphs and clipped widget shapes.
func fillMaskSolid(dst *image.RGBA, cl image.Rectangle, m *clipNode, c color.NRGBA, opacity float32) {
	ca := uint32(float32(c.A)*opacity + .5)

	if ca == 0 {
		return
	}

	cr, cg, cb := uint32(c.R), uint32(c.G), uint32(c.B)
	opaque := ca == 255

	for y := cl.Min.Y; y < cl.Max.Y; y++ {
		preemptYield(y)
		mrow := m.mask.Pix[(y-m.maskMin.Y)*m.mask.Stride:]
		drow := dst.Pix[(y-dst.Rect.Min.Y)*dst.Stride:]

		for x := cl.Min.X; x < cl.Max.X; x++ {
			cov := uint32(mrow[x-m.maskMin.X])

			if cov == 0 {
				continue
			}

			i := (x - dst.Rect.Min.X) * 4

			// fully covered opaque pixels (the interior of every
			// solid shape) store directly, skipping the
			// read-modify-write blend
			if opaque && cov == 255 {
				drow[i] = c.R
				drow[i+1] = c.G
				drow[i+2] = c.B
				drow[i+3] = 255
				continue
			}

			a := mul255(ca, cov)

			if a == 0 {
				continue
			}

			blendPix(drow[i:i+4:i+4], mul255(cr, a), mul255(cg, a), mul255(cb, a), a)
		}
	}
}

// blendPix blends a premultiplied source pixel over pix (RGBA order).
func blendPix(pix []byte, sr, sg, sb, sa uint32) {
	if sa == 255 {
		pix[0], pix[1], pix[2], pix[3] = uint8(sr), uint8(sg), uint8(sb), 255
		return
	}

	inv := 255 - sa
	pix[0] = uint8(sr + mul255(uint32(pix[0]), inv))
	pix[1] = uint8(sg + mul255(uint32(pix[1]), inv))
	pix[2] = uint8(sb + mul255(uint32(pix[2]), inv))
	pix[3] = uint8(sa + mul255(uint32(pix[3]), inv))
}

// mul255 computes a*b/255 with rounding.
func mul255(a, b uint32) uint32 {
	t := a*b + 128
	return (t + t>>8) >> 8
}

// splitTransform separates the integer translation from a transform,
// keeping the fractional part (for subpixel-correct cached masks).
func splitTransform(t f32.Affine2D) (f32.Affine2D, image.Point) {
	sx, hx, ox, hy, sy, oy := t.Elems()

	iox, fox := math.Modf(float64(ox))
	ioy, foy := math.Modf(float64(oy))

	ft := f32.NewAffine2D(sx, hx, float32(fox), hy, sy, float32(foy))

	return ft, image.Pt(int(iox), int(ioy))
}

func axisAligned(t f32.Affine2D) bool {
	_, hx, _, hy, _, _ := t.Elems()
	return hx == 0 && hy == 0
}

// transformRect maps an axis-aligned rectangle through an axis-aligned
// transform.
func transformRect(r f32.Rectangle, t f32.Affine2D) f32.Rectangle {
	a := t.Transform(r.Min)
	b := t.Transform(r.Max)

	return f32.Rectangle{Min: a, Max: b}.Canon()
}

// transformBounds returns the bounding box of r under an arbitrary affine
// transform.
func transformBounds(r f32.Rectangle, t f32.Affine2D) f32.Rectangle {
	c0 := t.Transform(r.Min)
	c1 := t.Transform(f32.Point{X: r.Max.X, Y: r.Min.Y})
	c2 := t.Transform(r.Max)
	c3 := t.Transform(f32.Point{X: r.Min.X, Y: r.Max.Y})

	min := c0
	max := c0

	for _, c := range []f32.Point{c1, c2, c3} {
		if c.X < min.X {
			min.X = c.X
		}
		if c.Y < min.Y {
			min.Y = c.Y
		}
		if c.X > max.X {
			max.X = c.X
		}
		if c.Y > max.Y {
			max.Y = c.Y
		}
	}

	return f32.Rectangle{Min: min, Max: max}
}

// ---- op payload decoders (shadows of gpu's private decoders) ----

func decodeStrokeOp(data []byte) float32 {
	_ = data[4]
	bo := binary.LittleEndian
	return math.Float32frombits(bo.Uint32(data[1:]))
}

func decodeColorOp(data []byte) color.NRGBA {
	data = data[:ops.TypeColorLen]
	return color.NRGBA{R: data[1], G: data[2], B: data[3], A: data[4]}
}

type linearGradientOpData struct {
	stop1  f32.Point
	stop2  f32.Point
	color1 color.NRGBA
	color2 color.NRGBA
}

func decodeLinearGradientOp(data []byte) linearGradientOpData {
	data = data[:ops.TypeLinearGradientLen]
	bo := binary.LittleEndian

	return linearGradientOpData{
		stop1: f32.Point{
			X: math.Float32frombits(bo.Uint32(data[1:])),
			Y: math.Float32frombits(bo.Uint32(data[5:])),
		},
		stop2: f32.Point{
			X: math.Float32frombits(bo.Uint32(data[9:])),
			Y: math.Float32frombits(bo.Uint32(data[13:])),
		},
		color1: color.NRGBA{R: data[17], G: data[18], B: data[19], A: data[20]},
		color2: color.NRGBA{R: data[21], G: data[22], B: data[23], A: data[24]},
	}
}

// ---- path flattening and mask rasterization ----

// flattener accumulates transformed, flattened contours for rasterization.
type flattener struct {
	pts      []f32.Point
	starts   []int
	contour  uint32
	active   bool
	scratch  []stroke.QuadSegment
	min, max f32.Point
}

func (fl *flattener) reset() {
	fl.pts = fl.pts[:0]
	fl.starts = fl.starts[:0]
	fl.active = false
	inf := float32(math.Inf(1))
	fl.min = f32.Point{X: inf, Y: inf}
	fl.max = f32.Point{X: -inf, Y: -inf}
}

func (fl *flattener) grow(p f32.Point) {
	if p.X < fl.min.X {
		fl.min.X = p.X
	}
	if p.Y < fl.min.Y {
		fl.min.Y = p.Y
	}
	if p.X > fl.max.X {
		fl.max.X = p.X
	}
	if p.Y > fl.max.Y {
		fl.max.Y = p.Y
	}
}

// moveOrContinue starts a new contour unless from continues the current one.
func (fl *flattener) moveOrContinue(contour uint32, from f32.Point) {
	if fl.active && contour == fl.contour {
		last := fl.pts[len(fl.pts)-1]
		if last == from {
			return
		}
	}

	fl.starts = append(fl.starts, len(fl.pts))
	fl.pts = append(fl.pts, from)
	fl.grow(from)
	fl.contour = contour
	fl.active = true
}

func (fl *flattener) lineTo(p f32.Point) {
	fl.pts = append(fl.pts, p)
	fl.grow(p)
}

// quadTo flattens a quadratic Bézier into lines with ~0.2px tolerance.
func (fl *flattener) quadTo(ctrl, to f32.Point) {
	from := fl.pts[len(fl.pts)-1]

	// deviation of the control point from the chord midpoint bounds the
	// curve's distance from the chord
	dx := from.X - 2*ctrl.X + to.X
	dy := from.Y - 2*ctrl.Y + to.Y
	dev := float64(dx*dx + dy*dy)

	n := 1 + int(math.Sqrt(math.Sqrt(dev*4)))
	if n > 32 {
		n = 32
	}

	step := 1 / float32(n)

	for i := 1; i < n; i++ {
		u := float32(i) * step
		v := 1 - u
		p := f32.Point{
			X: v*v*from.X + 2*v*u*ctrl.X + u*u*to.X,
			Y: v*v*from.Y + 2*v*u*ctrl.Y + u*u*to.Y,
		}
		fl.lineTo(p)
	}

	fl.lineTo(to)
}

func (fl *flattener) quadSeg(contour uint32, q stroke.QuadSegment) {
	fl.moveOrContinue(contour, q.From)
	fl.quadTo(q.Ctrl, q.To)
}

// addPathData decodes gio-encoded path records into flattened contours,
// applying tr to every point.
func (fl *flattener) addPathData(pathData []byte, tr f32.Affine2D) {
	for len(pathData) >= scene.CommandSize+4 {
		contour := binary.LittleEndian.Uint32(pathData)
		cmd := ops.DecodeCommand(pathData[4 : 4+scene.CommandSize])

		switch cmd.Op() {
		case scene.OpLine:
			from, to := scene.DecodeLine(cmd)
			fl.moveOrContinue(contour, tr.Transform(from))
			fl.lineTo(tr.Transform(to))
		case scene.OpGap:
			from, to := scene.DecodeGap(cmd)
			fl.moveOrContinue(contour, tr.Transform(from))
			fl.lineTo(tr.Transform(to))
		case scene.OpQuad:
			from, ctrl, to := scene.DecodeQuad(cmd)
			fl.moveOrContinue(contour, tr.Transform(from))
			fl.quadTo(tr.Transform(ctrl), tr.Transform(to))
		case scene.OpCubic:
			from, ctrl0, ctrl1, to := scene.DecodeCubic(cmd)
			fl.scratch = stroke.SplitCubic(from, ctrl0, ctrl1, to, fl.scratch[:0])

			for _, q := range fl.scratch {
				q = q.Transform(tr)
				fl.quadSeg(contour, q)
			}
		default:
			// unsupported command; skip
		}

		pathData = pathData[scene.CommandSize+4:]
	}
}

// maskFor returns the cached (or freshly rasterized) coverage mask for an
// encoded path under the given transform.
func (r *Renderer) maskFor(pathData []byte, tr f32.Affine2D, outline bool, strokeWidth float32) *maskEntry {
	sx, hx, ox, hy, sy, oy := tr.Elems()

	mk := maskKey{
		hash: fnv1a(0, pathData),
		sx:   sx, hx: hx, ox: ox, hy: hy, sy: sy, oy: oy,
		outline:     outline,
		strokeWidth: strokeWidth,
	}

	if e, ok := r.cache[mk]; ok {
		if e != nil {
			e.frame = r.frame
		}
		return e
	}

	fl := &r.flat
	fl.reset()

	if strokeWidth > 0 {
		quads := stroke.StrokePathCommands(stroke.StrokeStyle{Width: strokeWidth}, pathData)

		for _, q := range quads {
			fl.quadSeg(q.Contour, q.Quad.Transform(tr))
		}
	} else {
		fl.addPathData(pathData, tr)
	}

	e := r.rasterize(fl)
	r.put(mk, e)

	return e
}

// maskForQuad rasterizes a transformed rectangle as a polygon mask (for
// rect clips under rotation/shear).
func (r *Renderer) maskForQuad(rect f32.Rectangle, tr f32.Affine2D) *maskEntry {
	sx, hx, ox, hy, sy, oy := tr.Elems()

	var rb [16]byte

	le := binary.LittleEndian
	le.PutUint32(rb[0:], math.Float32bits(rect.Min.X))
	le.PutUint32(rb[4:], math.Float32bits(rect.Min.Y))
	le.PutUint32(rb[8:], math.Float32bits(rect.Max.X))
	le.PutUint32(rb[12:], math.Float32bits(rect.Max.Y))

	mk := maskKey{
		hash: fnv1a(1, rb[:]),
		sx:   sx, hx: hx, ox: ox, hy: hy, sy: sy, oy: oy,
	}

	if e, ok := r.cache[mk]; ok {
		if e != nil {
			e.frame = r.frame
		}
		return e
	}

	fl := &r.flat
	fl.reset()

	fl.moveOrContinue(0, tr.Transform(rect.Min))
	fl.lineTo(tr.Transform(f32.Point{X: rect.Max.X, Y: rect.Min.Y}))
	fl.lineTo(tr.Transform(rect.Max))
	fl.lineTo(tr.Transform(f32.Point{X: rect.Min.X, Y: rect.Max.Y}))

	e := r.rasterize(fl)
	r.put(mk, e)

	return e
}

// put inserts a cache entry, tracking the byte budget. Nil entries
// (degenerate paths) are cached too, avoiding repeated flattening.
func (r *Renderer) put(mk maskKey, e *maskEntry) {
	if old, ok := r.cache[mk]; ok && old != nil {
		r.cacheBytes -= len(old.alpha.Pix)
	}

	r.cache[mk] = e

	if e != nil {
		r.cacheBytes += len(e.alpha.Pix)
	}
}

// maxMaskDim bounds a single mask's dimensions as a safety limit.
const maxMaskDim = 4096

// rasterize converts the flattener's contours into an alpha mask.
// It returns nil for empty or degenerate paths.
func (r *Renderer) rasterize(fl *flattener) *maskEntry {
	if len(fl.pts) == 0 || fl.min.X > fl.max.X {
		return nil
	}

	x0 := int(math.Floor(float64(fl.min.X)))
	y0 := int(math.Floor(float64(fl.min.Y)))
	x1 := int(math.Ceil(float64(fl.max.X))) + 1
	y1 := int(math.Ceil(float64(fl.max.Y))) + 1

	w, h := x1-x0, y1-y0

	if w <= 0 || h <= 0 || w > maxMaskDim || h > maxMaskDim {
		return nil
	}

	if w > r.scanW || h > r.scanH {
		if w > r.scanW {
			r.scanW = w
		}
		if h > r.scanH {
			r.scanH = h
		}
		r.scanner.SetBounds(r.scanW, r.scanH)
		r.scanner.SetWinding(true)
	}

	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	r.mspan.dst = mask

	off := f32.Point{X: float32(x0), Y: float32(y0)}

	for ci, start := range fl.starts {
		end := len(fl.pts)
		if ci+1 < len(fl.starts) {
			end = fl.starts[ci+1]
		}

		if end-start < 2 {
			continue
		}

		first := fl.pts[start]
		r.scanner.Start(toFixed(first, off))

		for _, p := range fl.pts[start+1 : end] {
			r.scanner.Line(toFixed(p, off))
		}

		// close the contour
		r.scanner.Line(toFixed(first, off))
	}

	r.scanner.Draw()
	r.scanner.Clear()

	return &maskEntry{
		alpha: mask,
		rect:  image.Rect(x0, y0, x1, y1),
		frame: r.frame,
	}
}

func toFixed(p, off f32.Point) fixed.Point26_6 {
	return fixed.Point26_6{
		X: fixed.Int26_6((p.X - off.X) * 64),
		Y: fixed.Int26_6((p.Y - off.Y) * 64),
	}
}

// maskSpanner writes coverage spans into an alpha mask.
type maskSpanner struct {
	dst *image.Alpha
}

func (s *maskSpanner) SetColor(any) {}

func (s *maskSpanner) GetSpanFunc() scan.SpanFunc {
	return s.span
}

func (s *maskSpanner) span(yi, xi0, xi1 int, alpha uint32) {
	// Clip to the mask bounds. The scanner is sized to the largest glyph seen
	// (its bounds only grow), so a narrower/shorter mask can otherwise receive
	// spans past its buffer -- an out-of-bounds heap write.
	b := s.dst.Rect
	if yi < b.Min.Y || yi >= b.Max.Y {
		return
	}
	if xi0 < b.Min.X {
		xi0 = b.Min.X
	}
	if xi1 > b.Max.X {
		xi1 = b.Max.X
	}
	if xi0 >= xi1 {
		return
	}

	a := uint8(alpha >> 8)
	row := s.dst.Pix[yi*s.dst.Stride:]

	for x := xi0; x < xi1; x++ {
		row[x] = a
	}
}

// SPDX-License-Identifier: Unlicense OR MIT

package swrender

import (
	"fmt"
	"image"
	"image/color"
	"runtime"
	"testing"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// dashFrame mirrors the bmx kernel dashboard: two rounded cards with
// label/value rows whose values change every frame (uptime, heap,
// frame counter) — the workload that leaked on hardware.
func dashFrame(r *Renderer, th *material.Theme, o *op.Ops, dst *image.RGBA, i int) {
	o.Reset()

	gtx := layout.Context{
		Ops:         o,
		Metric:      unit.Metric{PxPerDp: 1.5, PxPerSp: 1.5},
		Constraints: layout.Exact(dst.Bounds().Size()),
	}

	paint.Fill(o, color.NRGBA{R: 0x12, G: 0x14, B: 0x1c, A: 0xff})

	card := func(y int, rows [][2]string) {
		defer op.Offset(image.Pt(36, y)).Push(o).Pop()
		cl := clip.RRect{Rect: image.Rect(0, 0, 1200, 180), SE: 15, SW: 15, NE: 15, NW: 15}.Push(o)
		paint.Fill(o, color.NRGBA{R: 0x1e, G: 0x24, B: 0x32, A: 0xff})
		cl.Pop()

		for j, row := range rows {
			func() {
				defer op.Offset(image.Pt(24, 20+j*34)).Push(o).Pop()
				material.Body1(th, row[0]+"  "+row[1]).Layout(gtx)
			}()
		}
	}

	card(40, [][2]string{
		{"uptime", fmt.Sprintf("%.1f s", float64(i)*0.5)},
		{"heap", fmt.Sprintf("%d KiB", 1000+i*93)},
		{"goroutines", "4"},
		{"frame", fmt.Sprintf("#%d in %.1f ms", i, 60.0+float64(i%7))},
	})

	card(260, [][2]string{
		{"X", fmt.Sprintf("%8.3f mm", float64(i%50))},
		{"Y", fmt.Sprintf("%8.3f mm", float64(i%40))},
		{"Z", "   0.000 mm"},
		{"E", fmt.Sprintf("%8.3f mm", float64(i)*0.01)},
	})

	r.Frame(o, dst)
}

// TestDashboardSoak runs the dashboard for many frames and fails on
// live-set growth or frame-time drift.
func TestDashboardSoak(t *testing.T) {
	size := image.Pt(1280, 720)
	th := newTheme()
	r := New(size)
	dst := image.NewRGBA(image.Rectangle{Max: size})
	o := new(op.Ops)

	const frames = 1200
	live := make([]uint64, 0, 3)
	times := make([]time.Duration, frames)

	for i := 0; i < frames; i++ {
		t0 := time.Now()
		dashFrame(r, th, o, dst, i)
		times[i] = time.Since(t0)

		if i == 199 || i == 599 || i == 1199 {
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			live = append(live, m.HeapInuse)
			t.Logf("frame %4d: live heap %6d KiB, cache %d entries / %d KiB",
				i+1, m.HeapInuse/1024, len(r.cache), r.cacheBytes/1024)
		}
	}

	avg := func(a []time.Duration) time.Duration {
		var s time.Duration
		for _, d := range a {
			s += d
		}
		return s / time.Duration(len(a))
	}

	early := avg(times[100:200])
	late := avg(times[frames-100:])
	t.Logf("frame time: early %v, late %v", early, late)

	// live set must not keep growing between the 600- and 1200-frame marks
	if g := int64(live[2]) - int64(live[1]); g > 2<<20 {
		t.Errorf("live heap grew %d KiB between frame 600 and 1200 (leak)", g/1024)
	}

	if late > early*3/2 {
		t.Errorf("frame time drifted: early %v -> late %v", early, late)
	}
}

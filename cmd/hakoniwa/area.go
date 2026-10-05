package main

// Detection of the active picture area inside the captured frame.
//
// When the target's resolution has a different aspect ratio from the capture
// format (e.g. a 16:10 desktop in 1920x1080), the source pads it with black
// bars. Absolute mouse coordinates (0..32767) cover the target desktop, not
// the bars, so the browser must map pointer positions to this area.
//
// Detection is conservative: bars must be (near) pure black, symmetric, and
// the picture edge just inside them must be mostly non-black. A dark screen
// with sparse content (BIOS text, a black desktop) therefore keeps the last
// accepted area instead of shrinking it to the content.

import (
	"bytes"
	"image"
	"image/jpeg"
	"sync"
	"time"
)

// area is the picture region as fractions of the frame (0..1).
type area struct {
	X, Y, W, H float64
}

var fullArea = area{0, 0, 1, 1}

const (
	barLuma       = 24   // max luma of a bar pixel
	edgeFill      = 0.6  // min fraction of non-black samples on the picture edge
	symmetrySlack = 0.01 // max difference between opposite bars (fraction of frame)
)

type areaDetector struct {
	mu        sync.Mutex
	cur       area
	candidate area
}

func newAreaDetector() *areaDetector { return &areaDetector{cur: fullArea, candidate: fullArea} }

func (d *areaDetector) get() area {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur
}

// run re-detects the area from the latest frame every interval.
func (d *areaDetector) run(h *hub, interval time.Duration) {
	for range time.Tick(interval) {
		b, _, _ := h.latest()
		if b == nil {
			continue
		}
		img, err := jpeg.Decode(bytes.NewReader(frameJPEG(b)))
		if err != nil {
			continue
		}
		a, ok := detectArea(img)
		d.mu.Lock()
		if ok && a == d.candidate {
			d.cur = a // seen twice in a row
		}
		if ok {
			d.candidate = a
		}
		d.mu.Unlock()
	}
}

func luma(img image.Image, x, y int) uint8 {
	if yc, ok := img.(*image.YCbCr); ok {
		return yc.Y[yc.YOffset(x, y)]
	}
	r, g, b, _ := img.At(x, y).RGBA()
	return uint8((299*r + 587*g + 114*b) / 1000 >> 8)
}

func detectArea(img image.Image) (area, bool) {
	bnd := img.Bounds()
	W, H := bnd.Dx(), bnd.Dy()
	const step = 4
	// Fraction of non-black samples on column x / row y.
	colFill := func(x int) float64 {
		n, lit := 0, 0
		for y := bnd.Min.Y; y < bnd.Max.Y; y += step {
			n++
			if luma(img, bnd.Min.X+x, y) > barLuma {
				lit++
			}
		}
		return float64(lit) / float64(n)
	}
	rowFill := func(y int) float64 {
		n, lit := 0, 0
		for x := bnd.Min.X; x < bnd.Max.X; x += step {
			n++
			if luma(img, x, bnd.Min.Y+y) > barLuma {
				lit++
			}
		}
		return float64(lit) / float64(n)
	}
	// First non-black column/row from each side, limited to a quarter of the frame.
	scan := func(n int, fill func(int) float64, from, dir int) (int, bool) {
		for i := 0; i < n/4; i++ {
			if fill(from+i*dir) > 0 {
				return i, true
			}
		}
		return 0, false
	}
	l, ok1 := scan(W, colFill, 0, 1)
	r, ok2 := scan(W, colFill, W-1, -1)
	t, ok3 := scan(H, rowFill, 0, 1)
	b, ok4 := scan(H, rowFill, H-1, -1)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return area{}, false // mostly black frame
	}
	if abs(l-r) > int(symmetrySlack*float64(W)) || abs(t-b) > int(symmetrySlack*float64(H)) {
		return area{}, false
	}
	// The picture edge just inside the bars must be solid.
	in := 2
	if l > 0 && (colFill(l+in) < edgeFill || colFill(W-1-r-in) < edgeFill) {
		return area{}, false
	}
	if t > 0 && (rowFill(t+in) < edgeFill || rowFill(H-1-b-in) < edgeFill) {
		return area{}, false
	}
	return area{
		X: float64(l) / float64(W),
		Y: float64(t) / float64(H),
		W: float64(W-l-r) / float64(W),
		H: float64(H-t-b) / float64(H),
	}, true
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

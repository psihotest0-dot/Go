package ingest

import (
	"image"
	"image/color"
	"math"
	"sort"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
)

// Preparing a phone photo of a page for Tesseract. Measured on 2026-11-27 on the owner's photos of a
// loan schedule and an insurance price list (2048 px, and the same shrunk to Telegram's 1280 px): a
// photo as it is loses the digits to the table's rules and to the uneven light (3 and 8, 5 and 9, 6
// and 0), and --psm 3 reads a table column by column. Grey, scaled, the light flattened, the rules
// taken out, black and white, then --psm 6: whole rows of the loan table 14 -> 50 of 84, its numbers
// 307 -> 448 of 504, the words 341 -> 364 of 382; of the 1280 px photos the insurance amounts 5 -> 17
// of 29. The numbers below are the measured ones; k is the longest side over 2048, so that they
// follow the size the page is scaled to.

// photoPrep is one way of preparing a photo: the size of its longest side, and whether the page is
// turned straight first.
type photoPrep struct {
	name    string
	longest int
	deskew  bool
}

// photoPreps are both read, and the one Tesseract is surer of (the mean confidence of its words) is
// kept: one size alone is not safe on every page - at 2816 px a page ran its rows together (188 of
// 504 numbers on their rows), and on the 1280 px photos the straightened one alone got 340. On the
// 2048 px photos either alone was as good as the pick (421 and 424 against 418); on the 1280 px ones
// the pick held (419) where one of them fell.
var photoPreps = []photoPrep{
	{name: "scaled", longest: 2560},
	{name: "straightened", longest: 2304, deskew: true},
}

// maxLetter is the tallest letter (px) a prepared page may have. The sizes of photoPreps are for a
// photo of a whole page: there the letters come out 20-29 px tall (the 80th percentile of the blobs,
// on all ten measured photos). A photo taken close up would come out with letters of 100 px and more,
// which Tesseract misreads: such a page is prepared again at the size that makes them maxLetter (up
// to twice: big letters break into pieces at the first size, so the first guess is too big).
const maxLetter = 32

// preparePhoto returns the black and white page Tesseract reads.
func preparePhoto(src image.Image, p photoPrep) *image.Gray {
	b := src.Bounds()
	floor := min(max(b.Dx(), b.Dy()), photoMinSide)
	size := p.longest
	g := preparePage(src, p, size, 0)
	for range 2 {
		hs := letterHeights(g)
		if len(hs) < 50 {
			break
		}
		letter := hs[len(hs)*80/100]
		if letter <= maxLetter*9/8 {
			break
		}
		next := max(size*maxLetter/letter, floor)
		if next >= size {
			break
		}
		size = next
		g = preparePage(src, p, size, maxLetter)
	}
	return g
}

// preparePage scales src so that its longest side is longest and makes it black and white. The sizes
// of the filters follow the page (k); when the height of its letters is known (letter > 0), the light
// filter is never narrower than half a letter (a stroke must not count as paper) and a rule never
// shorter than three letters (a word must not count as a rule).
func preparePage(src image.Image, p photoPrep, longest, letter int) *image.Gray {
	b := src.Bounds()
	s := float64(longest) / float64(max(b.Dx(), b.Dy()))
	k := float64(longest) / 2048
	w, h := int(float64(b.Dx())*s+0.5), int(float64(b.Dy())*s+0.5)
	flatR, minRun, dil := int(12*k), int(70*k), int(2*k+0.5)
	if letter > 0 {
		flatR, minRun, dil = max(flatR, letter/2), max(minRun, 3*letter), max(dil, 1)
	}
	g := image.NewGray(image.Rect(0, 0, w, h))
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(g, g.Rect, src, b.Min, draw.Src)
	} else {
		draw.CatmullRom.Scale(g, g.Rect, src, b, draw.Src, nil)
	}
	flattenLight(g, flatR)
	if p.deskew {
		if a := skewAngle(darkMask(g, 85), w, h, 5, 0.1); a != 0 {
			g = rotateGray(g, a)
		}
	}
	m := darkMask(g, 85)
	removeRules(m, w, h, minRun, 1, dil)
	for i, dark := range m {
		if dark {
			g.Pix[i] = 0
		} else {
			g.Pix[i] = 255
		}
	}
	return g
}

// letterHeights are the heights, sorted, of the blobs of a black and white page that look like
// letters (4-connected ink, taller than 4 px, at most three heights wide, under an eighth of the page).
func letterHeights(g *image.Gray) []int {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	seen := make([]bool, len(g.Pix))
	var hs []int
	stack := make([]int, 0, 1024)
	for i, v := range g.Pix {
		if v != 0 || seen[i] {
			continue
		}
		seen[i] = true
		stack = append(stack[:0], i)
		x0, x1, y0, y1 := w, -1, h, -1
		for len(stack) > 0 {
			q := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := q%w, q/w
			x0, x1, y0, y1 = min(x0, x), max(x1, x), min(y0, y), max(y1, y)
			for _, n := range [4]int{q - 1, q + 1, q - w, q + w} {
				if n < 0 || n >= len(g.Pix) || g.Pix[n] != 0 || seen[n] ||
					(n == q-1 && x == 0) || (n == q+1 && x == w-1) {
					continue
				}
				seen[n] = true
				stack = append(stack, n)
			}
		}
		if bh, bw := y1-y0+1, x1-x0+1; bh > 4 && bw <= 3*bh && bh < h/8 {
			hs = append(hs, bh)
		}
	}
	sort.Ints(hs)
	return hs
}

// flattenLight divides each pixel by the brightness of the paper around it - a max filter of radius
// r (it takes the strokes away) then a box blur of the same radius - so a shadow over half the page
// is no darker than the rest.
func flattenLight(g *image.Gray, r int) {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	bg := boxBlur(maxFilter(g.Pix, w, h, r), w, h, r)
	for i, p := range g.Pix {
		v := 255
		if bg[i] > 0 {
			v = int(p) * 255 / int(bg[i])
		}
		g.Pix[i] = uint8(min(v, 255))
	}
}

// maxFilter is the brightest pixel of the (2r+1)-square around each one, every second pixel of the
// square looked at (strokes are wider than that at these sizes).
func maxFilter(p []uint8, w, h, r int) []uint8 {
	tmp := make([]uint8, len(p))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m := uint8(0)
			for k := max(0, x-r); k <= min(w-1, x+r); k += 2 {
				m = max(m, p[y*w+k])
			}
			tmp[y*w+x] = m
		}
	}
	out := make([]uint8, len(p))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			m := uint8(0)
			for k := max(0, y-r); k <= min(h-1, y+r); k += 2 {
				m = max(m, tmp[k*w+x])
			}
			out[y*w+x] = m
		}
	}
	return out
}

// boxBlur is the mean of the (2r+1)-square around each pixel (from an integral image).
func boxBlur(p []uint8, w, h, r int) []uint8 {
	ii := make([]int64, (w+1)*(h+1))
	for y := 0; y < h; y++ {
		var row int64
		for x := 0; x < w; x++ {
			row += int64(p[y*w+x])
			ii[(y+1)*(w+1)+x+1] = ii[y*(w+1)+x+1] + row
		}
	}
	out := make([]uint8, len(p))
	for y := 0; y < h; y++ {
		y0, y1 := max(0, y-r), min(h, y+r+1)
		for x := 0; x < w; x++ {
			x0, x1 := max(0, x-r), min(w, x+r+1)
			s := ii[y1*(w+1)+x1] - ii[y0*(w+1)+x1] - ii[y1*(w+1)+x0] + ii[y0*(w+1)+x0]
			out[y*w+x] = uint8(s / int64((y1-y0)*(x1-x0)))
		}
	}
	return out
}

// darkMask marks the ink: pixels darker than pct % of the mean of the 31-pixel square around them.
func darkMask(g *image.Gray, pct int) []bool {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	mean := boxBlur(g.Pix, w, h, 15)
	m := make([]bool, len(g.Pix))
	for i, p := range g.Pix {
		m[i] = int(p)*100 < int(mean[i])*pct
	}
	return m
}

// removeRules takes the rules of a table out of the ink mask: runs of ink at least minRun long along
// a row or a column. A rule on a photo is a little slanted, so a run may drift dil pixels across (it
// is looked for in the mask widened by dil); only ink is taken, and grow pixels on either side of it
// across the run (the soft edge of the rule). Text has no runs that long: its letters stay.
func removeRules(m []bool, w, h, minRun, grow, dil int) {
	mv := make([]bool, len(m)) // widened up and down: finds the slanted horizontal rules
	mh := make([]bool, len(m)) // widened left and right: the vertical ones
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for d := -dil; d <= dil; d++ {
				if yy := y + d; yy >= 0 && yy < h && m[yy*w+x] {
					mv[y*w+x] = true
				}
				if xx := x + d; xx >= 0 && xx < w && m[y*w+xx] {
					mh[y*w+x] = true
				}
			}
		}
	}
	kill := make([]bool, len(m))
	for y := 0; y < h; y++ {
		for x := 0; x < w; {
			if !mv[y*w+x] {
				x++
				continue
			}
			s := x
			for x < w && mv[y*w+x] {
				x++
			}
			if x-s < minRun {
				continue
			}
			for k := s; k < x; k++ {
				if !m[y*w+k] {
					continue
				}
				for dy := -grow; dy <= grow; dy++ {
					if yy := y + dy; yy >= 0 && yy < h {
						kill[yy*w+k] = true
					}
				}
			}
		}
	}
	for x := 0; x < w; x++ {
		for y := 0; y < h; {
			if !mh[y*w+x] {
				y++
				continue
			}
			s := y
			for y < h && mh[y*w+x] {
				y++
			}
			if y-s < minRun {
				continue
			}
			for k := s; k < y; k++ {
				if !m[k*w+x] {
					continue
				}
				for dx := -grow; dx <= grow; dx++ {
					if xx := x + dx; xx >= 0 && xx < w {
						kill[k*w+xx] = true
					}
				}
			}
		}
	}
	for i, k := range kill {
		if k {
			m[i] = false
		}
	}
}

// skewAngle is the turn of the page (radians, within maxDeg either way, in steps of stepDeg) at which
// its ink lines up into the sharpest rows: the profile of the sheared ink, in 2-pixel bins, has the
// largest sum of squares there. Every second pixel each way is looked at.
func skewAngle(m []bool, w, h int, maxDeg, stepDeg float64) float64 {
	type pt struct{ x, y float64 }
	var pts []pt
	for y := 0; y < h; y += 2 {
		for x := 0; x < w; x += 2 {
			if m[y*w+x] {
				pts = append(pts, pt{float64(x), float64(y)})
			}
		}
	}
	best, bestScore := 0.0, -1.0
	bins := make([]float64, h+2*w)
	for d := -maxDeg; d <= maxDeg+1e-9; d += stepDeg {
		t := math.Tan(d * math.Pi / 180)
		clear(bins)
		for _, p := range pts {
			if k := int((p.y-p.x*t)/2) + w; k >= 0 && k < len(bins) {
				bins[k]++
			}
		}
		var s float64
		for _, b := range bins {
			s += b * b
		}
		if s > bestScore {
			bestScore, best = s, d
		}
	}
	return best * math.Pi / 180
}

// rotateGray turns g by -a around its centre; the corners it uncovers are white.
func rotateGray(g *image.Gray, a float64) *image.Gray {
	w, h := float64(g.Rect.Dx()), float64(g.Rect.Dy())
	out := image.NewGray(g.Rect)
	draw.Draw(out, out.Rect, &image.Uniform{color.Gray{Y: 255}}, image.Point{}, draw.Src)
	c, s := math.Cos(-a), math.Sin(-a)
	cx, cy := w/2, h/2
	m := f64.Aff3{c, -s, cx - c*cx + s*cy, s, c, cy - s*cx - c*cy}
	draw.BiLinear.Transform(out, m, g, g.Rect, draw.Over, nil)
	return out
}

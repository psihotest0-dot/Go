package ingest

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

func TestTheRulesOfATableGoAndTheLettersStay(t *testing.T) {
	const w, h = 400, 200
	m := make([]bool, w*h)
	set := func(x, y int) {
		if x >= 0 && x < w && y >= 0 && y < h {
			m[y*w+x] = true
		}
	}
	var rule []int
	// A horizontal rule 3 px thick that drops a pixel every 60 px (a slanted photo), and a vertical
	// one 2 px thick that moves a pixel every 50 px.
	for x := 10; x < 390; x++ {
		for d := 0; d < 3; d++ {
			y := 120 + x/60 + d
			set(x, y)
			rule = append(rule, y*w+x)
		}
	}
	for y := 10; y < 190; y++ {
		for d := 0; d < 2; d++ {
			x := 250 + y/50 + d
			set(x, y)
			rule = append(rule, y*w+x)
		}
	}
	// A letter (a 10 x 14 ring) and a comma 4 px above the rule.
	var keep []int
	for y := 60; y < 74; y++ {
		for x := 50; x < 60; x++ {
			if y < 62 || y > 71 || x < 52 || x > 57 {
				set(x, y)
				keep = append(keep, y*w+x)
			}
		}
	}
	for y := 113; y < 116; y++ {
		for x := 150; x < 152; x++ {
			set(x, y)
			keep = append(keep, y*w+x)
		}
	}

	removeRules(m, w, h, 70, 1, 2)

	for _, i := range rule {
		require.False(t, m[i], "rule pixel at (%d,%d) is still there", i%w, i/w)
	}
	for _, i := range keep {
		require.True(t, m[i], "letter pixel at (%d,%d) was taken", i%w, i/w)
	}
}

func TestTheTurnOfThePageIsFoundAndUndone(t *testing.T) {
	const w, h = 600, 400
	g := image.NewGray(image.Rect(0, 0, w, h))
	for i := range g.Pix {
		g.Pix[i] = 255
	}
	slope := math.Tan(2 * math.Pi / 180)
	// Ten rows of "words": dashes 3 px high along a line going down by 2 degrees.
	for r := 0; r < 10; r++ {
		for x := 20; x < 580; x++ {
			if (x/12)%3 == 2 {
				continue
			}
			for d := 0; d < 3; d++ {
				y := 40 + r*32 + int(float64(x)*slope) + d
				g.Pix[y*w+x] = 0
			}
		}
	}
	m := make([]bool, len(g.Pix))
	for i, p := range g.Pix {
		m[i] = p < 128
	}
	a := skewAngle(m, w, h, 5, 0.1)
	assert.InDelta(t, 2.0, a*180/math.Pi, 0.15)

	// Turned back, the first row is level: its ink at the left and at the right is at the same height.
	st := rotateGray(g, a)
	rowY := func(x0, x1 int) float64 {
		var sum, n float64
		for y := 0; y < 90; y++ {
			for x := x0; x < x1; x++ {
				if st.Pix[y*w+x] < 128 {
					sum += float64(y)
					n++
				}
			}
		}
		return sum / n
	}
	assert.InDelta(t, rowY(60, 160), rowY(440, 540), 1.5)
}

func TestAShadowOverHalfThePageDoesNotTurnThePaperIntoInk(t *testing.T) {
	const w, h = 400, 120
	g := image.NewGray(image.Rect(0, 0, w, h))
	ink := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			paper := 250 - 130*x/w // from white to a dark grey on the right
			v := paper
			if (x/20)%2 == 0 && y >= 50 && y < 64 && (x%20) >= 4 && (x%20) < 8 {
				v = paper * 30 / 100 // a stroke 4 px wide
				ink[y*w+x] = true
			}
			g.Pix[y*w+x] = uint8(v)
		}
	}
	flattenLight(g, 12)
	m := darkMask(g, 85)
	var lostInk, falseInk int
	for i := range m {
		switch {
		case ink[i] && !m[i]:
			lostInk++
		case !ink[i] && m[i]:
			falseInk++
		}
	}
	assert.Zero(t, falseInk, "paper marked as ink")
	assert.Zero(t, lostInk, "ink lost")
}

func TestAPreparedPhotoIsBlackAndWhiteAtTheSizeOfItsWay(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 500))
	draw.Draw(src, src.Rect, &image.Uniform{color.RGBA{230, 225, 220, 255}}, image.Point{}, draw.Src)
	draw.Draw(src, image.Rect(100, 100, 300, 120), &image.Uniform{color.Black}, image.Point{}, draw.Src)
	for _, p := range photoPreps {
		g := preparePhoto(src, p)
		assert.Equal(t, p.longest, g.Rect.Dx(), p.name)
		assert.Equal(t, p.longest/2, g.Rect.Dy(), p.name)
		for _, v := range g.Pix {
			if v != 0 && v != 255 {
				t.Fatalf("%s: grey level %d in a black and white page", p.name, v)
			}
		}
	}
}

func TestTheLanguagesFollowTheScriptOfThePage(t *testing.T) {
	bills := NewImageOCRExtractor().WithLanguages("deu+eng+bul")
	for script, want := range map[string]string{"Latin": "deu+eng", "Cyrillic": "bul+eng", "": "deu+eng+bul", "Arabic": "deu+eng+bul"} {
		e := *bills
		e.scriptFn = func(context.Context, []byte) string { return script }
		assert.Equal(t, want, e.photoLanguages(t.Context(), nil), script)
	}
	lib := NewImageOCRExtractor()
	lib.scriptFn = func(context.Context, []byte) string { return "Latin" }
	assert.Equal(t, "eng", lib.photoLanguages(t.Context(), nil))

	// One script only: nothing to narrow, Tesseract is not asked.
	de := NewImageOCRExtractor().WithLanguages("deu+eng")
	de.scriptFn = func(context.Context, []byte) string { panic("asked") }
	assert.Equal(t, "deu+eng", de.photoLanguages(t.Context(), nil))
}

func TestTheReportsOfTesseractAreRead(t *testing.T) {
	assert.Equal(t, "Cyrillic", parseScript("Page number: 0\nOrientation in degrees: 0\nRotate: 0\nOrientation confidence: 2.09\nScript: Cyrillic\nScript confidence: 7.75\n"))
	assert.Equal(t, "", parseScript("Too few characters. Skipping this page\n"))

	tsv := "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"1\t1\t0\t0\t0\t0\t0\t0\t2560\t2560\t-1\t\n" +
		"4\t1\t1\t1\t1\t0\t10\t10\t500\t30\t-1\t\n" +
		"5\t1\t1\t1\t1\t1\t10\t10\t100\t30\t96.5\t15.03.2019\n" +
		"5\t1\t1\t1\t1\t2\t120\t10\t100\t30\t81.5\t7.215,40\n" +
		"5\t1\t1\t1\t1\t3\t240\t10\t100\t30\t40\t \n"
	assert.InDelta(t, 89.0, meanWordConfidence([]byte(tsv)), 1e-9)
	assert.Zero(t, meanWordConfidence(nil))
}

// pagePhoto is a light photo-like picture with some dark marks, as a JPEG.
func pagePhoto(t *testing.T, w, h int, paper uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, &image.Uniform{color.RGBA{paper, paper, paper, 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(w/10, h/10, w/2, h/10+8), &image.Uniform{color.Black}, image.Point{}, draw.Src)
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}))
	return b.Bytes()
}

func TestTheReadingTesseractIsSurerOfIsKept(t *testing.T) {
	data := pagePhoto(t, 800, 600, 220)
	byWay := func(conf map[int]float64, fail map[int]bool) photoRun {
		return func(_ context.Context, page []byte, langs string) (string, float64, error) {
			img, err := png.Decode(bytes.NewReader(page))
			require.NoError(t, err)
			assert.Equal(t, "deu+eng", langs)
			long := img.Bounds().Dx()
			if fail[long] {
				return "", 0, errors.New("tesseract crashed")
			}
			return "page read at " + strings.Repeat("x", long/1000), conf[long], nil
		}
	}
	e := NewImageOCRExtractor().WithLanguages("deu+eng+bul").WithPhotoCleanup()
	e.scriptFn = func(context.Context, []byte) string { return "Latin" }

	e.photoRunFn = byWay(map[int]float64{2560: 70, 2304: 80}, nil)
	text, err := e.Extract(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "page read at xx", text, "the straightened page (2304 px) was surer")

	e.photoRunFn = byWay(map[int]float64{2560: 70, 2304: 80}, map[int]bool{2304: true})
	text, err = e.Extract(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "page read at xx", text, "the scaled page (2560 px) when the other failed")
}

func TestAPictureThatIsNoPhotoOfAPageIsLeftToThePlainReading(t *testing.T) {
	e := NewImageOCRExtractor().WithPhotoCleanup()
	e.photoRunFn = func(context.Context, []byte, string) (string, float64, error) { panic("read as a photo") }
	e.scriptFn = func(context.Context, []byte) string { return "" }
	for name, data := range map[string][]byte{
		"dark":     pagePhoto(t, 800, 600, 40),
		"small":    pagePhoto(t, 300, 200, 220),
		"no image": []byte("%PDF-1.4 not a picture"),
	} {
		_, err := e.extractPhoto(t.Context(), data)
		assert.ErrorIs(t, err, errPhotoSkipped, name)
	}
}

func TestBothReadingsFailingIsAnErrorNotAnEmptyText(t *testing.T) {
	e := NewImageOCRExtractor().WithPhotoCleanup()
	e.scriptFn = func(context.Context, []byte) string { return "" }
	e.photoRunFn = func(context.Context, []byte, string) (string, float64, error) { return "  \n", 90, nil }
	_, err := e.extractPhoto(t.Context(), pagePhoto(t, 800, 600, 220))
	require.Error(t, err)
	assert.NotErrorIs(t, err, errPhotoSkipped)
}

func TestTheLibraryExtractorIsNotAPhotoReader(t *testing.T) {
	assert.False(t, NewImageOCRExtractor().photo)
	assert.False(t, NewImageOCRExtractor().WithLanguages("deu+eng+bul").photo)
	img := NewImageOCRExtractor()
	assert.True(t, img.WithPhotoCleanup().photo)
	assert.False(t, img.photo, "WithPhotoCleanup returns a copy")
}

// A photo of a price list taken close up: big letters, the paper darker on one side, a rule under
// each row, turned by a degree and a half. Read by the real Tesseract (skipped without it), as a
// photo and as before (made-up numbers; the font is a 7 x 13 bitmap blown up five times). On
// 2026-11-27 the photo reading found 6 of the 7 (the last date came out as 15,05,2019), the plain
// one 3: no date (15,03,2019 ...) and 35,14 as 35.44.
func TestAPhotoTakenCloseUpIsReadBetterThanBefore(t *testing.T) {
	requireTools(t, "tesseract")
	if !OCRLanguagesInstalled(t.Context(), "deu+eng") {
		t.Skip("deu.traineddata is not installed")
	}
	rows := []string{"Datum      Rate     Zinsen", "15.03.2019  412,70   36,25", "15.04.2019  412,70   35,90", "15.05.2019  412,70   35,14"}
	small := image.NewGray(image.Rect(0, 0, 220, 80))
	draw.Draw(small, small.Rect, image.White, image.Point{}, draw.Src)
	d := font.Drawer{Dst: small, Src: image.Black, Face: basicfont.Face7x13}
	for i, r := range rows {
		d.Dot = fixed.P(8, 16+i*17)
		d.DrawString(r)
		for x := 4; x < 216; x++ { // a rule under each row
			small.Pix[(20+i*17)*220+x] = 0
		}
	}
	big := image.NewGray(image.Rect(0, 0, 1100, 400))
	draw.NearestNeighbor.Scale(big, big.Rect, small, small.Rect, draw.Src, nil)
	turned := rotateGray(big, -1.5*math.Pi/180)
	photo := image.NewRGBA(turned.Rect)
	for y := 0; y < turned.Rect.Dy(); y++ {
		for x := 0; x < turned.Rect.Dx(); x++ {
			v := int(turned.Pix[y*turned.Rect.Dx()+x]) * (240 - 80*x/turned.Rect.Dx()) / 255
			photo.Set(x, y, color.RGBA{uint8(v), uint8(v), uint8(v), 255})
		}
	}
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, photo, &jpeg.Options{Quality: 85}))

	// The page is prepared with letters of about maxLetter, not the 100 px the size of a whole page gives.
	hs := letterHeights(preparePhoto(photo, photoPreps[0]))
	require.NotEmpty(t, hs)
	assert.LessOrEqual(t, hs[len(hs)*80/100], maxLetter*9/8)

	found := func(text string) int {
		n := 0
		for _, want := range []string{"412,70", "36,25", "35,90", "35,14", "15.03.2019", "15.04.2019", "15.05.2019"} {
			if strings.Contains(text, want) {
				n++
			}
		}
		return n
	}
	asPhoto, err := NewImageOCRExtractor().WithLanguages("deu+eng").WithPhotoCleanup().Extract(context.Background(), bytes.NewReader(b.Bytes()))
	require.NoError(t, err)
	plain, err := NewImageOCRExtractor().WithLanguages("deu+eng").Extract(context.Background(), bytes.NewReader(b.Bytes()))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, found(asPhoto), 5, asPhoto)
	assert.Greater(t, found(asPhoto), found(plain), "as a photo: %q, as before: %q", asPhoto, plain)
}

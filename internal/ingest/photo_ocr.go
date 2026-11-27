package ingest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // phone photos
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go-monolith/metrics"
)

// A photo smaller than photoMinSide is no page of a document; one whose median grey is below
// photoDarkMedian is a dark screen or a dark scene, not a sheet of paper (the light flattening and the
// black and white of the photo path are made for dark ink on light paper). Both are read as before.
const (
	photoMinSide    = 400
	photoDarkMedian = 90
)

// errPhotoSkipped says the photo path left a picture to the plain one.
var errPhotoSkipped = errors.New("not a photo of a page")

// photoRun reads one prepared page in langs with --psm 6: its text and the mean confidence of its words.
type photoRun func(ctx context.Context, page []byte, langs string) (text string, conf float64, err error)

// scriptOf tells the script of a picture by Tesseract's own detection ("Latin", "Cyrillic"; "" when
// it cannot tell).
type scriptOf func(ctx context.Context, data []byte) string

// WithPhotoCleanup is the extractor that reads a picture as a phone photo of a page (photo_prep.go):
// prepared two ways, each read with --psm 6, the surer kept; the languages narrowed to the script of
// the page. A picture that is no such photo, or that this path fails on, is read as before.
func (e *ImageOCRExtractor) WithPhotoCleanup() *ImageOCRExtractor {
	c := *e
	c.photo = true
	return &c
}

// extractPhoto is the photo path of Extract.
func (e *ImageOCRExtractor) extractPhoto(ctx context.Context, data []byte) (string, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		metrics.OCRPhotos.WithLabelValues("skipped_format").Inc()
		return "", fmt.Errorf("%w: %v", errPhotoSkipped, err)
	}
	b := img.Bounds()
	if max(b.Dx(), b.Dy()) < photoMinSide {
		metrics.OCRPhotos.WithLabelValues("skipped_small").Inc()
		return "", errPhotoSkipped
	}
	if medianGrey(img) < photoDarkMedian {
		metrics.OCRPhotos.WithLabelValues("skipped_dark").Inc()
		return "", errPhotoSkipped
	}

	started := time.Now()
	langsCh := make(chan string, 1)
	go func() { langsCh <- e.photoLanguages(ctx, data) }()

	type result struct {
		text string
		conf float64
		err  error
	}
	results := make([]result, len(photoPreps))
	pages := make([][]byte, len(photoPreps))
	var wg sync.WaitGroup
	for i, p := range photoPreps {
		wg.Go(func() {
			var buf bytes.Buffer
			enc := png.Encoder{CompressionLevel: png.BestSpeed}
			if err := enc.Encode(&buf, preparePhoto(img, p)); err != nil {
				results[i].err = err
				return
			}
			pages[i] = buf.Bytes()
		})
	}
	wg.Wait()
	langs := <-langsCh

	run := e.photoRunFn
	if run == nil {
		run = runTesseractPage
	}
	for i := range photoPreps {
		if results[i].err != nil {
			continue
		}
		wg.Go(func() {
			release, err := acquireOCR(ctx)
			if err != nil {
				results[i].err = err
				return
			}
			defer release()
			text, conf, err := run(ctx, pages[i], langs)
			results[i] = result{text: text, conf: conf, err: err}
		})
	}
	wg.Wait()

	best := -1
	attrs := []any{slog.String("langs", langs), slog.Duration("took", time.Since(started))}
	for i, r := range results {
		if r.err != nil {
			attrs = append(attrs, slog.String(photoPreps[i].name, "error: "+r.err.Error()))
			continue
		}
		attrs = append(attrs, slog.Float64(photoPreps[i].name, r.conf))
		if strings.TrimSpace(r.text) != "" && (best < 0 || r.conf > results[best].conf) {
			best = i
		}
	}
	if best < 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		metrics.OCRPhotos.WithLabelValues("failed").Inc()
		slog.WarnContext(ctx, "photo OCR found no text, reading the picture as it is", attrs...)
		return "", fmt.Errorf("photo OCR: no text in either preparation")
	}
	metrics.OCRPhotos.WithLabelValues(photoPreps[best].name).Inc()
	slog.InfoContext(ctx, "photo OCR", append(attrs, slog.String("kept", photoPreps[best].name))...)
	return JoinHyphenated(results[best].text), nil
}

// medianGrey is the median grey level of a 64 x 64 grid over the picture.
func medianGrey(img image.Image) uint8 {
	b := img.Bounds()
	const n = 64
	v := make([]int, 0, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			x := b.Min.X + (2*j+1)*b.Dx()/(2*n)
			y := b.Min.Y + (2*i+1)*b.Dy()/(2*n)
			v = append(v, int(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y))
		}
	}
	sort.Ints(v)
	return uint8(v[len(v)/2])
}

// cyrillicLangs are Tesseract's languages written in Cyrillic.
var cyrillicLangs = map[string]bool{"bul": true, "rus": true, "ukr": true, "srp": true, "mkd": true, "bel": true}

// photoLanguages narrows the extractor's languages to the script of the page: a German letter read
// with Bulgarian too comes back with Cyrillic in its margins (183 such letters on the five photos)
// and a name in Cyrillic letters, a Bulgarian one read with German too can come back in Latin
// lookalikes. Latin keeps the Latin languages, Cyrillic the Cyrillic ones and English; when
// Tesseract cannot tell, all of them stay.
func (e *ImageOCRExtractor) photoLanguages(ctx context.Context, data []byte) string {
	langs := ocrLanguages(e.langs)
	var latin, cyr []string
	for _, l := range strings.Split(langs, "+") {
		if cyrillicLangs[l] {
			cyr = append(cyr, l)
		} else {
			latin = append(latin, l)
		}
	}
	if len(latin) == 0 || len(cyr) == 0 {
		return langs
	}
	detect := e.scriptFn
	if detect == nil {
		detect = detectScript
	}
	switch detect(ctx, data) {
	case "Latin":
		return strings.Join(latin, "+")
	case "Cyrillic":
		for _, l := range latin {
			if l == "eng" {
				cyr = append(cyr, l)
			}
		}
		return strings.Join(cyr, "+")
	}
	return langs
}

// detectScript runs Tesseract's orientation and script detection (--psm 0, osd.traineddata) on the
// picture as it is: "Latin", "Cyrillic", ... or "" when it fails or finds too little text.
func detectScript(ctx context.Context, data []byte) string {
	release, err := acquireOCR(ctx)
	if err != nil {
		return ""
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tesseract", "stdin", "stdout", "--psm", "0")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseScript(string(out))
}

// parseScript takes the script out of Tesseract's --psm 0 report ("Script: Latin").
func parseScript(report string) string {
	for _, line := range strings.Split(report, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Script:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// runTesseractPage reads a prepared page with --psm 6 (one block of text: the rows of a table stay
// rows) into its text and its TSV, from which the mean confidence of the words comes.
func runTesseractPage(ctx context.Context, page []byte, langs string) (string, float64, error) {
	ctx, cancel := context.WithTimeout(ctx, ocrTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "ocr-photo-")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(dir)
	base := filepath.Join(dir, "page")
	cmd := exec.CommandContext(ctx, "tesseract", "stdin", base, "-l", langs, "--psm", "6",
		"-c", "preserve_interword_spaces=1", "txt", "tsv")
	cmd.Stdin = bytes.NewReader(page)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("tesseract OCR failed: %w (stderr: %s)", err, errBuf.String())
	}
	text, err := os.ReadFile(base + ".txt")
	if err != nil {
		return "", 0, err
	}
	tsv, err := os.ReadFile(base + ".tsv")
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(string(text)), meanWordConfidence(tsv), nil
}

// meanWordConfidence is the mean "conf" of the words (level 5, with text) of Tesseract's TSV.
func meanWordConfidence(tsv []byte) float64 {
	var sum float64
	var n int
	sc := bufio.NewScanner(bytes.NewReader(tsv))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 12 || f[0] != "5" || strings.TrimSpace(f[11]) == "" {
			continue
		}
		c, err := strconv.ParseFloat(f[10], 64)
		if err != nil || c < 0 {
			continue
		}
		sum += c
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

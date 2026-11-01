package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ocrTimeout bounds a single tesseract run (one image or one rasterised PDF page).
const ocrTimeout = 2 * time.Minute

const (
	// defaultOCRConcurrency is how many pages are OCR'd at once. Measured on the
	// target machine (16 threads, 24 dense pages of Bulgarian text): one page at a
	// time with a multi-threaded tesseract took 138 s; with single-threaded
	// processes 4 at once took 43 s, 8 took 26 s and 12 took 20 s. Six leaves
	// room for the backend, the embedder and the model to work meanwhile.
	defaultOCRConcurrency = 6

	// defaultOCRDPI is the resolution pages are rasterised at. On the same 24
	// pages 300 dpi lost 48 letters against 250 and 200 dpi (which read
	// identically) and 200 dpi was 30 % faster than 250. Those pages are clean
	// renders; 250 keeps a margin for the small print of real scans until they
	// have been compared (task 32). OCR_DPI changes it.
	defaultOCRDPI = 250
)

// envInt reads a positive integer from the environment, falling back to def for
// an unset, malformed or out of range value.
func envInt(name string, def, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		slog.Warn("ignoring invalid "+name, slog.String("value", raw), slog.Int("using", def))
		return def
	}
	return v
}

var (
	ocrSlotsOnce sync.Once
	ocrSlots     chan struct{}
)

// acquireOCR takes one of the process-wide OCR slots (OCR_CONCURRENCY), so that
// an image upload and a PDF that is being OCR'd page by page do not together
// start more tesseract processes than the machine can feed.
func acquireOCR(ctx context.Context) (release func(), err error) {
	ocrSlotsOnce.Do(func() {
		ocrSlots = make(chan struct{}, envInt("OCR_CONCURRENCY", defaultOCRConcurrency, 1, 64))
	})
	select {
	case ocrSlots <- struct{}{}:
		return func() { <-ocrSlots }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

// ImageOCRExtractor is an extractor that uses Tesseract OCR to extract text from images via CLI.
type ImageOCRExtractor struct{}

// NewImageOCRExtractor creates a new ImageOCRExtractor.
func NewImageOCRExtractor() *ImageOCRExtractor {
	return &ImageOCRExtractor{}
}

// Name returns the name of the extractor.
func (e *ImageOCRExtractor) Name() string {
	return "Image OCR (Tesseract CLI)"
}

// Supports checks if the file is an image that can be processed. SVG is markup,
// not pixels: Tesseract cannot read it, and its text is not what OCR would find.
func (e *ImageOCRExtractor) Supports(mimeType, ext string) bool {
	return strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "image/svg")
}

// Extract extracts text from the image reader using the Tesseract CLI.
func (e *ImageOCRExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	release, err := acquireOCR(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	text, err := runTesseract(ctx, r, false)
	if err != nil {
		return "", err
	}
	// A scanned page is full of words broken at the end of a line.
	return JoinHyphenated(text), nil
}

// runTesseract reads an image from r and returns the recognised text. With
// singleThread set, Tesseract's own OpenMP threads are limited to one: when
// several pages are recognised at once, four processes with four threads each
// only fight over the cores.
func runTesseract(ctx context.Context, r io.Reader, singleThread bool) (string, error) {
	// A hung tesseract must not block the ingest job until its own hour-long timeout.
	ctx, cancel := context.WithTimeout(ctx, ocrTimeout)
	defer cancel()

	// Execute tesseract CLI, reading from stdin and writing to stdout, using bul and eng languages.
	cmd := exec.CommandContext(ctx, "tesseract", "stdin", "stdout", "-l", "bul+eng", "--psm", "3", "-c", "preserve_interword_spaces=1")
	if singleThread {
		cmd.Env = append(os.Environ(), "OMP_THREAD_LIMIT=1")
	}

	// Pass the image data via standard input.
	cmd.Stdin = r

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tesseract OCR failed: %w (stderr: %s)", err, errBuf.String())
	}

	return strings.TrimSpace(outBuf.String()), nil
}

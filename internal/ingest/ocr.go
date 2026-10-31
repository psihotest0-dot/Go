package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ocrTimeout bounds a single tesseract run (one image or one rasterised PDF page).
const ocrTimeout = 2 * time.Minute

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

// Supports checks if the file is an image that can be processed.
func (e *ImageOCRExtractor) Supports(mimeType, ext string) bool {
	return strings.HasPrefix(mimeType, "image/")
}

// Extract extracts text from the image reader using the Tesseract CLI.
func (e *ImageOCRExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	// A hung tesseract must not block the ingest job until its own hour-long timeout.
	ctx, cancel := context.WithTimeout(ctx, ocrTimeout)
	defer cancel()

	// Execute tesseract CLI, reading from stdin and writing to stdout, using bul and eng languages.
	cmd := exec.CommandContext(ctx,"tesseract", "stdin", "stdout", "-l", "bul+eng", "--psm", "3", "-c", "preserve_interword_spaces=1")

	// Pass the image data via standard input.
	cmd.Stdin = r

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("tesseract OCR failed: %w (stderr: %s)", err, errBuf.String())
	}

	return strings.TrimSpace(outBuf.String()), nil
}

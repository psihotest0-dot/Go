package ingest

import (
	"context"
	"io"
	"strings"
	// "github.com/otiai10/gosseract/v2" // Uncomment when adding the dependency
)

// ImageOCRExtractor is an extractor that uses Tesseract OCR to extract text from images.
type ImageOCRExtractor struct{}

// NewImageOCRExtractor creates a new ImageOCRExtractor.
func NewImageOCRExtractor() *ImageOCRExtractor {
	return &ImageOCRExtractor{}
}

// Supports checks if the file is an image that can be processed.
func (e *ImageOCRExtractor) Supports(mimeType, ext string) bool {
	return strings.HasPrefix(mimeType, "image/")
}

// Extract extracts text from the image reader.
func (e *ImageOCRExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	// --- Draft Implementation Note ---
	// Real implementation requires github.com/otiai10/gosseract/v2 and libtesseract-dev
	//
	// data, err := io.ReadAll(r)
	// if err != nil {
	// 	return "", fmt.Errorf("failed to read image data: %w", err)
	// }
	//
	// client := gosseract.NewClient()
	// defer client.Close()
	//
	// // To support Bulgarian and English
	// client.SetLanguage("bul", "eng")
	//
	// err = client.SetImageFromBytes(data)
	// if err != nil {
	// 	return "", fmt.Errorf("failed to set image for OCR: %w", err)
	// }
	//
	// text, err := client.Text()
	// if err != nil {
	// 	return "", fmt.Errorf("failed to extract text via OCR: %w", err)
	// }
	//
	// return text, nil

	return "OCR Extraction Draft. To implement, add github.com/otiai10/gosseract/v2 dependency and libtesseract-dev system packages.", nil
}

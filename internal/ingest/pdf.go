package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

type PDFExtractor struct{}

func NewPDFExtractor() *PDFExtractor {
	return &PDFExtractor{}
}

func (e *PDFExtractor) Name() string {
	return "PDFExtractor"
}

func (e *PDFExtractor) Supports(mimeType, ext string) bool {
	if strings.HasPrefix(mimeType, "application/pdf") {
		return true
	}
	return strings.ToLower(ext) == ".pdf"
}

func (e *PDFExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	// Create a temporary file because pdftoppm/pdftotext work best with physical files.
	tmpFile, err := os.CreateTemp("", "upload-*.pdf")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for PDF: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(b); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write to temp file: %w", err)
	}
	tmpFile.Close()

	// Determine the number of pages using pdfcpu.
	rs := bytes.NewReader(b)
	conf := model.NewDefaultConfiguration()
	pageCount, err := api.PageCount(ctx, rs, conf)
	if err != nil {
		return "", fmt.Errorf("failed to get PDF page count: %w", err)
	}

	var finalBuf strings.Builder
	imgExtractor := NewImageOCRExtractor()

	// Process page by page
	for i := 1; i <= pageCount; i++ {
		pageStr := fmt.Sprintf("%d", i)

		// 1. Try to extract text using pdftotext for the specific page
		txtCmd := exec.CommandContext(ctx, "pdftotext", "-f", pageStr, "-l", pageStr, tmpFile.Name(), "-")
		txtOut, err := txtCmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// fallback or skip
			continue
		}

		pageText := strings.TrimSpace(string(txtOut))

		// 2. If text is less than 20 characters, assume it's a scanned page and fallback to OCR
		if len(pageText) < 20 {
			// Rasterize the page to PNG
			ppmCmd := exec.CommandContext(ctx, "pdftoppm", "-r", "300", "-f", pageStr, "-l", pageStr, "-png", tmpFile.Name())
			pngOut, err := ppmCmd.Output()
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				// Failed to rasterize, continue to next page
				continue
			}

			// Run OCR on the rasterized PNG
			ocrText, err := imgExtractor.Extract(ctx, bytes.NewReader(pngOut))
			if err == nil && len(strings.TrimSpace(ocrText)) > 0 {
				finalBuf.WriteString(strings.TrimSpace(ocrText))
				finalBuf.WriteString("\n[source: \"ocr\"]\n")
			}
		} else {
			finalBuf.WriteString(pageText)
			finalBuf.WriteString("\n")
		}
	}

	return strings.TrimSpace(finalBuf.String()), nil
}

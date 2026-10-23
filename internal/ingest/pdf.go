package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
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
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()

	if _, wErr := tmpFile.Write(b); wErr != nil {
		return "", fmt.Errorf("failed to write to temp file: %w", wErr)
	}

	// 1. Try to extract text using pdftotext for the entire file
	txtCmd := exec.CommandContext(ctx, "pdftotext", "-layout", tmpFile.Name(), "-")
	txtOut, err := txtCmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		slog.Warn("pdftotext failed for entire file, proceeding to fallback/OCR", slog.Any("error", err))
	}

	// Split extracted text by page using form feed \f
	pages := strings.Split(string(txtOut), "\f")

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
		var pageText string
		if i-1 < len(pages) {
			pageText = strings.TrimSpace(pages[i-1])
		}

		// 2. If text is less than 50 characters, assume it's a scanned page and fallback to OCR
		if len(pageText) < 50 {
			// Rasterize the page to PNG
			ppmCmd := exec.CommandContext(ctx, "pdftoppm", "-r", "300", "-f", pageStr, "-l", pageStr, "-png", tmpFile.Name())
			pngOut, err := ppmCmd.Output()
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				slog.Warn("pdftoppm failed for page", slog.String("page", pageStr), slog.Any("error", err))
				continue
			}

			// Run OCR on the rasterized PNG
			ocrText, err := imgExtractor.Extract(ctx, bytes.NewReader(pngOut))
			if err != nil {
				slog.Warn("OCR failed for page", slog.String("page", pageStr), slog.Any("error", err))
				continue
			}
			if len(strings.TrimSpace(ocrText)) > 0 {
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

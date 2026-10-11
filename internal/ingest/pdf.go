package ingest

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
)

type PDFExtractor struct{}

func NewPDFExtractor() *PDFExtractor {
	return &PDFExtractor{}
}

func (e *PDFExtractor) Name() string {
	return "PDFExtractor"
}

func (e *PDFExtractor) Supports(mimeType, ext string) bool {
	return mimeType == "application/pdf" || ext == ".pdf"
}

func (e *PDFExtractor) Extract(ctx context.Context, reader io.Reader) (string, error) {
	// Fallback to pdftotext

	// Create temporary file
	tmpFile, err := os.CreateTemp("", "upload-*.pdf")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, reader); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-enc", "UTF-8", tmpFile.Name(), "-")

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", err
	}

	return strings.TrimSpace(out.String()), nil
}

package ingest

import (
	"bytes"
	"context"
	"io"
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
	rs := bytes.NewReader(b)
	conf := model.NewDefaultConfiguration()

	var buf strings.Builder
	digestContent := func(r io.Reader, pageNum int) error {
		textBytes, err := io.ReadAll(r)
		if err == nil {
			buf.Write(textBytes)
			buf.WriteString("\n")
		}
		return nil
	}

	err = api.ExtractContent(ctx, rs, nil, digestContent, conf)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(buf.String()), nil
}

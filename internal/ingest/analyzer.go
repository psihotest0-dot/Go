package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/gabriel-vasile/mimetype"
)

type Analyzer struct {
	extractors []Extractor
}

func NewAnalyzer() *Analyzer {
	return &Analyzer{
		extractors: []Extractor{
			NewTextExtractor(),
			NewPDFExtractor(),
			NewOfficeExtractor(),
		},
	}
}

// Analyze extracts text from a given reader depending on magic bytes and filename.
func (a *Analyzer) Analyze(ctx context.Context, reader io.Reader, filename string) (string, error) {
	// Read everything into memory so we can detect and extract
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	mtype := mimetype.Detect(data)
	mimeStr := mtype.String()
	ext := filepath.Ext(filename)

	var selectedExtractor Extractor
	for _, e := range a.extractors {
		if e.Supports(mimeStr, ext) {
			selectedExtractor = e
			break
		}
	}

	if selectedExtractor == nil {
		return "", fmt.Errorf("unsupported file type: mime=%s, ext=%s", mimeStr, ext)
	}

	return selectedExtractor.Extract(ctx, bytes.NewReader(data))
}

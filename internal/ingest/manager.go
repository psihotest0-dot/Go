package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/gabriel-vasile/mimetype"
)

type Manager struct {
	extractors []Extractor
}

func NewManager() *Manager {
	return &Manager{
		extractors: []Extractor{
			NewTextExtractor(),
			NewHTMLExtractor(),
			NewPDFExtractor(),
			NewDOCXExtractor(),
			NewXLSXExtractor(),
		},
	}
}

func (m *Manager) Extract(ctx context.Context, filename string, r io.Reader) (string, error) {
	ext := filepath.Ext(filename)

	var buf bytes.Buffer
	tee := io.TeeReader(r, &buf)

	header := make([]byte, 512)
	n, err := io.ReadFull(tee, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", fmt.Errorf("failed to read file header: %w", err)
	}

	mtype := mimetype.Detect(header[:n])
	mimeStr := mtype.String()

	multiReader := io.MultiReader(&buf, r)

	for _, ex := range m.extractors {
		if ex.Supports(mimeStr, ext) {
			return ex.Extract(ctx, multiReader)
		}
	}

	return "", fmt.Errorf("no suitable extractor found for mime type: %s, extension: %s", mimeStr, ext)
}

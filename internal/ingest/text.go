package ingest

import (
	"context"
	"io"
	"strings"
)

type TextExtractor struct{}

func NewTextExtractor() *TextExtractor {
	return &TextExtractor{}
}

func (e *TextExtractor) Name() string {
	return "TextExtractor"
}

func (e *TextExtractor) Supports(mimeType, ext string) bool {
	if strings.HasPrefix(mimeType, "text/plain") ||
		strings.HasPrefix(mimeType, "text/markdown") ||
		strings.HasPrefix(mimeType, "text/csv") ||
		strings.HasPrefix(mimeType, "application/json") ||
		strings.HasPrefix(mimeType, "application/yaml") ||
		strings.HasPrefix(mimeType, "application/x-yaml") ||
		strings.HasPrefix(mimeType, "text/yaml") ||
		strings.HasPrefix(mimeType, "text/x-yaml") {
		return true
	}

	exts := []string{".txt", ".md", ".csv", ".json", ".yaml", ".yml", ".log"}
	ext = strings.ToLower(ext)
	for _, x := range exts {
		if x == ext {
			return true
		}
	}

	return false
}

func (e *TextExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	bytes, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

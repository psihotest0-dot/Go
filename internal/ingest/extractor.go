package ingest

import (
	"context"
	"io"
)

// Extractor is the interface for different document parsers.
type Extractor interface {
	// Name returns the name of the extractor.
	Name() string

	// Supports checks if this extractor supports parsing the given mimeType and file extension.
	Supports(mimeType, ext string) bool

	// Extract parses the content from the reader and returns the extracted text.
	Extract(ctx context.Context, reader io.Reader) (string, error)
}

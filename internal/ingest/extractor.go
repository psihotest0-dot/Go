package ingest

import (
	"context"
	"io"
)

type Extractor interface {
	Name() string
	Supports(mimeType, ext string) bool
	Extract(ctx context.Context, reader io.Reader) (string, error)
}

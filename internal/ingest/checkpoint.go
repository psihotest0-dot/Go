package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type checkpointKey struct{}

// WithCheckpointDir lets a long extraction keep its progress in dir. A scanned
// book takes minutes of OCR; without saved pages a timeout, a restart or a
// failure on page 290 would start again from page 1. The caller removes the
// directory with ClearCheckpoint once the file no longer needs it.
func WithCheckpointDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, checkpointKey{}, dir)
}

func checkpointDirFrom(ctx context.Context) string {
	dir, _ := ctx.Value(checkpointKey{}).(string)
	return dir
}

// ClearCheckpoint deletes saved progress. An empty dir is a no-op.
func ClearCheckpoint(dir string) error {
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// pageStore keeps the OCR text of finished pages, one small file per page. The
// resolution is part of the name, so changing OCR_DPI does not reuse pages that
// were recognised at another one.
type pageStore struct {
	dir string
	dpi int
}

func newPageStore(dir string, dpi int) *pageStore {
	if dir == "" {
		return nil
	}
	return &pageStore{dir: dir, dpi: dpi}
}

func (s *pageStore) path(page int) string {
	return filepath.Join(s.dir, fmt.Sprintf("p%05d.d%d.txt", page, s.dpi))
}

// Get returns the saved text of a page. A page that was recognised as blank is
// saved as an empty file and reported as found, so it is not recognised again.
func (s *pageStore) Get(page int) (string, bool) {
	if s == nil {
		return "", false
	}
	b, err := os.ReadFile(s.path(page))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Put saves the text of a page. The file appears whole or not at all, so a
// crash in the middle of a write cannot leave half a page behind.
func (s *pageStore) Put(page int, text string) error {
	if s == nil {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "page-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path(page)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

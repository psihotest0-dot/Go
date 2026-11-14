package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gabriel-vasile/mimetype"
)

type Manager struct {
	extractors []Extractor
	limits     Limits
}

// Option customises a Manager.
type Option func(*Manager)

// WithLimits replaces the default input and output limits. Zero fields keep
// their defaults.
func WithLimits(l Limits) Option {
	return func(m *Manager) {
		if l.MaxInputBytes > 0 {
			m.limits.MaxInputBytes = l.MaxInputBytes
		}
		if l.MaxTextRunes > 0 {
			m.limits.MaxTextRunes = l.MaxTextRunes
		}
	}
}

func NewManager(opts ...Option) *Manager {
	m := &Manager{
		extractors: []Extractor{
			NewTextExtractor(),
			NewHTMLExtractor(),
			NewPDFExtractor(),
			NewDOCXExtractor(),
			NewXLSXExtractor(),
			NewImageOCRExtractor(),
			NewEPUBExtractor(),
		},
		limits: DefaultLimits(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Extract returns the text of a file. It reads at most Limits.MaxInputBytes,
// returns text that PostgreSQL can store (valid UTF-8, no NUL), and fails rather
// than return an empty result, so that a file with nothing readable in it is
// reported instead of becoming a "completed" file with zero chunks.
func (m *Manager) Extract(ctx context.Context, filename string, r io.Reader) (string, error) {
	ext := filepath.Ext(filename)
	r = newCappedReader(r, &byteBudget{left: m.limits.MaxInputBytes}, ErrInputTooLarge)

	header := make([]byte, detectBytes)
	n, err := io.ReadFull(r, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("failed to read file header: %w", err)
	}
	if n == 0 {
		return "", ErrEmptyFile
	}
	header = header[:n]

	mimeStr := mimetype.Detect(header).String()
	body := io.MultiReader(bytes.NewReader(header), r)

	for _, ex := range m.extractors {
		if !ex.Supports(mimeStr, ext) {
			continue
		}
		text, err := ex.Extract(ctx, body)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ex.Name(), err)
		}
		return m.finish(stripGutenberg(sanitizeText(text)))
	}

	return "", fmt.Errorf("%w: mime type %s, extension %q", ErrUnsupported, mimeStr, ext)
}

// finish applies the checks that hold for every format.
func (m *Manager) finish(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", ErrNoText
	}
	if runes := utf8.RuneCountInString(text); runes > m.limits.MaxTextRunes {
		return "", fmt.Errorf("%w: %d characters, the limit is %d", ErrTextTooLarge, runes, m.limits.MaxTextRunes)
	}
	return text, nil
}

package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

// stubExtractor lets a test control exactly what an extractor returns.
type stubExtractor struct {
	text string
	err  error
}

func (s stubExtractor) Name() string                                       { return "stub" }
func (s stubExtractor) Supports(mimeType, ext string) bool                 { return true }
func (s stubExtractor) Extract(context.Context, io.Reader) (string, error) { return s.text, s.err }

func stubManager(text string, opts ...Option) *Manager {
	m := &Manager{extractors: []Extractor{stubExtractor{text: text}}, limits: DefaultLimits()}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestManagerStripsNULAndInvalidUTF8(t *testing.T) {
	// A NUL byte or a broken sequence makes the INSERT into PostgreSQL fail, and
	// with it the whole file.
	m := stubManager("Здра\x00вей\xff\xfe свят\x00")
	got, err := m.Extract(context.Background(), "a.txt", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "Здравей свят" {
		t.Fatalf("got %q", got)
	}
	if !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
		t.Fatalf("result is not storable: %q", got)
	}
}

func TestManagerRealTextFileWithNUL(t *testing.T) {
	m := NewManager()
	got, err := m.Extract(context.Background(), "notes.txt", strings.NewReader("едно\x00две\nтри"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "единдве\nтри" && got != "едно две\nтри" && !strings.Contains(got, "едно") {
		t.Fatalf("unexpected text %q", got)
	}
	if strings.ContainsRune(got, 0) {
		t.Fatalf("NUL survived: %q", got)
	}
}

func TestManagerEmptyResultIsAnError(t *testing.T) {
	m := NewManager()

	t.Run("zero bytes", func(t *testing.T) {
		_, err := m.Extract(context.Background(), "empty.txt", bytes.NewReader(nil))
		if !errors.Is(err, ErrEmptyFile) {
			t.Fatalf("err = %v, want ErrEmptyFile", err)
		}
	})
	t.Run("only whitespace", func(t *testing.T) {
		_, err := m.Extract(context.Background(), "blank.txt", strings.NewReader(" \n\t \r\n"))
		if !errors.Is(err, ErrNoText) {
			t.Fatalf("err = %v, want ErrNoText", err)
		}
	})
	t.Run("html without text", func(t *testing.T) {
		_, err := m.Extract(context.Background(), "page.html", strings.NewReader("<html><body><script>var a=1;</script></body></html>"))
		if !errors.Is(err, ErrNoText) {
			t.Fatalf("err = %v, want ErrNoText", err)
		}
	})
	t.Run("text that is only NUL", func(t *testing.T) {
		_, err := m.Extract(context.Background(), "nul.txt", strings.NewReader("\x00\x00\x00"))
		if !errors.Is(err, ErrNoText) {
			t.Fatalf("err = %v, want ErrNoText", err)
		}
	})
}

func TestManagerSVGIsNotOCRed(t *testing.T) {
	svg := `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><text>Hi</text></svg>`
	if NewImageOCRExtractor().Supports("image/svg+xml", ".svg") {
		t.Fatal("the OCR extractor must not take SVG")
	}
	_, err := NewManager().Extract(context.Background(), "logo.svg", strings.NewReader(svg))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	// Real raster images are still OCR candidates.
	if !NewImageOCRExtractor().Supports("image/png", ".png") {
		t.Fatal("PNG must still go to OCR")
	}
}

func TestManagerInputLimit(t *testing.T) {
	m := NewManager(WithLimits(Limits{MaxInputBytes: 10_000}))

	_, err := m.Extract(context.Background(), "big.txt", strings.NewReader(strings.Repeat("a", 10_001)))
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("over the limit: err = %v, want ErrInputTooLarge", err)
	}

	got, err := m.Extract(context.Background(), "ok.txt", strings.NewReader(strings.Repeat("a", 10_000)))
	if err != nil || len(got) != 10_000 {
		t.Fatalf("exactly at the limit: len=%d err=%v", len(got), err)
	}
}

func TestManagerInputLimitDoesNotReadTheWholeSource(t *testing.T) {
	// An endless source must stop at the limit, not be read to exhaustion.
	var read int64
	endless := readerFunc(func(p []byte) (int, error) {
		for i := range p {
			p[i] = 'a'
		}
		read += int64(len(p))
		return len(p), nil
	})
	m := NewManager(WithLimits(Limits{MaxInputBytes: 1 << 20}))
	_, err := m.Extract(context.Background(), "endless.txt", endless)
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if read > 2<<20 {
		t.Fatalf("read %d bytes from an endless source with a 1 MiB limit", read)
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestManagerTextLimit(t *testing.T) {
	m := stubManager(strings.Repeat("я", 1001), WithLimits(Limits{MaxTextRunes: 1000}))
	_, err := m.Extract(context.Background(), "a.txt", strings.NewReader("x"))
	if !errors.Is(err, ErrTextTooLarge) {
		t.Fatalf("err = %v, want ErrTextTooLarge", err)
	}

	// The limit counts characters, not bytes: 1000 Cyrillic letters are 2000 bytes.
	m = stubManager(strings.Repeat("я", 1000), WithLimits(Limits{MaxTextRunes: 1000}))
	if _, err := m.Extract(context.Background(), "a.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("1000 characters should fit: %v", err)
	}
}

func TestManagerKeepsExtractorErrorsAndReadErrors(t *testing.T) {
	boom := errors.New("boom")
	m := &Manager{extractors: []Extractor{stubExtractor{err: boom}}, limits: DefaultLimits()}
	_, err := m.Extract(context.Background(), "a.txt", strings.NewReader("x"))
	if !errors.Is(err, boom) {
		t.Fatalf("extractor error lost: %v", err)
	}
	if IsPermanent(err) {
		t.Fatal("an unknown extractor error must stay retryable")
	}

	_, err = NewManager().Extract(context.Background(), "a.txt", iotest.ErrReader(boom))
	if !errors.Is(err, boom) {
		t.Fatalf("read error lost: %v", err)
	}
}

func TestIsPermanent(t *testing.T) {
	for _, err := range []error{ErrEmptyFile, ErrNoText, ErrInputTooLarge, ErrTextTooLarge, ErrUnsupported, ErrArchiveTooLarge, ErrPasswordProtected} {
		if !IsPermanent(err) {
			t.Errorf("%v should be permanent", err)
		}
	}
	if IsPermanent(errors.New("timeout")) || IsPermanent(nil) {
		t.Error("unrelated errors are not permanent")
	}
}

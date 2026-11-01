package ingest

import (
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultMaxInputBytes is the largest file the manager reads. It matches the
	// default upload limit (config.DefaultMaxUploadMB); the worker passes the
	// configured MAX_UPLOAD_MB instead.
	DefaultMaxInputBytes int64 = 100 << 20

	// DefaultMaxTextRunes is the ceiling on the text extracted from one file. A
	// thousand page book is ~3 million characters, so this only trips on runaway
	// output (a decompression bomb, an OCR loop), and a clear error beats
	// chunking, embedding and storing a hundred million characters.
	DefaultMaxTextRunes = 50_000_000

	// detectBytes is how much of the file is sniffed for its MIME type.
	// mimetype.Detect reads at most 3072 bytes, and several container formats
	// (docx, epub, xlsx) are only told apart from a plain zip further than 512.
	detectBytes = 3072

	// maxExtractedBytes bounds the memory one extractor may use for its output.
	// It is a guard for the archive formats, where the output size is not known
	// until the XML has been read; the exact rune ceiling is applied afterwards.
	maxExtractedBytes = 200 << 20
)

// Limits bounds what the manager reads and what an extraction may produce.
type Limits struct {
	MaxInputBytes int64
	MaxTextRunes  int
}

// DefaultLimits returns the limits NewManager uses without options.
func DefaultLimits() Limits {
	return Limits{MaxInputBytes: DefaultMaxInputBytes, MaxTextRunes: DefaultMaxTextRunes}
}

// Errors for input that will fail the same way every time. A worker should not
// retry them: see IsPermanent.
var (
	ErrEmptyFile         = errors.New("file is empty")
	ErrNoText            = errors.New("no text could be extracted from the file")
	ErrInputTooLarge     = errors.New("file is too large")
	ErrTextTooLarge      = errors.New("extracted text is too large")
	ErrUnsupported       = errors.New("unsupported file format")
	ErrArchiveTooLarge   = errors.New("archive expands to more data than the allowed limit")
	ErrPasswordProtected = errors.New("document is password protected")
)

// IsPermanent reports whether retrying the same file cannot succeed.
func IsPermanent(err error) bool {
	for _, target := range []error{
		ErrEmptyFile, ErrNoText, ErrInputTooLarge, ErrTextTooLarge,
		ErrUnsupported, ErrArchiveTooLarge, ErrPasswordProtected,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// byteBudget is a number of bytes that may still be read. Several readers can
// share one budget, which is how an archive's entries are limited together.
type byteBudget struct{ left int64 }

// cappedReader reads from r while the budget lasts and fails with tooBig,
// instead of silently truncating, when the source has more. A truncated file
// would ingest as a plausible but incomplete document.
type cappedReader struct {
	r      io.Reader
	budget *byteBudget
	tooBig error
}

func newCappedReader(r io.Reader, budget *byteBudget, tooBig error) *cappedReader {
	return &cappedReader{r: r, budget: budget, tooBig: tooBig}
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.budget.left <= 0 {
		// The allowance is used up: more data is an error, EOF is fine.
		var probe [1]byte
		n, err := c.r.Read(probe[:])
		if n > 0 {
			return 0, c.tooBig
		}
		return 0, err
	}
	if int64(len(p)) > c.budget.left {
		p = p[:c.budget.left]
	}
	n, err := c.r.Read(p)
	c.budget.left -= int64(n)
	return n, err
}

// sanitizeText makes extracted text storable in PostgreSQL: it rejects both
// invalid UTF-8 and NUL bytes, and a single one fails the whole INSERT.
func sanitizeText(s string) string {
	if utf8.ValidString(s) && !strings.Contains(s, "\x00") {
		return s
	}
	s = strings.ToValidUTF8(s, "")
	return strings.ReplaceAll(s, "\x00", "")
}

// textBuilder collects extracted text up to a byte ceiling, so that an archive
// that expands without bound fails with ErrTextTooLarge instead of exhausting
// memory. It also keeps track of trailing newlines, so that empty paragraphs
// and runs of <br> do not pile up blank lines.
type textBuilder struct {
	sb       strings.Builder
	max      int
	trailing int // newlines at the end of the text so far
}

func newTextBuilder() *textBuilder { return &textBuilder{max: maxExtractedBytes} }

func (b *textBuilder) WriteString(s string) error {
	if s == "" {
		return nil
	}
	if b.sb.Len()+len(s) > b.max {
		return ErrTextTooLarge
	}
	b.sb.WriteString(s)
	if kept := strings.TrimRight(s, "\n"); kept == "" {
		b.trailing += len(s)
	} else {
		b.trailing = len(s) - len(kept)
	}
	return nil
}

// Newline ends the current line, unless there is nothing yet or it is already ended.
func (b *textBuilder) Newline() error {
	if b.sb.Len() == 0 || b.trailing >= 1 {
		return nil
	}
	return b.WriteString("\n")
}

// Paragraph leaves one blank line, unless there is nothing yet or it is already there.
func (b *textBuilder) Paragraph() error {
	if b.sb.Len() == 0 || b.trailing >= 2 {
		return nil
	}
	return b.WriteString(strings.Repeat("\n", 2-b.trailing))
}

func (b *textBuilder) Len() int { return b.sb.Len() }

func (b *textBuilder) String() string { return b.sb.String() }

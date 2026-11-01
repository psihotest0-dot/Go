package ingest

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

type DOCXExtractor struct {
	limits zipLimits
}

func NewDOCXExtractor() *DOCXExtractor {
	return &DOCXExtractor{limits: defaultZipLimits}
}

func (e *DOCXExtractor) Name() string {
	return "DOCXExtractor"
}

func (e *DOCXExtractor) Supports(mimeType, ext string) bool {
	if strings.HasPrefix(mimeType, "application/vnd.openxmlformats-officedocument.wordprocessingml.document") {
		return true
	}
	return strings.ToLower(ext) == ".docx"
}

// Extract reads word/document.xml as a stream of tokens. Building a tree of the
// document (the previous approach) cost ~40 times the size of the XML in
// allocations; the stream needs memory only for the text itself.
//
// Text comes from <w:t>. A paragraph ends with a newline, a tab becomes a space,
// <w:br> starts a new line, and a table row is one line with its cells separated
// by spaces. The text of a run is kept as it is, not trimmed: Word splits a word
// into several runs
// ("Hel" + "lo") wherever the formatting changes, and the spaces are in the runs.
// <mc:Fallback> is skipped; it repeats the text of the <mc:Choice> before it
// (text boxes, shapes).
func (e *DOCXExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	a, err := openZipArchive(r, e.limits)
	if err != nil {
		return "", err
	}
	defer a.Close()

	doc := a.Find("word/document.xml")
	if doc == nil {
		return "", fmt.Errorf("%w: word/document.xml is missing, this is not a docx", ErrUnsupported)
	}
	rc, err := a.Open(doc)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	tb := newTextBuilder()
	if err := writeDOCXText(ctx, rc, tb); err != nil {
		return "", err
	}
	return strings.TrimSpace(tb.String()), nil
}

func writeDOCXText(ctx context.Context, r io.Reader, tb *textBuilder) error {
	dec := xml.NewDecoder(r)
	var (
		inText       bool
		fallbackDeep int
		cellDeep     int // table cells open around the current paragraph
	)
	for tokens := 0; ; tokens++ {
		if tokens%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if fallbackDeep > 0 {
				if t.Name.Local == "Fallback" {
					fallbackDeep++
				}
				continue
			}
			var werr error
			switch t.Name.Local {
			case "Fallback":
				fallbackDeep = 1
			case "t":
				inText = true
			case "tc":
				cellDeep++
			case "tab":
				werr = tb.WriteString(" ")
			case "br", "cr":
				werr = tb.Newline()
			}
			if werr != nil {
				return werr
			}
		case xml.EndElement:
			if fallbackDeep > 0 {
				if t.Name.Local == "Fallback" {
					fallbackDeep--
				}
				continue
			}
			var werr error
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if cellDeep > 0 {
					werr = tb.WriteString(" ") // a cell's paragraphs stay on the row's line
				} else {
					werr = tb.Newline()
				}
			case "tc":
				cellDeep = max(0, cellDeep-1)
			case "tr":
				werr = tb.Newline()
			}
			if werr != nil {
				return werr
			}
		case xml.CharData:
			if inText && fallbackDeep == 0 {
				if err := tb.WriteString(string(t)); err != nil {
					return err
				}
			}
		}
	}
}

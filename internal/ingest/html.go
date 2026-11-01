package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// htmlMaxToken bounds the memory the tokenizer may hold for one token. A tag
// carries its attributes, so a page with a base64 image inlined in an <img>
// tag is one large token; 32 MiB is far above any real page.
const htmlMaxToken = 32 << 20

type HTMLExtractor struct{}

func NewHTMLExtractor() *HTMLExtractor {
	return &HTMLExtractor{}
}

func (e *HTMLExtractor) Name() string {
	return "HTMLExtractor"
}

func (e *HTMLExtractor) Supports(mimeType, ext string) bool {
	if strings.HasPrefix(mimeType, "text/html") {
		return true
	}
	return strings.ToLower(ext) == ".html" || strings.ToLower(ext) == ".htm"
}

func (e *HTMLExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	tb := newTextBuilder()
	if err := writeHTMLText(ctx, r, tb); err != nil {
		return "", err
	}
	return strings.TrimSpace(tb.String()), nil
}

func isBlockElement(tag string) bool {
	switch tag {
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr", "article", "section", "blockquote":
		return true
	}
	return false
}

func isSkippedElement(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "template":
		return true
	}
	return false
}

// writeHTMLText decodes an HTML or XHTML document (any encoding newHTMLReader
// understands) and appends its visible text to tb, with a blank line between
// block elements.
//
// It reads tokens instead of building a tree: the tree of a 100 MB page of
// small tags needs gigabytes, the token stream needs a few kilobytes. A block
// element breaks the text where it starts and where it ends, so a document
// that leaves out the closing </p> still gets its paragraphs apart.
func writeHTMLText(ctx context.Context, r io.Reader, tb *textBuilder) error {
	decoded, err := newHTMLReader(r)
	if err != nil {
		return err
	}
	z := html.NewTokenizer(decoded)
	z.SetMaxBuf(htmlMaxToken)

	var skipTag string // the script/style/... element whose content is being skipped
	skipDepth := 0

	for tokens := 0; ; tokens++ {
		if tokens%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			switch {
			case errors.Is(err, io.EOF):
				return nil
			case errors.Is(err, html.ErrBufferExceeded):
				return fmt.Errorf("%w: a single HTML token is larger than %d MiB", ErrInputTooLarge, htmlMaxToken>>20)
			}
			return err

		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			if text := strings.TrimSpace(string(z.Text())); text != "" {
				if err := tb.WriteString(text + " "); err != nil {
					return err
				}
			}

		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if skipDepth > 0 {
				if tag == skipTag && tt == html.StartTagToken {
					skipDepth++
				}
				continue
			}
			switch {
			case tt == html.SelfClosingTagToken && isSkippedElement(tag):
				// Nothing inside, nothing to skip.
			case isSkippedElement(tag):
				skipTag, skipDepth = tag, 1
			case tag == "br":
				if err := tb.Newline(); err != nil {
					return err
				}
			case isBlockElement(tag):
				if err := tb.Paragraph(); err != nil {
					return err
				}
			}

		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if skipDepth > 0 {
				if tag == skipTag {
					skipDepth--
				}
				continue
			}
			if isBlockElement(tag) {
				if err := tb.Paragraph(); err != nil {
					return err
				}
			}
		}
	}
}

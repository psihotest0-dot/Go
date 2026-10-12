package ingest

import (
	"context"
	"io"
	"strings"

	"golang.org/x/net/html"
)

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
	doc, err := html.Parse(r)
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				buf.WriteString(text + " ")
			}
		} else if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)

	return strings.TrimSpace(buf.String()), nil
}

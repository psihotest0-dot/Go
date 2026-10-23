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

	isBlockElement := func(tag string) bool {
		switch tag {
		case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr", "article", "section", "blockquote":
			return true
		}
		return false
	}

	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				buf.WriteString(text + " ")
			}
		} else if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style" || n.Data == "noscript" || n.Data == "template") {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}

		if n.Type == html.ElementNode && isBlockElement(n.Data) {
			buf.WriteString("\n\n")
		}
	}
	f(doc)

	return strings.TrimSpace(buf.String()), nil
}

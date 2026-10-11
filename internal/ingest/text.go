package ingest

import (
	"context"
	"io"
	"strings"

	"golang.org/x/net/html"
)

type TextExtractor struct{}

func NewTextExtractor() *TextExtractor {
	return &TextExtractor{}
}

func (e *TextExtractor) Name() string {
	return "TextExtractor"
}

func (e *TextExtractor) Supports(mimeType, ext string) bool {
	return strings.HasPrefix(mimeType, "text/") ||
		mimeType == "application/json" ||
		mimeType == "text/csv" ||
		mimeType == "text/markdown" ||
		mimeType == "application/x-yaml" ||
		ext == ".txt" || ext == ".md" || ext == ".csv" || ext == ".json" || ext == ".yaml" || ext == ".log" || ext == ".html"
}

func (e *TextExtractor) Extract(ctx context.Context, reader io.Reader) (string, error) {
	// First read all bytes
	bytes, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}

	content := string(bytes)

	// Fast path check if HTML based on tags
	if strings.Contains(strings.ToLower(content), "<html") || strings.Contains(strings.ToLower(content), "<body") {
		return e.extractHTML(strings.NewReader(content))
	}

	return content, nil
}

func (e *TextExtractor) extractHTML(reader io.Reader) (string, error) {
	doc, err := html.Parse(reader)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				sb.WriteString(text + " ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			// Skip scripts and styles
			if c.Type == html.ElementNode && (c.Data == "script" || c.Data == "style") {
				continue
			}
			f(c)
		}
	}
	f(doc)

	return strings.TrimSpace(sb.String()), nil
}

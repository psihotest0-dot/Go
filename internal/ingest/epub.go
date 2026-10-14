package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"

	"golang.org/x/net/html"
)

type EPUBExtractor struct{}

func NewEPUBExtractor() *EPUBExtractor {
	return &EPUBExtractor{}
}

func (e *EPUBExtractor) Name() string {
	return "EPUBExtractor"
}

func (e *EPUBExtractor) Supports(mimeType, ext string) bool {
	if strings.HasPrefix(mimeType, "application/epub+zip") {
		return true
	}
	return strings.ToLower(ext) == ".epub"
}

func (e *EPUBExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		if strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".htm") || strings.HasSuffix(name, ".xhtml") {
			rc, err := f.Open()
			if err != nil {
				continue
			}

			doc, err := html.Parse(rc)
			rc.Close()
			if err != nil {
				continue
			}

			var extract func(*html.Node)
			extract = func(n *html.Node) {
				if n.Type == html.TextNode {
					text := strings.TrimSpace(n.Data)
					if text != "" {
						buf.WriteString(text + " ")
					}
				} else if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
					return
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					extract(c)
				}
			}
			extract(doc)
		}
	}

	return strings.TrimSpace(buf.String()), nil
}

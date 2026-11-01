package ingest

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"strings"

	"golang.org/x/net/html/charset"
)

type EPUBExtractor struct {
	limits zipLimits
}

func NewEPUBExtractor() *EPUBExtractor {
	return &EPUBExtractor{limits: defaultZipLimits}
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

// Extract returns the chapters in reading order. The order comes from the spine
// of the OPF package file; the order of entries in the archive is whatever the
// packer happened to write, and puts chapter 10 before chapter 2 in many books.
// A book whose package file cannot be read falls back to archive order.
func (e *EPUBExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	a, err := openZipArchive(r, e.limits)
	if err != nil {
		return "", err
	}
	defer a.Close()

	chapters := epubSpine(a)
	if len(chapters) == 0 {
		chapters = archiveOrderChapters(a)
	}

	tb := newTextBuilder()
	for _, ch := range chapters {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		rc, err := a.Open(ch)
		if err != nil {
			if IsPermanent(err) {
				return "", err
			}
			slog.Warn("skipping unreadable epub chapter", slog.String("name", ch.Name), slog.Any("error", err))
			continue
		}
		err = writeHTMLText(ctx, rc, tb)
		rc.Close()
		if err != nil {
			if IsPermanent(err) || ctx.Err() != nil {
				return "", err
			}
			slog.Warn("skipping unreadable epub chapter", slog.String("name", ch.Name), slog.Any("error", err))
			continue
		}
		if err := tb.Paragraph(); err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(tb.String()), nil
}

func isHTMLName(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".htm") || strings.HasSuffix(name, ".xhtml")
}

func archiveOrderChapters(a *zipArchive) []*zip.File {
	var out []*zip.File
	for _, f := range a.reader.File {
		if isHTMLName(f.Name) {
			out = append(out, f)
		}
	}
	return out
}

type epubContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type epubPackage struct {
	Manifest []struct {
		ID        string `xml:"id,attr"`
		Href      string `xml:"href,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"manifest>item"`
	Spine []struct {
		IDRef string `xml:"idref,attr"`
	} `xml:"spine>itemref"`
}

func decodeXMLEntry(a *zipArchive, f *zip.File, v any) error {
	rc, err := a.Open(f)
	if err != nil {
		return err
	}
	defer rc.Close()
	dec := xml.NewDecoder(rc)
	dec.CharsetReader = charset.NewReaderLabel
	return dec.Decode(v)
}

// epubSpine returns the readable chapters of the book in spine order, or nil
// when the container, the package file or the spine cannot be used.
func epubSpine(a *zipArchive) []*zip.File {
	container := a.Find("META-INF/container.xml")
	if container == nil {
		return nil
	}
	var c epubContainer
	if err := decodeXMLEntry(a, container, &c); err != nil || len(c.Rootfiles) == 0 {
		return nil
	}
	opfPath := resolveZipPath("", c.Rootfiles[0].FullPath)
	opfFile := a.Find(opfPath)
	if opfFile == nil {
		return nil
	}
	var pkg epubPackage
	if err := decodeXMLEntry(a, opfFile, &pkg); err != nil {
		return nil
	}

	type item struct{ href, mediaType string }
	manifest := make(map[string]item, len(pkg.Manifest))
	for _, m := range pkg.Manifest {
		manifest[m.ID] = item{href: m.Href, mediaType: strings.ToLower(m.MediaType)}
	}

	var out []*zip.File
	seen := make(map[string]bool)
	for _, ref := range pkg.Spine {
		it, ok := manifest[ref.IDRef]
		if !ok {
			continue
		}
		name := resolveZipPath(opfPath, it.href)
		if seen[name] {
			continue
		}
		isMarkup := strings.Contains(it.mediaType, "html") || isHTMLName(name)
		if !isMarkup {
			continue // an image or a stylesheet in the spine
		}
		if f := a.Find(name); f != nil {
			seen[name] = true
			out = append(out, f)
		}
	}
	return out
}

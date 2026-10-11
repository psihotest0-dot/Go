package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strings"
)

type DOCXExtractor struct{}

func NewDOCXExtractor() *DOCXExtractor {
	return &DOCXExtractor{}
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

func (e *DOCXExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", err
	}

	var documentXML *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			documentXML = f
			break
		}
	}

	if documentXML == nil {
		return "", nil // Or error "not a valid docx"
	}

	rc, err := documentXML.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	decoder := xml.NewDecoder(rc)
	var buf strings.Builder
	for {
		t, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch v := t.(type) {
		case xml.CharData:
			text := strings.TrimSpace(string(v))
			if text != "" {
				buf.WriteString(text + " ")
			}
		}
	}

	return strings.TrimSpace(buf.String()), nil
}

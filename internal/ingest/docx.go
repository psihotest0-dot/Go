package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
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

type DOCXNode struct {
	XMLName xml.Name
	Content string     `xml:",chardata"`
	Nodes   []DOCXNode `xml:",any"`
}

func extractDOCXText(n DOCXNode, buf *strings.Builder) {
	if n.XMLName.Local == "instrText" || n.XMLName.Local == "delText" || n.XMLName.Local == "commentReference" {
		return
	}
	if n.XMLName.Local == "t" {
		text := strings.TrimSpace(n.Content)
		if text != "" {
			buf.WriteString(text + " ")
		}
	}
	for _, child := range n.Nodes {
		extractDOCXText(child, buf)
	}
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
		return "", errors.New("not a valid docx")
	}

	rc, err := documentXML.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	docBytes, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}

	var root DOCXNode
	if err := xml.Unmarshal(docBytes, &root); err != nil {
		return "", err
	}

	var buf strings.Builder
	extractDOCXText(root, &buf)

	return strings.TrimSpace(buf.String()), nil
}

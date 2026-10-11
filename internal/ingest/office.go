package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strings"

	"github.com/xuri/excelize/v2"
)

type OfficeExtractor struct{}

func NewOfficeExtractor() *OfficeExtractor {
	return &OfficeExtractor{}
}

func (e *OfficeExtractor) Name() string {
	return "OfficeExtractor"
}

func (e *OfficeExtractor) Supports(mimeType, ext string) bool {
	return mimeType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" ||
		mimeType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
		ext == ".docx" || ext == ".xlsx"
}

func (e *OfficeExtractor) Extract(ctx context.Context, reader io.Reader) (string, error) {
	// Read all data first since zip needs ReaderAt
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}

	// Try extracting as Excel first
	if err := ctx.Err(); err != nil {
		return "", err
	}

	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err == nil {
		defer f.Close()
		var sb strings.Builder
		for _, name := range f.GetSheetMap() {
			rows, err := f.GetRows(name)
			if err != nil {
				continue
			}
			for _, row := range rows {
				for _, cell := range row {
					if cell != "" {
						sb.WriteString(cell + " ")
					}
				}
				sb.WriteString("\n")
			}
		}
		if sb.Len() > 0 {
			return strings.TrimSpace(sb.String()), nil
		}
	}

	// If excelize fails or returns empty, try parsing docx
	zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}

	for _, file := range zipReader.File {
		if file.Name == "word/document.xml" {
			return e.extractDocxXML(file)
		}
	}

	return "", nil
}

func (e *OfficeExtractor) extractDocxXML(file *zip.File) (string, error) {
	rc, err := file.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	decoder := xml.NewDecoder(rc)
	var sb strings.Builder
	inText := false

	for {
		t, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch se := t.(type) {
		case xml.StartElement:
			if se.Name.Local == "t" {
				inText = true
			}
		case xml.EndElement:
			if se.Name.Local == "t" {
				inText = false
				sb.WriteString(" ")
			} else if se.Name.Local == "p" {
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				sb.Write(se)
			}
		}
	}

	return strings.TrimSpace(sb.String()), nil
}

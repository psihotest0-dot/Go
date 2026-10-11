package ingest

import (
	"context"
	"io"
	"strings"

	"github.com/xuri/excelize/v2"
)

type XLSXExtractor struct{}

func NewXLSXExtractor() *XLSXExtractor {
	return &XLSXExtractor{}
}

func (e *XLSXExtractor) Name() string {
	return "XLSXExtractor"
}

func (e *XLSXExtractor) Supports(mimeType, ext string) bool {
	if strings.HasPrefix(mimeType, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet") {
		return true
	}
	return strings.ToLower(ext) == ".xlsx"
}

func (e *XLSXExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf strings.Builder

	sheets := f.GetSheetList()
	for _, sheet := range sheets {
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		for _, row := range rows {
			for _, cell := range row {
				if cell != "" {
					buf.WriteString(cell + " ")
				}
			}
			buf.WriteString("\n")
		}
	}

	return strings.TrimSpace(buf.String()), nil
}

package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/xuri/excelize/v2"
)

// xlsxUnzipXMLLimit is the size above which excelize keeps a worksheet on disk
// instead of in memory.
const xlsxUnzipXMLLimit = 32 << 20

type XLSXExtractor struct {
	limits zipLimits
}

func NewXLSXExtractor() *XLSXExtractor {
	return &XLSXExtractor{limits: defaultZipLimits}
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

// Extract renders every sheet as text, one line per row. The first non-empty row
// of a sheet is taken as its header, and every later row is written as
// "Header: value; Header: value", so that a chunk cut out of the middle of a big
// table still says what each value is. Rows are streamed (Rows) rather than
// loaded as one [][]string, and excelize is limited in how much it unzips.
func (e *XLSXExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	a, err := openZipArchive(r, e.limits)
	if err != nil {
		return "", err
	}
	defer a.Close()

	f, err := excelize.OpenFile(a.Path(), excelize.Options{
		UnzipSizeLimit:    e.limits.MaxTotalBytes,
		UnzipXMLSizeLimit: min(xlsxUnzipXMLLimit, e.limits.MaxTotalBytes),
	})
	if err != nil {
		return "", openError(err)
	}
	defer f.Close()

	tb := newTextBuilder()
	for _, sheet := range f.GetSheetList() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := writeSheet(ctx, f, sheet, tb); err != nil {
			if IsPermanent(err) || ctx.Err() != nil {
				return "", err
			}
			slog.Warn("failed to read sheet", slog.String("sheet", sheet), slog.Any("error", err))
		}
	}
	return strings.TrimSpace(tb.String()), nil
}

func writeSheet(ctx context.Context, f *excelize.File, sheet string, tb *textBuilder) error {
	rows, err := f.Rows(sheet)
	if err != nil {
		return err
	}
	defer rows.Close()

	var header []string
	for n := 0; rows.Next(); n++ {
		if n%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		line := rowText(header, cols)
		if line == "" {
			continue
		}
		if header == nil {
			header = cols
		}
		if err := tb.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return rows.Error()
}

// rowText renders one row as "Header: value; Header: value". The first row of a
// sheet (header == nil) is listed as it is.
func rowText(header, cols []string) string {
	var parts []string
	for i, cell := range cols {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			continue
		}
		if i < len(header) {
			if name := strings.TrimSpace(header[i]); name != "" {
				cell = name + ": " + cell
			}
		}
		parts = append(parts, cell)
	}
	return strings.Join(parts, "; ")
}

// openError turns excelize's errors into the ones the worker knows how to treat.
func openError(err error) error {
	switch {
	case errors.Is(err, excelize.ErrWorkbookPassword):
		return fmt.Errorf("%w: the workbook is encrypted", ErrPasswordProtected)
	case errors.Is(err, excelize.ErrWorkbookFileFormat):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	case strings.Contains(err.Error(), "unzip size exceeds"):
		return fmt.Errorf("%w: %v", ErrArchiveTooLarge, err)
	}
	return fmt.Errorf("failed to open the workbook: %w", err)
}

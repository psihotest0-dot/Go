package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

const (
	// pdfMinNativeRunes is the amount of extractable text below which a page is
	// treated as a scan and sent to OCR.
	pdfMinNativeRunes = 50

	// pdfTextTimeout bounds one pdftotext run over the whole document. Native
	// extraction takes milliseconds per page, so this only trips on a hung process.
	pdfTextTimeout = 3 * time.Minute

	// pdfRasterTimeout bounds rasterising a single page for OCR.
	pdfRasterTimeout = 60 * time.Second
)

// popplerGlobs are the usual Windows install locations of Poppler. Git for Windows
// ships Xpdf's pdftotext, which can come first in PATH and is a different program;
// an installed Poppler is preferred over whatever PATH finds.
var popplerGlobs = []string{
	`C:\poppler*\Library\bin`,
	`C:\poppler*\bin`,
	`C:\Program Files\poppler*\Library\bin`,
	`C:\Program Files\poppler*\bin`,
}

// resolvePDFTool returns the executable to run for a Poppler/Xpdf tool: the path in
// envVar when set, else an installed Poppler (Windows), else the bare name that
// exec resolves through PATH.
func resolvePDFTool(envVar, name string) string {
	if p := strings.TrimSpace(os.Getenv(envVar)); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, dir := range popplerGlobs {
			matches, _ := filepath.Glob(filepath.Join(dir, name+".exe"))
			if len(matches) > 0 {
				sort.Strings(matches)
				return matches[len(matches)-1]
			}
		}
	}
	return name
}

// pdfToolVersion returns the first line of `<tool> -v`, which the Poppler and Xpdf
// tools print to stderr.
func pdfToolVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path, "-v").CombinedOutput()
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

type PDFExtractor struct {
	pdftotext string
	pdftoppm  string
}

// NewPDFExtractor resolves the external PDF tools (PDFTOTEXT_PATH, PDFTOPPM_PATH)
// and logs which binaries will be used, so a wrong pdftotext is visible at startup.
func NewPDFExtractor() *PDFExtractor {
	e := &PDFExtractor{
		pdftotext: resolvePDFTool("PDFTOTEXT_PATH", "pdftotext"),
		pdftoppm:  resolvePDFTool("PDFTOPPM_PATH", "pdftoppm"),
	}
	for _, tool := range []string{e.pdftotext, e.pdftoppm} {
		resolved, err := exec.LookPath(tool)
		if err != nil {
			slog.Warn("PDF tool not found; PDF ingest will fail or fall back to nothing",
				slog.String("tool", tool), slog.Any("error", err))
			continue
		}
		slog.Info("PDF tool", slog.String("path", resolved), slog.String("version", pdfToolVersion(resolved)))
	}
	return e
}

func (e *PDFExtractor) Name() string {
	return "PDFExtractor"
}

func (e *PDFExtractor) Supports(mimeType, ext string) bool {
	if strings.HasPrefix(mimeType, "application/pdf") {
		return true
	}
	return strings.ToLower(ext) == ".pdf"
}

func (e *PDFExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	// Create a temporary file because pdftoppm/pdftotext work best with physical files.
	tmpFile, err := os.CreateTemp("", "upload-*.pdf")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for PDF: %w", err)
	}
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()

	if _, wErr := tmpFile.Write(b); wErr != nil {
		return "", fmt.Errorf("failed to write to temp file: %w", wErr)
	}

	// 1. Try to extract text using pdftotext for the entire file. -enc UTF-8 is
	// required: Xpdf's pdftotext defaults to Latin-1 and silently drops every
	// Cyrillic letter, which makes a text PDF look like a scan.
	txtCtx, cancelTxt := context.WithTimeout(ctx, pdfTextTimeout)
	txtOut, err := exec.CommandContext(txtCtx, e.pdftotext, "-enc", "UTF-8", "-layout", tmpFile.Name(), "-").Output()
	cancelTxt()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		slog.Warn("pdftotext failed for entire file, proceeding to fallback/OCR", slog.Any("error", err))
	}

	// Split extracted text by page using form feed \f
	pages := strings.Split(string(txtOut), "\f")

	// Determine the number of pages using pdfcpu.
	rs := bytes.NewReader(b)
	conf := model.NewDefaultConfiguration()
	pageCount, err := api.PageCount(ctx, rs, conf)
	if err != nil {
		return "", fmt.Errorf("failed to get PDF page count: %w", err)
	}

	var finalBuf strings.Builder
	var lastErr error
	imgExtractor := NewImageOCRExtractor()

	// Process page by page
	for i := 1; i <= pageCount; i++ {
		pageStr := fmt.Sprintf("%d", i)
		var pageText string
		if i-1 < len(pages) {
			pageText = strings.TrimSpace(pages[i-1])
		}

		// 2. Enough native text: use it as is.
		if utf8.RuneCountInString(pageText) >= pdfMinNativeRunes {
			finalBuf.WriteString(pageText)
			finalBuf.WriteString("\n")
			continue
		}

		// 3. Otherwise assume a scanned page and fall back to OCR.
		ocrText, err := e.ocrPage(ctx, tmpFile.Name(), pageStr, imgExtractor)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			slog.Warn("OCR failed for page", slog.String("page", pageStr), slog.Any("error", err))
			lastErr = err
		}
		switch {
		case ocrText != "":
			finalBuf.WriteString(ocrText)
			finalBuf.WriteString("\n[source: \"ocr\"]\n")
		case pageText != "":
			// A short native page (a title, a page number) beats nothing.
			finalBuf.WriteString(pageText)
			finalBuf.WriteString("\n")
		}
	}

	text := strings.TrimSpace(finalBuf.String())
	if text == "" {
		// An empty result must not look like a successful ingest with zero chunks.
		if lastErr != nil {
			return "", fmt.Errorf("no text could be extracted from the PDF (%d pages): %w", pageCount, lastErr)
		}
		return "", fmt.Errorf("no text could be extracted from the PDF (%d pages)", pageCount)
	}
	return text, nil
}

// ocrPage rasterises one page and runs OCR on it. It returns trimmed text, which is
// empty when the page has nothing readable on it.
func (e *PDFExtractor) ocrPage(ctx context.Context, pdfPath, page string, ocr *ImageOCRExtractor) (string, error) {
	rasterCtx, cancel := context.WithTimeout(ctx, pdfRasterTimeout)
	defer cancel()

	png, err := exec.CommandContext(rasterCtx, e.pdftoppm, "-r", "300", "-f", page, "-l", page, "-png", pdfPath).Output()
	if err != nil {
		return "", fmt.Errorf("pdftoppm failed for page %s: %w", page, err)
	}

	text, err := ocr.Extract(ctx, bytes.NewReader(png))
	if err != nil {
		return "", fmt.Errorf("page %s: %w", page, err)
	}
	return strings.TrimSpace(text), nil
}

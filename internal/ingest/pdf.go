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
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// pdfMinNativeRunes is the amount of extractable text below which a page is
	// treated as a scan and sent to OCR.
	pdfMinNativeRunes = 50

	// pdfTextTimeout bounds one pdftotext run over the whole document. Native
	// extraction takes milliseconds per page, so this only trips on a hung process.
	pdfTextTimeout = 3 * time.Minute

	// pdfInfoTimeout bounds pdfinfo, which only reads the document catalogue.
	pdfInfoTimeout = 30 * time.Second

	// pdfRasterTimeout bounds rasterising a single page for OCR.
	pdfRasterTimeout = 60 * time.Second

	// The whole extraction gets pdfBaseTimeout plus pdfPerPageTimeout for every
	// page. A page costs ~1.5 s of OCR and four are done at once, so this is more
	// than ten times what a healthy run needs, while a stuck one still ends.
	pdfBaseTimeout    = 10 * time.Minute
	pdfPerPageTimeout = 6 * time.Second

	// pdfProgressEvery is how many OCR'd pages are logged as one progress line.
	pdfProgressEvery = 10

	// pdfOCRMinFailures is how many failed pages are tolerated before OCR is
	// given up on, provided that more than half of the pages so far failed. A
	// missing or broken tesseract fails every page; waiting for all of them would
	// only take longer to say so.
	pdfOCRMinFailures = 10
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
	pdfinfo   string

	dpi         int
	concurrency int

	// ocrPageFn recognises one page. It is a field so that tests can count and
	// time the pages without a scanned PDF and without tesseract.
	ocrPageFn func(ctx context.Context, pdfPath string, page int) (string, error)
}

// NewPDFExtractor resolves the external PDF tools (PDFTOTEXT_PATH, PDFTOPPM_PATH,
// PDFINFO_PATH) and logs which binaries will be used, so a wrong pdftotext is
// visible at startup. OCR_DPI and OCR_CONCURRENCY tune the OCR of scanned pages.
func NewPDFExtractor() *PDFExtractor {
	e := &PDFExtractor{
		pdftotext:   resolvePDFTool("PDFTOTEXT_PATH", "pdftotext"),
		pdftoppm:    resolvePDFTool("PDFTOPPM_PATH", "pdftoppm"),
		pdfinfo:     resolvePDFTool("PDFINFO_PATH", "pdfinfo"),
		dpi:         envInt("OCR_DPI", defaultOCRDPI, 72, 600),
		concurrency: envInt("OCR_CONCURRENCY", defaultOCRConcurrency, 1, 64),
	}
	e.ocrPageFn = e.ocrPage
	for _, tool := range []string{e.pdftotext, e.pdftoppm, e.pdfinfo} {
		resolved, err := exec.LookPath(tool)
		if err != nil {
			slog.Warn("PDF tool not found; PDF ingest will fail or fall back to nothing",
				slog.String("tool", tool), slog.Any("error", err))
			continue
		}
		slog.Info("PDF tool", slog.String("path", resolved), slog.String("version", pdfToolVersion(resolved)))
	}
	slog.Info("PDF OCR settings", slog.Int("dpi", e.dpi), slog.Int("pages_at_once", e.concurrency))
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

// runTool runs a Poppler tool and returns its stdout and stderr.
func runTool(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}

func isPasswordError(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "incorrect password")
}

var pdfPagesLine = regexp.MustCompile(`(?m)^Pages:\s+(\d+)`)

// pageCount asks pdfinfo how many pages the document has. The second result is
// false when it could not say (the tool is missing or the file is odd), in which
// case the caller counts form feeds in the pdftotext output instead; a document
// that needs a password is an error.
func (e *PDFExtractor) pageCount(ctx context.Context, pdfPath string) (int, bool, error) {
	out, stderr, err := runTool(ctx, pdfInfoTimeout, e.pdfinfo, pdfPath)
	if isPasswordError(stderr) {
		return 0, false, fmt.Errorf("%w: the PDF needs a password to be opened", ErrPasswordProtected)
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
		slog.Warn("pdfinfo failed, counting pages from the extracted text", slog.Any("error", err))
		return 0, false, nil
	}
	m := pdfPagesLine.FindSubmatch(out)
	if m == nil {
		return 0, false, nil
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil || n <= 0 {
		return 0, false, nil
	}
	return n, true, nil
}

// pdfDeadline is how long extracting a document of the given size may take.
func pdfDeadline(pages int) time.Duration {
	return pdfBaseTimeout + time.Duration(pages)*pdfPerPageTimeout
}

func (e *PDFExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	// pdftotext and pdftoppm need a file, and a 100 MB scan should not be held in
	// memory as well, so the upload is copied straight to disk.
	tmpFile, err := os.CreateTemp("", "upload-*.pdf")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for PDF: %w", err)
	}
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()
	if _, err := io.Copy(tmpFile, r); err != nil {
		return "", fmt.Errorf("failed to write to temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", err
	}
	pdfPath := tmpFile.Name()

	pageTotal, known, err := e.pageCount(ctx, pdfPath)
	if err != nil {
		return "", err
	}
	if known {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pdfDeadline(pageTotal))
		defer cancel()
	}

	// 1. Try to extract text using pdftotext for the entire file. -enc UTF-8 is
	// required: Xpdf's pdftotext defaults to Latin-1 and silently drops every
	// Cyrillic letter, which makes a text PDF look like a scan.
	txtOut, stderr, txtErr := runTool(ctx, pdfTextTimeout, e.pdftotext, "-enc", "UTF-8", "-layout", pdfPath, "-")
	if txtErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if isPasswordError(stderr) {
			return "", fmt.Errorf("%w: the PDF needs a password to be opened", ErrPasswordProtected)
		}
		slog.Warn("pdftotext failed for entire file, proceeding to fallback/OCR", slog.Any("error", txtErr))
	}

	// pdftotext ends every page with a form feed.
	pages := strings.Split(string(txtOut), "\f")
	if !known {
		// Without pdfinfo the form feeds are the page count; the piece after the
		// last one is not a page.
		pageTotal = len(pages) - 1
		if pageTotal <= 0 {
			return "", fmt.Errorf("could not determine the number of pages of the PDF (pdftotext: %v)", txtErr)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pdfDeadline(pageTotal))
		defer cancel()
	}

	texts := make([]string, pageTotal) // the text of every page, in page order
	var scans []int                    // pages that need OCR
	for i := 1; i <= pageTotal; i++ {
		var pageText string
		if i-1 < len(pages) {
			pageText = strings.TrimSpace(pages[i-1])
		}
		texts[i-1] = pageText
		// 2. Enough native text: use it as is. 3. Otherwise assume a scan.
		if utf8.RuneCountInString(pageText) < pdfMinNativeRunes {
			scans = append(scans, i)
		}
	}

	var ocr ocrOutcome
	if len(scans) > 0 {
		ocr, err = e.ocrPages(ctx, pdfPath, scans, newPageStore(checkpointDirFrom(ctx), e.dpi))
		if err != nil {
			return "", err
		}
	}

	var finalBuf strings.Builder
	for i := 1; i <= pageTotal; i++ {
		switch ocrText := ocr.text[i]; {
		case ocrText != "":
			finalBuf.WriteString(ocrText)
			finalBuf.WriteString("\n[source: \"ocr\"]\n")
		case texts[i-1] != "":
			// Native text, or a short native page (a title, a page number) that
			// beats nothing.
			finalBuf.WriteString(texts[i-1])
			finalBuf.WriteString("\n")
		}
	}

	// Words broken across lines (native text and OCR alike) are stitched together
	// over the whole document, so that the rest of the book can settle the
	// ambiguous breaks. The pages saved in the checkpoint stay as recognised.
	text := strings.TrimSpace(JoinHyphenated(finalBuf.String()))
	if text == "" {
		// An empty result must not look like a successful ingest with zero chunks.
		if ocr.lastErr != nil {
			// OCR itself failed: this may pass on a retry, so it is not ErrNoText.
			return "", fmt.Errorf("no text could be extracted from the PDF (%d pages): %w", pageTotal, ocr.lastErr)
		}
		return "", fmt.Errorf("%w: the PDF has %d pages and none of them contains readable text", ErrNoText, pageTotal)
	}
	return text, nil
}

// ocrOutcome is the result of recognising a set of pages.
type ocrOutcome struct {
	text    map[int]string // by page number; blank pages map to ""
	lastErr error          // the last page error, nil if every page was recognised
}

// ocrPages recognises the given pages with up to e.concurrency workers and
// returns the text by page number. Pages saved by an earlier, interrupted run
// are taken from the store and every finished page is saved, so a retry starts
// where the last run stopped. A page that fails is logged and left out; the last
// such error is returned so the caller can tell "blank" from "broken".
func (e *PDFExtractor) ocrPages(ctx context.Context, pdfPath string, pages []int, store *pageStore) (ocrOutcome, error) {
	results := make(map[int]string, len(pages))
	var todo []int
	for _, p := range pages {
		if text, ok := store.Get(p); ok {
			results[p] = text
			continue
		}
		todo = append(todo, p)
	}
	if resumed := len(pages) - len(todo); resumed > 0 {
		slog.Info("PDF OCR resumes from saved pages", slog.Int("saved", resumed), slog.Int("remaining", len(todo)))
	}
	if len(todo) == 0 {
		return ocrOutcome{text: results}, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu      sync.Mutex
		lastErr error
		done    atomic.Int64
		failed  atomic.Int64
		aborted atomic.Bool
		queue   = make(chan int)
		wg      sync.WaitGroup
		start   = time.Now()
	)
	workers := min(e.concurrency, len(todo))
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for page := range queue {
				text, err := e.recogniseOne(ctx, pdfPath, page)
				n := done.Add(1)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					slog.Warn("OCR failed for page", slog.Int("page", page), slog.Any("error", err))
					f := failed.Add(1)
					mu.Lock()
					lastErr = err
					mu.Unlock()
					if f >= pdfOCRMinFailures && f*2 > n {
						aborted.Store(true)
						cancel()
						return
					}
					continue
				}
				if putErr := store.Put(page, text); putErr != nil {
					slog.Warn("could not save the OCR text of a page", slog.Int("page", page), slog.Any("error", putErr))
				}
				mu.Lock()
				results[page] = text
				mu.Unlock()
				if n%pdfProgressEvery == 0 {
					slog.Info("PDF OCR progress", slog.Int64("done", n), slog.Int("of", len(todo)),
						slog.Duration("elapsed", time.Since(start).Round(time.Second)))
				}
			}
		}()
	}

feed:
	for _, p := range todo {
		select {
		case queue <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()

	if aborted.Load() {
		return ocrOutcome{}, fmt.Errorf("OCR failed for %d of %d pages, giving up: %w", failed.Load(), done.Load(), lastErr)
	}
	if err := ctx.Err(); err != nil {
		return ocrOutcome{}, err
	}
	return ocrOutcome{text: results, lastErr: lastErr}, nil
}

// recogniseOne runs the OCR of one page, holding one of the process-wide slots.
func (e *PDFExtractor) recogniseOne(ctx context.Context, pdfPath string, page int) (string, error) {
	release, err := acquireOCR(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return e.ocrPageFn(ctx, pdfPath, page)
}

// ocrPage rasterises one page and runs OCR on it. It returns trimmed text, which is
// empty when the page has nothing readable on it.
func (e *PDFExtractor) ocrPage(ctx context.Context, pdfPath string, page int) (string, error) {
	pageArg := strconv.Itoa(page)
	png, stderr, err := runTool(ctx, pdfRasterTimeout, e.pdftoppm, "-r", strconv.Itoa(e.dpi), "-f", pageArg, "-l", pageArg, "-png", pdfPath)
	if err != nil {
		return "", fmt.Errorf("pdftoppm failed for page %d: %w (stderr: %s)", page, err, strings.TrimSpace(stderr))
	}

	text, err := runTesseract(ctx, bytes.NewReader(png), true)
	if err != nil {
		return "", fmt.Errorf("page %d: %w", page, err)
	}
	return strings.TrimSpace(text), nil
}

package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// blankPagesPDF builds a valid PDF of n pages without any content: neither
// pdftotext nor a real OCR finds text on it, so every page is an OCR candidate.
func blankPagesPDF(n int) []byte {
	var b bytes.Buffer
	offsets := make([]int, 0, n+2)
	obj := func(s string) {
		offsets = append(offsets, b.Len())
		b.WriteString(s)
	}
	b.WriteString("%PDF-1.4\n")
	obj("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	var kids strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&kids, "%d 0 R ", i+3)
	}
	obj(fmt.Sprintf("2 0 obj\n<< /Type /Pages /Kids [%s] /Count %d >>\nendobj\n", kids.String(), n))
	for i := 0; i < n; i++ {
		obj(fmt.Sprintf("%d 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n", i+3))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", n+3)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", n+3, xref)
	return b.Bytes()
}

// pdfWithFakeOCR returns an extractor whose per-page OCR is fn. The PDF tools
// (pdfinfo, pdftotext) are the real ones; only the slow part is replaced.
func pdfWithFakeOCR(t *testing.T, concurrency int, fn func(ctx context.Context, page int) (string, error)) *PDFExtractor {
	t.Helper()
	e := NewPDFExtractor()
	requireTools(t, e.pdftotext, e.pdfinfo)
	e.concurrency = concurrency
	e.ocrPageFn = func(ctx context.Context, _ string, page int) (string, error) { return fn(ctx, page) }
	return e
}

func pageText(page int) string { return fmt.Sprintf("Текст на страница %d", page) }

func TestPDFOCRRunsPagesInParallelAndKeepsOrder(t *testing.T) {
	const pages, perPage = 20, 40 * time.Millisecond
	var inFlight, peak atomic.Int32
	ocr := func(ctx context.Context, page int) (string, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		// Uneven times, so pages finish out of order.
		time.Sleep(perPage + time.Duration(rand.Intn(30))*time.Millisecond)
		return pageText(page), nil
	}

	// The slot semaphore is shared by the whole process; make sure it allows 4.
	t.Setenv("OCR_CONCURRENCY", "4")
	e := pdfWithFakeOCR(t, 4, ocr)

	start := time.Now()
	text, err := e.Extract(context.Background(), bytes.NewReader(blankPagesPDF(pages)))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}

	// Order: page 1 .. page 20, each exactly once.
	last := -1
	for p := 1; p <= pages; p++ {
		idx := strings.Index(text, pageText(p)+"\n")
		if idx < 0 {
			t.Fatalf("page %d is missing from %q", p, text)
		}
		if idx < last {
			t.Fatalf("page %d is out of order", p)
		}
		last = idx
	}
	if got := strings.Count(text, `[source: "ocr"]`); got != pages {
		t.Errorf("%d pages marked as OCR, want %d", got, pages)
	}

	if peak.Load() < 2 || peak.Load() > 4 {
		t.Errorf("peak concurrency %d, want 2..4", peak.Load())
	}
	// Sequentially this is at least 20 * 40 ms = 800 ms; four at a time ~1/4 of the average.
	sequential := time.Duration(pages) * (perPage + 15*time.Millisecond)
	if elapsed > sequential/2 {
		t.Errorf("took %s, sequential would be about %s: pages were not processed in parallel", elapsed, sequential)
	}
	t.Logf("%d pages in %s (sequential ~%s), peak concurrency %d", pages, elapsed, sequential, peak.Load())
}

// An interrupted run (timeout, restart, a failure on page 14) must continue from
// the pages that were already recognised, not start again from page 1.
func TestPDFOCRResumesFromSavedPages(t *testing.T) {
	const pages = 12
	dir := t.TempDir()
	ctx := WithCheckpointDir(context.Background(), dir)
	pdf := blankPagesPDF(pages)

	var mu sync.Mutex
	calls := map[int]int{}
	var failFrom atomic.Int32
	failFrom.Store(7) // the first run cannot recognise page 7 and later
	ocr := func(ctx context.Context, page int) (string, error) {
		mu.Lock()
		calls[page]++
		mu.Unlock()
		if int32(page) >= failFrom.Load() {
			return "", errors.New("tesseract crashed")
		}
		return pageText(page), nil
	}
	t.Setenv("OCR_CONCURRENCY", "4")
	e := pdfWithFakeOCR(t, 1, ocr) // one worker: pages are tried in order

	// Run 1: pages 1-6 succeed, 7.. fail. Fewer than 10 failures, so it carries on and finishes.
	text, err := e.Extract(ctx, bytes.NewReader(pdf))
	if err != nil {
		t.Fatal(err)
	}
	for p := 1; p <= 6; p++ {
		if !strings.Contains(text, pageText(p)) {
			t.Fatalf("run 1: page %d is missing", p)
		}
	}
	if strings.Contains(text, pageText(7)) {
		t.Fatal("run 1: page 7 should have failed")
	}

	// Run 2: the OCR works now. Only the pages that did not make it are recognised.
	failFrom.Store(1000)
	mu.Lock()
	for p := range calls {
		calls[p] = 0
	}
	mu.Unlock()
	text, err = e.Extract(ctx, bytes.NewReader(pdf))
	if err != nil {
		t.Fatal(err)
	}
	for p := 1; p <= pages; p++ {
		if !strings.Contains(text, pageText(p)) {
			t.Errorf("run 2: page %d is missing", p)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for p := 1; p <= 6; p++ {
		if calls[p] != 0 {
			t.Errorf("page %d was recognised again after it had been saved", p)
		}
	}
	for p := 7; p <= pages; p++ {
		if calls[p] != 1 {
			t.Errorf("page %d recognised %d times in run 2, want 1", p, calls[p])
		}
	}

	// ClearCheckpoint throws the saved pages away.
	if err := ClearCheckpoint(dir); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("checkpoint directory is still there")
	}
}

func TestPDFOCRCheckpointIsPerResolution(t *testing.T) {
	dir := t.TempDir()
	store := newPageStore(dir, 250)
	if err := store.Put(3, "стар текст"); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.Get(3); !ok || got != "стар текст" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
	if _, ok := newPageStore(dir, 300).Get(3); ok {
		t.Error("a page recognised at another resolution must not be reused")
	}
	// A blank page is saved too, so it is not recognised again.
	if err := store.Put(4, ""); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.Get(4); !ok || got != "" {
		t.Errorf("blank page: %q, %v", got, ok)
	}
	var none *pageStore
	if _, ok := none.Get(1); ok || none.Put(1, "x") != nil {
		t.Error("a nil store must be a harmless no-op")
	}
}

func TestPDFOCRGivesUpWhenEveryPageFails(t *testing.T) {
	var calls atomic.Int32
	e := pdfWithFakeOCR(t, 2, func(ctx context.Context, page int) (string, error) {
		calls.Add(1)
		return "", errors.New("tesseract is not installed")
	})

	_, err := e.Extract(context.Background(), bytes.NewReader(blankPagesPDF(60)))
	if err == nil || !strings.Contains(err.Error(), "giving up") {
		t.Fatalf("err = %v, want a 'giving up' error", err)
	}
	if IsPermanent(err) {
		t.Error("a broken OCR is not a property of the file: the job must be retried")
	}
	if n := calls.Load(); n >= 60 {
		t.Errorf("OCR was attempted on all %d pages instead of stopping early", n)
	}
}

func TestPDFOCRBlankScanIsPermanentNoText(t *testing.T) {
	e := pdfWithFakeOCR(t, 4, func(ctx context.Context, page int) (string, error) { return "", nil })

	_, err := e.Extract(context.Background(), bytes.NewReader(blankPagesPDF(5)))
	if !errors.Is(err, ErrNoText) || !IsPermanent(err) {
		t.Fatalf("err = %v, want ErrNoText", err)
	}
}

func TestPDFOCRStopsOnCancel(t *testing.T) {
	started := make(chan struct{}, 64)
	e := pdfWithFakeOCR(t, 4, func(ctx context.Context, page int) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Extract(ctx, bytes.NewReader(blankPagesPDF(30)))
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Extract did not stop after the context was cancelled")
	}
}

func TestPDFPageCountFallsBackToFormFeeds(t *testing.T) {
	e := pdfWithFakeOCR(t, 2, func(ctx context.Context, page int) (string, error) { return pageText(page), nil })
	e.pdfinfo = "pdfinfo-that-does-not-exist"

	text, err := e.Extract(context.Background(), bytes.NewReader(blankPagesPDF(7)))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(text, `[source: "ocr"]`); got != 7 {
		t.Errorf("%d pages recognised, want 7 (counted from form feeds)", got)
	}
}

func TestPDFDeadlineGrowsWithPages(t *testing.T) {
	if pdfDeadline(1000) <= pdfDeadline(10) {
		t.Fatal("the deadline must depend on the number of pages")
	}
	if got := pdfDeadline(1000); got < 90*time.Minute {
		t.Errorf("a 1000 page scan gets only %s", got)
	}
}

func encryptedPDF(t *testing.T, plain []byte, userPW, ownerPW string) []byte {
	t.Helper()
	var out bytes.Buffer
	conf := model.NewAESConfiguration(userPW, ownerPW, 256)
	if err := api.Encrypt(context.Background(), bytes.NewReader(plain), &out, conf); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestPDFEncrypted(t *testing.T) {
	plain, err := os.ReadFile(filepath.Join("testdata", "bulgarian.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	e := NewPDFExtractor()
	requireTools(t, e.pdftotext, e.pdfinfo)

	t.Run("password to open", func(t *testing.T) {
		_, err := e.Extract(context.Background(), bytes.NewReader(encryptedPDF(t, plain, "secret", "owner")))
		if !errors.Is(err, ErrPasswordProtected) || !IsPermanent(err) {
			t.Fatalf("err = %v, want ErrPasswordProtected", err)
		}
	})
	t.Run("owner password only", func(t *testing.T) {
		// Anyone can open it (the user password is empty), so it is an ordinary PDF.
		text, err := e.Extract(context.Background(), bytes.NewReader(encryptedPDF(t, plain, "", "owner")))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "Дунав") {
			t.Errorf("text not extracted: %q", firstRunes(text, 100))
		}
	})
}

// Words broken at the end of a line are stitched back together over the whole
// document: a break on one page is settled by a word on another, and the pages
// saved for a resumed run stay as they were recognised.
func TestPDFTextHasItsBrokenWordsJoined(t *testing.T) {
	pages := map[int]string{
		1: "Железни свързани гривни на арестувани или зат-\nворници. Той беше по-\nголям от брат си.",
		2: "Затворници и белетристи-\nчески жанрове. Вятър от северо-\nзапад. Северо-запад е посока.",
	}
	dir := t.TempDir()
	ctx := WithCheckpointDir(context.Background(), dir)
	t.Setenv("OCR_CONCURRENCY", "4")
	e := pdfWithFakeOCR(t, 2, func(ctx context.Context, page int) (string, error) { return pages[page], nil })

	text, err := e.Extract(ctx, bytes.NewReader(blankPagesPDF(2)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"арестувани или затворници.", "по-голям от брат си", "белетристически жанрове", "от северо-запад."} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is missing from:\n%s", want, text)
		}
	}
	if strings.Contains(text, "-\n") {
		t.Errorf("a break was left:\n%s", text)
	}

	// The checkpoint keeps the raw recognised text.
	saved, ok := newPageStore(dir, e.dpi).Get(1)
	if !ok || !strings.Contains(saved, "зат-\nворници") {
		t.Errorf("the saved page was changed: %q", saved)
	}
}

// The page workers are goroutines outside River's recovery: a panic in one of
// them would end the whole process, not just this book. recogniseOne turns it
// into the failure of that page, which the extractor already counts.
func TestRecogniseOneTurnsAPanicIntoAPageError(t *testing.T) {
	e := NewPDFExtractor()
	e.ocrPageFn = func(context.Context, string, int) (string, error) { panic("tesseract bindings blew up") }

	text, err := e.recogniseOne(context.Background(), "book.pdf", 7)

	if err == nil || !strings.Contains(err.Error(), "page 7 panicked") {
		t.Fatalf("err = %v, want a 'page 7 panicked' error", err)
	}
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
	// The OCR slot was released: the next page is not stuck behind the panic.
	e.ocrPageFn = func(context.Context, string, int) (string, error) { return "ok", nil }
	if got, err := e.recogniseOne(context.Background(), "book.pdf", 8); err != nil || got != "ok" {
		t.Errorf("after the panic: %q, %v", got, err)
	}
}

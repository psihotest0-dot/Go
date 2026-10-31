package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode"
)

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

// blankPDF builds a valid one-page PDF with no content stream, so neither
// pdftotext nor OCR can find any text on it.
func blankPDF() []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(s string) {
		offsets = append(offsets, b.Len())
		b.WriteString(s)
	}
	b.WriteString("%PDF-1.4\n")
	obj("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	obj("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	obj("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n")
	xref := b.Len()
	b.WriteString("xref\n0 4\n0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

func TestResolvePDFTool(t *testing.T) {
	t.Run("env var wins", func(t *testing.T) {
		t.Setenv("PDFTOTEXT_PATH", `  /opt/poppler/bin/pdftotext  `)
		if got := resolvePDFTool("PDFTOTEXT_PATH", "pdftotext"); got != "/opt/poppler/bin/pdftotext" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("falls back to the bare name", func(t *testing.T) {
		t.Setenv("PDFTOTEXT_PATH", "")
		old := popplerGlobs
		popplerGlobs = nil
		t.Cleanup(func() { popplerGlobs = old })
		if got := resolvePDFTool("PDFTOTEXT_PATH", "pdftotext"); got != "pdftotext" {
			t.Fatalf("got %q", got)
		}
	})
}

// A Bulgarian text PDF must come out of native extraction: with Xpdf's default
// encoding every Cyrillic letter was dropped, the page looked empty and the whole
// document went through OCR (about 50x slower, and lossy).
func TestPDFExtractorCyrillicNativeText(t *testing.T) {
	requireTools(t, NewPDFExtractor().pdftotext)

	data, err := os.ReadFile("testdata/bulgarian.pdf")
	if err != nil {
		t.Fatal(err)
	}

	text, err := NewPDFExtractor().Extract(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	cyrillic := 0
	for _, r := range text {
		if unicode.Is(unicode.Cyrillic, r) {
			cyrillic++
		}
	}
	if cyrillic < 2000 {
		t.Errorf("got %d Cyrillic letters, want at least 2000; text starts %q", cyrillic, firstRunes(text, 80))
	}
	if strings.Contains(text, `[source: "ocr"]`) {
		t.Error("text PDF went through OCR; expected native extraction")
	}
	if !strings.Contains(text, "Дунав") {
		t.Errorf("expected the word Дунав in the text; got %q", firstRunes(text, 200))
	}
}

func TestPDFExtractorFailsWhenNoTextAnywhere(t *testing.T) {
	ex := NewPDFExtractor()
	requireTools(t, ex.pdftotext, ex.pdftoppm, "tesseract")

	text, err := ex.Extract(context.Background(), bytes.NewReader(blankPDF()))
	if err == nil {
		t.Fatalf("expected an error for a PDF without any text, got text %q", text)
	}
	if !strings.Contains(err.Error(), "no text could be extracted") {
		t.Errorf("unexpected error: %v", err)
	}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

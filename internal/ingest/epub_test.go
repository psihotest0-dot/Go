package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"testing"
)

func TestEPUBExtractor(t *testing.T) {
	// Create a dummy EPUB (ZIP) file in memory
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	// Add a dummy HTML file inside
	f, err := w.Create("OEBPS/content.html")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write([]byte("<html><body><p>Hello EPUB</p><script>console.log('ignore')</script></body></html>"))
	if err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if err != nil {
		t.Fatal(err)
	}

	extractor := NewEPUBExtractor()

	// Test Extract
	text, err := extractor.Extract(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	expected := "Hello EPUB"
	if text != expected {
		t.Errorf("Expected '%s', got '%s'", expected, text)
	}

	// Test Supports
	if !extractor.Supports("application/epub+zip", ".epub") {
		t.Errorf("Expected Supports to return true for application/epub+zip and .epub")
	}
}

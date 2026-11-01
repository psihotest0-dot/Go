package ingest

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

// newZipWriter compresses with BestSpeed: the bombs below are hundreds of
// megabytes of repeated text, and only their size matters.
func newZipWriter(w io.Writer) *zip.Writer {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	return zw
}

func addEntry(t *testing.T, zw *zip.Writer, name, content string) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
}

// addRepeated writes head, unit repeated until at least size bytes, and tail.
func addRepeated(t *testing.T, zw *zip.Writer, name, head, unit, tail string, size int) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, head); err != nil {
		t.Fatal(err)
	}
	block := strings.Repeat(unit, max(1, (1<<20)/len(unit)))
	for written := 0; written < size; written += len(block) {
		if _, err := io.WriteString(w, block); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.WriteString(w, tail); err != nil {
		t.Fatal(err)
	}
}

// peakHeap runs fn and returns how far the live heap grew above its level before
// the call. It samples, so it can miss a very short spike; the spikes this guards
// against last for the whole of an extraction.
func peakHeap(fn func()) uint64 {
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak {
				peak = ms.HeapAlloc
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	fn()
	close(stop)
	<-done
	if peak < base.HeapAlloc {
		return 0
	}
	return peak - base.HeapAlloc
}

const (
	wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"`
	mib = 1 << 20
)

func docxArchive(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	addEntry(t, zw, "word/document.xml", `<?xml version="1.0" encoding="UTF-8"?><w:document `+wNS+`><w:body>`+body+`</w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDOCXText(t *testing.T) {
	body := `
<w:p><w:r><w:t>Hel</w:t></w:r><w:r><w:t xml:space="preserve">lo world</w:t></w:r></w:p>
<w:p><w:r><w:t>Втори</w:t></w:r><w:r><w:tab/></w:r><w:r><w:t>абзац</w:t></w:r></w:p>
<w:p/><w:p/>
<w:p><w:r><w:t>ред1</w:t><w:br/><w:t>ред2</w:t></w:r></w:p>
<w:tbl><w:tr>
  <w:tc><w:p><w:r><w:t>A1</w:t></w:r></w:p></w:tc>
  <w:tc><w:p><w:r><w:t>B1</w:t></w:r></w:p></w:tc>
</w:tr></w:tbl>
<mc:AlternateContent>
  <mc:Choice Requires="wps"><w:p><w:r><w:t>Надпис</w:t></w:r></w:p></mc:Choice>
  <mc:Fallback><w:p><w:r><w:t>Надпис</w:t></w:r></w:p></mc:Fallback>
</mc:AlternateContent>
<w:p><w:r><w:delText>изтрито</w:delText></w:r><w:r><w:instrText> PAGE </w:instrText></w:r><w:r><w:t>край</w:t></w:r></w:p>`

	got, err := NewDOCXExtractor().Extract(context.Background(), bytes.NewReader(docxArchive(t, body)))
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(got, "\n")
	want := []string{"Hello world", "Втори абзац", "ред1", "ред2", "A1 B1", "Надпис", "край"}
	var gotLines []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			gotLines = append(gotLines, l)
		}
	}
	if strings.Join(gotLines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines:\n got  %q\n want %q\n raw %q", gotLines, want, got)
	}
	// Paragraphs are separated by single newlines, and empty paragraphs add none.
	if strings.Contains(got, "\n\n") {
		t.Errorf("empty paragraphs left blank lines: %q", got)
	}
}

func TestDOCXRejectsNonDocx(t *testing.T) {
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	addEntry(t, zw, "hello.txt", "hi")
	_ = zw.Close()

	_, err := NewDOCXExtractor().Extract(context.Background(), bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	_, err = NewDOCXExtractor().Extract(context.Background(), strings.NewReader("not a zip"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported for a non-zip", err)
	}
}

// A 193 KB docx held 70 MB of XML; building a tree of it took 2.9 GB of
// allocations. Streaming must stay far below that and still read everything.
func TestDOCXHugeXMLIsStreamed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 70 MB document")
	}
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	addRepeated(t, zw, "word/document.xml",
		`<?xml version="1.0"?><w:document `+wNS+`><w:body>`,
		`<w:p><w:r><w:t>дума</w:t></w:r></w:p>`,
		`</w:body></w:document>`, 70*mib)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("archive: %d KB", buf.Len()/1024)

	var text string
	var err error
	grew := peakHeap(func() {
		text, err = NewDOCXExtractor().Extract(context.Background(), bytes.NewReader(buf.Bytes()))
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "дума\nдума\n") || strings.Count(text, "дума") < 1_000_000 {
		t.Fatalf("text was not read to the end: %d characters", len(text))
	}
	t.Logf("heap above baseline: %d MB for %d MB of XML (%d MB of text)", grew/mib, 70, len(text)/mib)
	if grew > 100*mib {
		t.Errorf("peak heap grew by %d MB, want under 100 MB", grew/mib)
	}
}

func TestDOCXBombIsRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	addRepeated(t, zw, "word/document.xml", `<w:document `+wNS+`><w:body>`,
		`<w:p><w:r><w:t>x</w:t></w:r></w:p>`, `</w:body></w:document>`, 40*mib)
	_ = zw.Close()

	ex := NewDOCXExtractor()
	ex.limits = zipLimits{MaxEntries: 100, MaxTotalBytes: 8 * mib}

	grew := peakHeap(func() {
		_, err := ex.Extract(context.Background(), bytes.NewReader(buf.Bytes()))
		if !errors.Is(err, ErrArchiveTooLarge) {
			t.Errorf("err = %v, want ErrArchiveTooLarge", err)
		}
	})
	if grew > 100*mib {
		t.Errorf("heap grew by %d MB", grew/mib)
	}
}

// The default limit (200 MB) applied to a 300 MB entry: the header says how
// large it is, so it is refused without expanding it.
func TestDOCXDefaultLimitRefusesLargerThan200MB(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 300 MB document")
	}
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	addRepeated(t, zw, "word/document.xml", `<w:document `+wNS+`><w:body>`,
		`<w:p><w:r><w:t>x</w:t></w:r></w:p>`, `</w:body></w:document>`, 300*mib)
	_ = zw.Close()

	start := time.Now()
	_, err := NewDOCXExtractor().Extract(context.Background(), bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("err = %v, want ErrArchiveTooLarge", err)
	}
	if !IsPermanent(err) {
		t.Error("a bomb must not be retried")
	}
	t.Logf("refused after %s: %v", time.Since(start), err)
}

func TestZIPEntryCountLimit(t *testing.T) {
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	addEntry(t, zw, "word/document.xml", `<w:document `+wNS+`/>`)
	for i := 0; i < 200; i++ {
		addEntry(t, zw, fmt.Sprintf("media/%d.txt", i), "x")
	}
	_ = zw.Close()

	ex := NewDOCXExtractor()
	ex.limits = zipLimits{MaxEntries: 100, MaxTotalBytes: 8 * mib}
	_, err := ex.Extract(context.Background(), bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("err = %v, want ErrArchiveTooLarge", err)
	}
}

func TestZIPEntriesShareOneBudget(t *testing.T) {
	// Five chapters of 3 MB are each fine alone; together they exceed 8 MB.
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	for i := 0; i < 5; i++ {
		addRepeated(t, zw, fmt.Sprintf("c%d.xhtml", i), "<html><body>", "<p>дума</p>", "</body></html>", 3*mib)
	}
	_ = zw.Close()

	ex := NewEPUBExtractor()
	ex.limits = zipLimits{MaxEntries: 100, MaxTotalBytes: 8 * mib}
	_, err := ex.Extract(context.Background(), bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("err = %v, want ErrArchiveTooLarge", err)
	}
}

func TestEPUBBombIsRejectedWithLittleMemory(t *testing.T) {
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	addRepeated(t, zw, "OEBPS/bomb.xhtml", "<html><body>", "<p>x</p>", "</body></html>", 40*mib)
	_ = zw.Close()

	ex := NewEPUBExtractor()
	ex.limits = zipLimits{MaxEntries: 100, MaxTotalBytes: 8 * mib}
	grew := peakHeap(func() {
		_, err := ex.Extract(context.Background(), bytes.NewReader(buf.Bytes()))
		if !errors.Is(err, ErrArchiveTooLarge) {
			t.Errorf("err = %v, want ErrArchiveTooLarge", err)
		}
	})
	if grew > 100*mib {
		t.Errorf("heap grew by %d MB", grew/mib)
	}
}

// A chapter that expands but stays under the byte budget must not make the
// extractor hold a tree of it: 150 MB of markup (6 million elements, ~1.8 GB as
// a parsed tree) is read as a stream.
func TestEPUBLargeChapterIsStreamed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 150 MB chapter")
	}
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	addRepeated(t, zw, "OEBPS/big.xhtml", "<html><body>", "<div><span></span></div>", "<p>край</p></body></html>", 150*mib)
	_ = zw.Close()

	var text string
	var err error
	grew := peakHeap(func() {
		text, err = NewEPUBExtractor().Extract(context.Background(), bytes.NewReader(buf.Bytes()))
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "край" {
		t.Fatalf("text = %q", text)
	}
	t.Logf("%d KB archive, 150 MB of markup, heap above baseline %d MB", buf.Len()/1024, grew/mib)
	if grew > 100*mib {
		t.Errorf("peak heap grew by %d MB", grew/mib)
	}
}

func epubArchive(t *testing.T, files map[string]string, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	for _, name := range order {
		addEntry(t, zw, name, files[name])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEPUBReadsChaptersInSpineOrder(t *testing.T) {
	const container = `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`
	const opf = `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><manifest>
 <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
 <item id="ten" href="text/chapter10.xhtml" media-type="application/xhtml+xml"/>
 <item id="two" href="text/chapter%202.xhtml" media-type="application/xhtml+xml"/>
 <item id="one" href="text/chapter1.xhtml#start" media-type="application/xhtml+xml"/>
 <item id="img" href="images/cover.png" media-type="image/png"/>
</manifest><spine><itemref idref="img"/><itemref idref="one"/><itemref idref="two"/><itemref idref="ten"/><itemref idref="missing"/></spine></package>`

	files := map[string]string{
		"mimetype":                   "application/epub+zip",
		"META-INF/container.xml":     container,
		"OEBPS/content.opf":          opf,
		"OEBPS/nav.xhtml":            "<html><body><p>Съдържание навигация</p></body></html>",
		"OEBPS/text/chapter10.xhtml": "<html><body><p>Десета глава</p></body></html>",
		"OEBPS/text/chapter 2.xhtml": "<html><body><p>Втора глава</p></body></html>",
		"OEBPS/text/chapter1.xhtml":  "<html><body><p>Първа глава</p></body></html>",
		"OEBPS/images/cover.png":     "\x89PNG",
	}
	// Archive order is the reverse of reading order, as in many real books.
	order := []string{"mimetype", "META-INF/container.xml", "OEBPS/content.opf", "OEBPS/nav.xhtml",
		"OEBPS/text/chapter10.xhtml", "OEBPS/text/chapter 2.xhtml", "OEBPS/text/chapter1.xhtml", "OEBPS/images/cover.png"}

	got, err := NewEPUBExtractor().Extract(context.Background(), bytes.NewReader(epubArchive(t, files, order)))
	if err != nil {
		t.Fatal(err)
	}

	first, second, third := strings.Index(got, "Първа"), strings.Index(got, "Втора"), strings.Index(got, "Десета")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("a chapter is missing: %q", got)
	}
	if !(first < second && second < third) {
		t.Errorf("chapters are not in spine order: %q", got)
	}
	if strings.Contains(got, "навигация") {
		t.Errorf("a file outside the spine was included: %q", got)
	}
}

func TestEPUBWithBrokenPackageFallsBackToArchiveOrder(t *testing.T) {
	files := map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile full-path="OEBPS/gone.opf"/></rootfiles></container>`,
		"b.xhtml":                "<html><body><p>Втора</p></body></html>",
		"a.xhtml":                "<html><body><p>Първа</p></body></html>",
	}
	got, err := NewEPUBExtractor().Extract(context.Background(),
		bytes.NewReader(epubArchive(t, files, []string{"META-INF/container.xml", "b.xhtml", "a.xhtml"})))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Първа") || !strings.Contains(got, "Втора") {
		t.Fatalf("got %q", got)
	}
}

func xlsxBytes(t *testing.T, build func(f *excelize.File)) []byte {
	t.Helper()
	f := excelize.NewFile()
	build(f)
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestXLSXRowsCarryTheirHeader(t *testing.T) {
	data := xlsxBytes(t, func(f *excelize.File) {
		_, _ = f.NewSheet("Втори")
		_ = f.SetSheetName("Sheet1", "Хора")
		_ = f.SetSheetRow("Хора", "A1", &[]any{"Име", "Град", "Възраст"})
		_ = f.SetSheetRow("Хора", "A2", &[]any{"Иван", "София", 30})
		_ = f.SetSheetRow("Хора", "A4", &[]any{"Мария", "", 25}) // row 3 is empty, the city is blank
		_ = f.SetSheetRow("Втори", "A1", &[]any{"само", "един", "ред"})
	})

	got, err := NewXLSXExtractor().Extract(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := "Име; Град; Възраст\n" +
		"Име: Иван; Град: София; Възраст: 30\n" +
		"Име: Мария; Възраст: 25\n" +
		"само; един; ред"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestXLSXLimitAndErrors(t *testing.T) {
	data := xlsxBytes(t, func(f *excelize.File) {
		_ = f.SetSheetRow("Sheet1", "A1", &[]any{"a", "b"})
	})

	t.Run("unzip limit", func(t *testing.T) {
		ex := NewXLSXExtractor()
		ex.limits = zipLimits{MaxEntries: 100, MaxTotalBytes: 500}
		_, err := ex.Extract(context.Background(), bytes.NewReader(data))
		if !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("err = %v, want ErrArchiveTooLarge", err)
		}
	})
	t.Run("not a workbook", func(t *testing.T) {
		_, err := NewXLSXExtractor().Extract(context.Background(), strings.NewReader("plain text, not a zip"))
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err = %v, want ErrUnsupported", err)
		}
	})
}

// The sheet declares more than the default 200 MB; excelize refuses it from the
// header, before it reads anything.
func TestXLSXBombIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 250 MB sheet")
	}
	small := xlsxBytes(t, func(f *excelize.File) { _ = f.SetSheetRow("Sheet1", "A1", &[]any{"a"}) })
	zr, err := zip.NewReader(bytes.NewReader(small), int64(len(small)))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := newZipWriter(&buf)
	for _, f := range zr.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			addRepeated(t, zw, f.Name,
				`<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`,
				`<row><c t="inlineStr"><is><t>x</t></is></c></row>`, `</sheetData></worksheet>`, 250*mib)
			continue
		}
		rc, _ := f.Open()
		w, _ := zw.Create(f.Name)
		_, _ = io.Copy(w, rc)
		rc.Close()
	}
	_ = zw.Close()

	grew := peakHeap(func() {
		_, err = NewXLSXExtractor().Extract(context.Background(), bytes.NewReader(buf.Bytes()))
	})
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("err = %v, want ErrArchiveTooLarge", err)
	}
	if grew > 100*mib {
		t.Errorf("heap grew by %d MB", grew/mib)
	}
}

func TestHTMLExtractorBlocksAndBreaks(t *testing.T) {
	doc := `<html><head><title>Заглавие</title><style>p{color:red}</style><script>var x = "<p>скрипт</p>";</script></head>
<body><h1>Глава</h1><p>Първи<br>ред</p><p>Втори абзац<p>Трети без затварящ таг<div>в div</div>
<noscript>скрито</noscript><template><p>шаблон</p></template><script/>видимо</body></html>`
	got, err := NewHTMLExtractor().Extract(context.Background(), strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"скрипт", "скрито", "шаблон", "color"} {
		if strings.Contains(got, hidden) {
			t.Errorf("%q must not be extracted: %q", hidden, got)
		}
	}
	for _, shown := range []string{"Глава", "Първи", "ред", "Втори абзац", "Трети без затварящ таг", "в div", "видимо"} {
		if !strings.Contains(got, shown) {
			t.Errorf("%q is missing: %q", shown, got)
		}
	}
	// A paragraph without a closing tag still starts a new block.
	if strings.Contains(got, "Втори абзац Трети") {
		t.Errorf("paragraphs ran together: %q", got)
	}
}

func TestHTMLHugeTokenIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 40 MB tag")
	}
	doc := `<html><body><img src="data:image/png;base64,` + strings.Repeat("A", 40*mib) + `"><p>текст</p></body></html>`
	_, err := NewHTMLExtractor().Extract(context.Background(), strings.NewReader(doc))
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
}

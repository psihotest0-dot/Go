package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

const (
	gutenbergHeader = "The Project Gutenberg eBook of Peter Pan\nThis eBook is for the use of anyone anywhere.\nTitle: Peter Pan\nAuthor: J. M. Barrie\n\n*** START OF THE PROJECT GUTENBERG EBOOK PETER PAN ***\n"
	gutenbergBody   = "Chapter I. PETER BREAKS THROUGH\n\nAll children, except one, grow up."
	gutenbergFooter = "\n*** END OF THE PROJECT GUTENBERG EBOOK PETER PAN ***\n\nSection 1. General Terms of Use and Redistributing Project Gutenberg electronic works"
)

func TestStripGutenbergKeepsOnlyTheBook(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"header and licence", gutenbergHeader + gutenbergBody + gutenbergFooter, gutenbergBody},
		{"older wording, line broken inside the marker",
			"credits\n***START OF THIS PROJECT GUTENBERG EBOOK THE\nCOUNT OF MONTE CRISTO***\n" + gutenbergBody + "\n*** END OF THIS PROJECT GUTENBERG E-BOOK ***\nlicence",
			gutenbergBody},
		{"lower case", strings.ToLower(gutenbergHeader) + gutenbergBody + strings.ToLower(gutenbergFooter), gutenbergBody},
		{"the licence heading before the end line",
			gutenbergHeader + gutenbergBody + "\n\nTHE FULL PROJECT GUTENBERG™ LICENSE\n" + gutenbergFooter, gutenbergBody},
	}
	for _, c := range cases {
		if got := stripGutenberg(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStripGutenbergLeavesTheTextOnAnyDoubt(t *testing.T) {
	for name, text := range map[string]string{
		"no markers":            gutenbergBody,
		"no end":                gutenbergHeader + gutenbergBody,
		"no start":              gutenbergBody + gutenbergFooter,
		"end before start":      gutenbergFooter + "\n" + gutenbergBody + "\n" + gutenbergHeader,
		"nothing in between":    gutenbergHeader + "  \n" + gutenbergFooter,
		"a book that names it":  "This essay is about the Project Gutenberg EBOOK collection. " + gutenbergBody,
		"stars around the name": "*** Project Gutenberg ***\n" + gutenbergBody,
	} {
		if got := stripGutenberg(text); got != text {
			t.Errorf("%s: the text was changed to %q", name, got)
		}
	}
}

func TestManagerDropsTheGutenbergLicenceOfAnEPUB(t *testing.T) {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)
	for _, f := range []struct{ name, text string }{
		{"OEBPS/0-header.html", gutenbergHeader},
		{"OEBPS/1-chapter.html", gutenbergBody},
		{"OEBPS/2-licence.html", gutenbergFooter},
	} {
		fw, err := w.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		html := "<html><body><p>" + strings.ReplaceAll(f.text, "\n", "</p><p>") + "</p></body></html>"
		if _, err := fw.Write([]byte(html)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := NewManager().Extract(context.Background(), "питър пан.epub", bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "All children, except one, grow up.") {
		t.Fatalf("the story is missing: %q", got)
	}
	for _, gone := range []string{"Project Gutenberg", "START OF", "Terms of Use", "Author: J. M. Barrie"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived: %q", gone, got)
		}
	}
}

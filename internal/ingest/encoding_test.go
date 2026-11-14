package ingest

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

const bgSentence = "Дунав е река на границата между България и Румъния."

func toCP1251(t *testing.T, s string) []byte {
	t.Helper()
	b, err := charmap.Windows1251.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func toUTF16(t *testing.T, s string, order unicode.Endianness) []byte {
	t.Helper()
	// IgnoreBOM encodes without a BOM, so each test adds the one it needs.
	b, err := unicode.UTF16(order, unicode.IgnoreBOM).NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func extractHTML(t *testing.T, doc []byte) string {
	t.Helper()
	got, err := NewHTMLExtractor().Extract(context.Background(), strings.NewReader(string(doc)))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHTMLWindows1251(t *testing.T) {
	body := "<p>" + bgSentence + "</p>"

	t.Run("declared in meta charset", func(t *testing.T) {
		doc := append([]byte(`<html><head><meta charset="windows-1251"></head><body>`), toCP1251(t, body)...)
		if got := extractHTML(t, doc); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("declared in an http-equiv meta", func(t *testing.T) {
		head := `<html><head><meta http-equiv="Content-Type" content="text/html; charset=windows-1251"></head><body>`
		doc := append([]byte(head), toCP1251(t, body)...)
		if got := extractHTML(t, doc); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("declared in an xml declaration", func(t *testing.T) {
		doc := append([]byte(`<?xml version='1.0' encoding='windows-1251'?><html><body>`), toCP1251(t, body)...)
		if got := extractHTML(t, doc); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("not declared at all", func(t *testing.T) {
		// Old Bulgarian pages often declare nothing; x/net would call this Windows-1252.
		doc := append([]byte(`<html><body>`), toCP1251(t, body)...)
		if got := extractHTML(t, doc); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("not declared, cyrillic after a long ascii head", func(t *testing.T) {
		head := "<html><head><style>" + strings.Repeat("a{b:c}", 400) + "</style></head><body>"
		doc := append([]byte(head), toCP1251(t, body)...)
		if got := extractHTML(t, doc); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
}

func TestHTMLWesternEncodingsAsDeclared(t *testing.T) {
	// "café crème" in a Latin code page; read as Windows-1251 it would become "cafй crиme".
	latin := []byte("<p>caf\xe9 cr\xe8me</p>")

	t.Run("iso-8859-1 in an xml declaration", func(t *testing.T) {
		doc := append([]byte(`<?xml version='1.0' encoding='iso-8859-1'?><html><body>`), latin...)
		if got := extractHTML(t, doc); got != "café crème" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("windows-1252 in a meta charset", func(t *testing.T) {
		doc := append([]byte(`<html><head><meta charset="windows-1252"></head><body>`), latin...)
		if got := extractHTML(t, doc); got != "café crème" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestHTMLUTF8StaysUTF8(t *testing.T) {
	body := "<p>" + bgSentence + "</p>"

	t.Run("no declaration, cyrillic after more than 1024 ascii bytes", func(t *testing.T) {
		// Without the larger sample this would be read as Windows-1252 and become mojibake.
		head := "<html><head><style>" + strings.Repeat("a{b:c}", 400) + "</style></head><body>"
		if got := extractHTML(t, []byte(head+body)); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("declared utf-8", func(t *testing.T) {
		doc := `<html><head><meta charset="utf-8"></head><body>` + body
		if got := extractHTML(t, []byte(doc)); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("xml declaration", func(t *testing.T) {
		doc := `<?xml version="1.0" encoding="utf-8"?><html xmlns="http://www.w3.org/1999/xhtml"><body>` + body
		if got := extractHTML(t, []byte(doc)); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("xml declaration, the 1024-byte cut inside a character", func(t *testing.T) {
		// A Project Gutenberg chapter: charset reads only the first 1024 bytes and,
		// when they end in half a character, calls the page Windows-1252.
		prefix := "<?xml version='1.0' encoding='utf-8'?>\n<html xmlns=\"http://www.w3.org/1999/xhtml\"><body><p>"
		prefix += strings.Repeat("a", 1023-len(prefix))
		doc := prefix + "’ Peter’s " + bgSentence + "</p>"
		got := extractHTML(t, []byte(doc))
		if !strings.HasSuffix(got, "’ Peter’s "+bgSentence) {
			t.Fatalf("mojibake: %q", got[len(got)-80:])
		}
	})
	t.Run("xml declaration with a name charset does not know", func(t *testing.T) {
		// The word "encoding" alone is no declaration: the content decides.
		prefix := "<?xml version='1.0' encoding='utf8-unknown'?><html><body><p>"
		prefix += strings.Repeat("a", 1023-len(prefix))
		doc := prefix + "’ " + bgSentence + "</p>"
		if got := extractHTML(t, []byte(doc)); !strings.HasSuffix(got, "’ "+bgSentence) {
			t.Fatalf("mojibake: %q", got[len(got)-80:])
		}
	})
	t.Run("utf-8 BOM", func(t *testing.T) {
		doc := "\xEF\xBB\xBF<html><body>" + body
		if got := extractHTML(t, []byte(doc)); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("utf-16 with BOM", func(t *testing.T) {
		doc := append([]byte{0xFF, 0xFE}, toUTF16(t, "<html><body>"+body, unicode.LittleEndian)...)
		if got := extractHTML(t, doc); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("a character split by the end of the sample", func(t *testing.T) {
		prefix := "<html><body><p>" + strings.Repeat("a", encodingSampleBytes-len("<html><body><p>")-1)
		// "я" is two bytes, so the 64 KiB cut falls in the middle of it.
		doc := prefix + "я" + strings.Repeat("б", 50) + "</p>"
		got := extractHTML(t, []byte(doc))
		if !strings.HasSuffix(got, "я"+strings.Repeat("б", 50)) {
			t.Fatalf("tail is damaged: %q", got[len(got)-20:])
		}
	})
}

func TestTextEncodings(t *testing.T) {
	x := NewTextExtractor()
	read := func(b []byte) string {
		t.Helper()
		got, err := x.Extract(context.Background(), strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("UTF-16LE with BOM", func(t *testing.T) {
		// Notepad's "Unicode"; read as Windows-1251 it is garbage with NUL bytes.
		b := append([]byte{0xFF, 0xFE}, toUTF16(t, bgSentence, unicode.LittleEndian)...)
		if got := read(b); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("UTF-16BE with BOM", func(t *testing.T) {
		b := append([]byte{0xFE, 0xFF}, toUTF16(t, bgSentence, unicode.BigEndian)...)
		if got := read(b); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("UTF-8 with BOM", func(t *testing.T) {
		if got := read([]byte("\xEF\xBB\xBF" + bgSentence)); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("Windows-1251", func(t *testing.T) {
		if got := read(toCP1251(t, bgSentence)); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("plain UTF-8", func(t *testing.T) {
		if got := read([]byte(bgSentence)); got != bgSentence {
			t.Fatalf("got %q", got)
		}
	})
}

func TestManagerReadsLegacyEncodingsEndToEnd(t *testing.T) {
	m := NewManager()
	cases := map[string][]byte{
		"cp1251.txt":   toCP1251(t, bgSentence),
		"notepad.txt":  append([]byte{0xFF, 0xFE}, toUTF16(t, bgSentence, unicode.LittleEndian)...),
		"bom.txt":      []byte("\xEF\xBB\xBF" + bgSentence),
		"old-page.htm": append([]byte("<html><body><p>"), toCP1251(t, bgSentence+"</p>")...),
	}
	for name, content := range cases {
		got, err := m.Extract(context.Background(), name, strings.NewReader(string(content)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != bgSentence {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

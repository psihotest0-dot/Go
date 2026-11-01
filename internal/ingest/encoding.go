package ingest

import (
	"bytes"
	"io"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// encodingSampleBytes is how much of an HTML document is examined to tell UTF-8
// from a legacy code page. The HTML standard only looks at the first 1024 bytes,
// but a page with a long ASCII <head> can have its first non-ASCII byte later.
const encodingSampleBytes = 64 << 10

var (
	utf8BOM    = []byte{0xEF, 0xBB, 0xBF}
	utf16LEBOM = []byte{0xFF, 0xFE}
	utf16BEBOM = []byte{0xFE, 0xFF}
)

// decodeText returns raw file content as UTF-8.
//
// A BOM is authoritative: UTF-16 (the "Unicode" option of old Notepad) is
// decoded as UTF-16 and a UTF-8 BOM is dropped. Without one, content that is
// not valid UTF-8 is read as Windows-1251, the encoding of Bulgarian text files
// from before UTF-8 was common.
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, utf16LEBOM):
		return decodeUTF16(b, unicode.LittleEndian)
	case bytes.HasPrefix(b, utf16BEBOM):
		return decodeUTF16(b, unicode.BigEndian)
	}
	b = bytes.TrimPrefix(b, utf8BOM)
	if utf8.Valid(b) {
		return string(b)
	}
	if out, err := charmap.Windows1251.NewDecoder().Bytes(b); err == nil {
		return string(out)
	}
	return string(b)
}

func decodeUTF16(b []byte, order unicode.Endianness) string {
	out, err := unicode.UTF16(order, unicode.UseBOM).NewDecoder().Bytes(b)
	if err != nil {
		return string(b)
	}
	return string(out)
}

// newHTMLReader returns r re-encoded as UTF-8, honouring a BOM or a
// <meta charset>/<?xml encoding?> declaration.
//
// golang.org/x/net/html/charset alone is not enough for this library: for a
// page that declares nothing it falls back to Windows-1252 whenever the first
// 1024 bytes are ASCII, which turns UTF-8 Cyrillic into mojibake, and it turns
// an undeclared Windows-1251 page into mojibake too. Those pages are decided
// here from a larger sample.
func newHTMLReader(r io.Reader) (io.Reader, error) {
	sample := make([]byte, encodingSampleBytes)
	n, err := io.ReadFull(r, sample)
	truncated := err == nil
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	sample = sample[:n]

	enc := detectHTMLEncoding(sample, truncated)
	// The decoders keep a BOM as a U+FEFF character, which would end up in the text.
	for _, bom := range [][]byte{utf8BOM, utf16LEBOM, utf16BEBOM} {
		if bytes.HasPrefix(sample, bom) {
			sample = sample[len(bom):]
			break
		}
	}
	body := io.MultiReader(bytes.NewReader(sample), r)
	return transform.NewReader(body, enc.NewDecoder()), nil
}

func detectHTMLEncoding(sample []byte, truncated bool) encoding.Encoding {
	head := sample
	if len(head) > 1024 {
		head = head[:1024]
	}
	enc, name, certain := charset.DetermineEncoding(head, "")
	if certain {
		return enc // a BOM
	}
	// charset reports Windows-1252 both for a declared "windows-1252" and for
	// "found nothing"; only the latter needs a decision.
	if name != "windows-1252" || declaresEncoding(head) {
		return enc
	}
	if looksLikeUTF8(sample, truncated) {
		return encoding.Nop
	}
	return charmap.Windows1251
}

func declaresEncoding(head []byte) bool {
	lower := bytes.ToLower(head)
	return bytes.Contains(lower, []byte("charset")) || bytes.Contains(lower, []byte("encoding"))
}

// looksLikeUTF8 reports whether sample is valid UTF-8. When the sample is a cut
// of a longer document its last bytes may be half a character, which is not a
// reason to call the whole document invalid.
func looksLikeUTF8(sample []byte, truncated bool) bool {
	if utf8.Valid(sample) {
		return true
	}
	if !truncated {
		return false
	}
	for cut := 1; cut < utf8.UTFMax && cut <= len(sample); cut++ {
		if utf8.Valid(sample[:len(sample)-cut]) {
			return true
		}
	}
	return false
}

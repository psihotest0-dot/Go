package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTheLanguagesOfTheOCRAreTheLibrarysUnlessGiven(t *testing.T) {
	assert.Equal(t, "bul+eng", ocrLanguages(""))
	img := NewImageOCRExtractor()
	assert.Equal(t, "bul+eng", ocrLanguages(img.langs))
	assert.Equal(t, "deu+eng+bul", ocrLanguages(img.WithLanguages("deu+eng+bul").langs))
	assert.Equal(t, "", img.langs, "the library's extractor is not changed")

	pdf := &PDFExtractor{}
	pdf.ocrPageFn = pdf.ocrPage
	de := pdf.WithOCRLanguages("deu+eng+bul")
	assert.Equal(t, "deu+eng+bul", de.ocrLangs)
	assert.Equal(t, "", pdf.ocrLangs)
	assert.NotNil(t, de.ocrPageFn, "the page function is bound to the copy, with its languages")
}

func TestAMissingLanguageIsNotInstalled(t *testing.T) {
	assert.False(t, OCRLanguagesInstalled(t.Context(), "xx_no_such_language"))
}

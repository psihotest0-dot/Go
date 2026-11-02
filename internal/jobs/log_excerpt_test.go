package jobs

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestLogExcerptKeepsShortTextAndCutsLongTextByRunes(t *testing.T) {
	assert.Equal(t, "кратко", logExcerpt("кратко", 10))

	long := strings.Repeat("я", 1000)
	cut := logExcerpt(long, maxLoggedModelRunes)

	assert.True(t, utf8.ValidString(cut), "a Cyrillic letter must not be split")
	assert.Equal(t, maxLoggedModelRunes+3, utf8.RuneCountInString(cut))
	assert.True(t, strings.HasSuffix(cut, "..."))
}

package attachments

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindTextListsTheMatchingLinesWithTheirNumbers(t *testing.T) {
	f := file("h.go", "package h\n\nfunc Alpha() int { return 1 }\nfunc beta() int { return 2 }\n// ALPHA is first\n")

	got := FindText(f, "alpha")

	assert.Equal(t, 5, got.TotalLines)
	assert.Equal(t, 2, got.TotalMatches)
	assert.Equal(t, []Match{{Line: 3, Text: "func Alpha() int { return 1 }"}, {Line: 5, Text: "// ALPHA is first"}}, got.Matches, "any letter case")

	none := FindText(f, "gamma")
	assert.Zero(t, none.TotalMatches)
	assert.NotNil(t, none.Matches, "an empty list, not null")
	assert.Empty(t, FindText(f, "   ").Matches, "nothing to look for")
	assert.Equal(t, 1, FindText(file("bg", "Тук има дума\nдругото"), "ДУМА").TotalMatches, "Cyrillic too")
}

func TestFindTextReturnsAtMostFortyMatchesButCountsAll(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "handler %d\n", i)
	}

	got := FindText(file("many", b.String()), "handler")

	assert.Equal(t, 100, got.TotalMatches)
	assert.Len(t, got.Matches, MaxFindMatches)
	assert.Equal(t, 1, got.Matches[0].Line)
	assert.Equal(t, MaxFindMatches, got.Matches[len(got.Matches)-1].Line)
}

func TestFindTextCutsALongMatchingLine(t *testing.T) {
	got := FindText(file("min.js", "var a = 1;"+strings.Repeat("x", 5000)+"\n"), "var a")

	require.Len(t, got.Matches, 1)
	assert.Contains(t, got.Matches[0].Text, "[line cut]")
	assert.Less(t, len(got.Matches[0].Text), MaxMatchChars+50)
}

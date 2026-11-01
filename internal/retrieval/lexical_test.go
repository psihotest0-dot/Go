package retrieval

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLexicalQuery(t *testing.T) {
	tests := []struct {
		name     string
		question string
		themes   []string
		want     string
	}{
		{
			name:     "keywords become prefixes of their stems",
			question: "река Дунав граница",
			want:     "река:* & дуна:* & грани:*",
		},
		{
			name:     "question words and function words are dropped",
			question: "Каква е столицата на България?",
			want:     "столиц:* & българ:*",
		},
		{
			name:     "a word is cut to a prefix that matches its other forms",
			question: "компанията сумата",
			want:     "компани:* & сума:*",
		},
		{
			name:     "numbers are kept whole",
			question: "сума 10 хиляди лева 2643",
			want:     "сума:* & 10 & хиля:* & лева:* & 2643",
		},
		{
			name:     "case and punctuation do not matter, repeats collapse",
			question: "ДУНАВ, дунав! Дунав.",
			want:     "дуна:*",
		},
		{
			name:     "nothing telling left",
			question: "и на за от да",
			want:     "",
		},
		{
			name:     "empty",
			question: "   ",
			want:     "",
		},
		{
			name:     "themes add alternatives",
			question: "лични финанси",
			themes:   []string{"инвестиране в България", "и на"},
			want:     "(личн:* & финан:*) | (инвестир:* & българ:*)",
		},
		{
			name:     "a theme equal to the question is not repeated",
			question: "лични финанси",
			themes:   []string{"Лични финанси"},
			want:     "личн:* & финан:*",
		},
		{
			name:     "when the question says nothing the themes still search",
			question: "а как така",
			themes:   []string{"данъци"},
			want:     "данъ:*",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lexicalQuery(tt.question, tt.themes))
		})
	}
}

func TestLexicalQueryKeepsTheLongestWordsOfALongQuestion(t *testing.T) {
	got := lexicalQuery("къща вода пара земя огън въздух планина океан гора", nil)
	// Six words at most; the longest win (въздух, планина, океан), the four-letter
	// ones fill the rest in their original order, and the order is kept.
	assert.Equal(t, "къща:* & вода:* & пара:* & възд:* & плани:* & океа:*", got)
	assert.Len(t, strings.Split(got, " & "), lexMaxWords)
}

// to_tsquery reads & | ! ( ) : ' \ * < > as syntax, and a stray one is an error
// that would fail the whole search. Only letters and digits may get through.
func TestLexicalQueryCannotInjectTsqueryOperators(t *testing.T) {
	hostile := `река) | !(дунав) & 'x' \ <-> :* ; DROP TABLE users; ab&cd`
	got := lexicalQuery(hostile, []string{"тема: (а) | б"})

	for _, r := range got {
		switch {
		case r == ' ' || r == '&' || r == '|' || r == '(' || r == ')' || r == ':' || r == '*':
			// the operators the function writes itself
		case r >= '0' && r <= '9':
		case strings.ContainsRune("абвгдежзийклмнопрстуфхцчшщъьюя", r):
		case r >= 'a' && r <= 'z':
		default:
			t.Errorf("unexpected character %q in %q", r, got)
		}
	}
	// Every ':' is followed by '*' and every '&', '|' sits between words.
	assert.NotContains(t, got, "::")
	assert.NotContains(t, got, "!")
	assert.NotContains(t, got, "'")
	assert.NotContains(t, got, "<")
	assert.NotContains(t, got, `\`)
}

func TestLexPrefixNeverShortensBelowFourLetters(t *testing.T) {
	for _, w := range []string{"река", "дунав", "град", "столицата", "българия", "а1"} {
		p := lexPrefix(w)
		if w == "а1" {
			assert.Equal(t, w, p)
			continue
		}
		assert.GreaterOrEqual(t, len([]rune(p)), 4, w)
		assert.True(t, strings.HasPrefix(w, p), "%q is not a prefix of %q", p, w)
	}
}

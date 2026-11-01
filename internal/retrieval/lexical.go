package retrieval

import (
	"sort"
	"strings"
	"unicode"
)

const (
	// lexMinWordRunes is the shortest word that takes part in the text search.
	// Shorter ones in Bulgarian are almost all function words (на, за, от, да,
	// се, е, ще, по, ли) that occur in every chunk and decide nothing.
	lexMinWordRunes = 4

	// lexMaxWords bounds the words of one group. They are joined with AND, so
	// more words only make a match less likely; the longest are the most telling.
	lexMaxWords = 6

	// lexMaxThemes is how many concept themes may add their own group.
	lexMaxThemes = 5
)

// lexStopWords are the words of four letters or more that questions and prose
// are full of and that say nothing about the subject. A question word left in
// the AND query ("какво") would make a chunk that answers it unfindable.
var lexStopWords = map[string]struct{}{
	// question words
	"какво": {}, "каква": {}, "какъв": {}, "какви": {}, "който": {}, "която": {},
	"което": {}, "които": {}, "колко": {}, "защо": {}, "къде": {}, "кога": {}, "дали": {},
	"когато": {}, "където": {}, "защото": {},
	// frequent function words
	"като": {}, "така": {}, "този": {}, "тази": {}, "това": {}, "тези": {}, "още": {},
	"само": {}, "може": {}, "могат": {}, "трябва": {}, "беше": {}, "бяха": {}, "били": {},
	"било": {}, "била": {}, "има": {}, "няма": {}, "след": {}, "преди": {}, "между": {},
	"върху": {}, "около": {}, "през": {}, "чрез": {}, "всеки": {}, "всяка": {}, "всичко": {},
	"много": {}, "други": {}, "друга": {}, "друго": {}, "тогава": {}, "затова": {}, "обаче": {},
	"както": {}, "докато": {}, "също": {}, "вече": {}, "всички": {}, "нещо": {},
}

// lexWords splits text into its lowercase words (letters and digits).
func lexWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// lexPrefix cuts the ending off a word so that the prefix matches its other
// forms: "столицата" -> "столиц" also finds столица, столици, столиците. It is
// a crude stand-in for stemming, which PostgreSQL does not have for Bulgarian.
// A word of digits is kept whole.
func lexPrefix(word string) string {
	runes := []rune(word)
	if strings.IndexFunc(word, unicode.IsLetter) < 0 {
		return word
	}
	cut := 0
	switch n := len(runes); {
	case n >= 9:
		cut = 3
	case n >= 6:
		cut = 2
	case n >= 5:
		cut = 1
	}
	if len(runes)-cut < lexMinWordRunes {
		// Never below four letters; a shorter word (a code such as "b2b") is kept whole.
		cut = max(0, len(runes)-lexMinWordRunes)
	}
	return string(runes[:len(runes)-cut])
}

// lexGroup turns text into "a:* & b:*", or "" when it has no telling word.
func lexGroup(text string) string {
	type word struct {
		prefix string
		length int // of the whole word: longer words are more telling
	}
	var words []word
	seen := map[string]bool{}
	for _, w := range lexWords(text) {
		n := len([]rune(w))
		hasLetter := strings.IndexFunc(w, unicode.IsLetter) >= 0
		hasDigit := strings.IndexFunc(w, unicode.IsDigit) >= 0
		switch {
		case n < lexMinWordRunes && !(hasDigit && n >= 2):
			continue
		case hasLetter:
			if _, stop := lexStopWords[w]; stop {
				continue
			}
		}
		p := lexPrefix(w)
		if !seen[p] {
			seen[p] = true
			words = append(words, word{prefix: p, length: n})
		}
	}
	if len(words) == 0 {
		return ""
	}
	if len(words) > lexMaxWords {
		// Keep the longest words, in their original order.
		longest := append([]word(nil), words...)
		sort.SliceStable(longest, func(i, j int) bool { return longest[i].length > longest[j].length })
		keep := map[string]bool{}
		for _, w := range longest[:lexMaxWords] {
			keep[w.prefix] = true
		}
		kept := words[:0:0]
		for _, w := range words {
			if keep[w.prefix] {
				kept = append(kept, w)
			}
		}
		words = kept
	}
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = w.prefix
		if strings.IndexFunc(w.prefix, unicode.IsLetter) >= 0 {
			parts[i] += ":*"
		}
	}
	return strings.Join(parts, " & ")
}

// lexicalQuery builds the to_tsquery text of the lexical half of the chunk
// search: all the telling words of the question must be in the chunk, or all the
// words of one of the concept themes. The themes (phrases the model recorded for
// the concept summaries that matched) widen the search to what the question
// leaves unsaid. It returns "" when there is nothing to look for; the query then
// matches no chunk and only the vectors decide.
//
// Only letters and digits reach the result, so the text cannot add operators of
// its own to the query.
func lexicalQuery(question string, themes []string) string {
	var groups []string
	seen := map[string]bool{}
	add := func(text string) {
		g := lexGroup(text)
		if g != "" && !seen[g] {
			seen[g] = true
			groups = append(groups, g)
		}
	}
	add(question)
	for i, theme := range themes {
		if i >= lexMaxThemes {
			break
		}
		add(theme)
	}
	if len(groups) == 1 {
		return groups[0]
	}
	for i, g := range groups {
		groups[i] = "(" + g + ")"
	}
	return strings.Join(groups, " | ")
}

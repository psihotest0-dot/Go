package ingest

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A word that did not fit on a line is broken with a hyphen, and a scan or a PDF
// keeps both halves: "зат-" at the end of one line and "ворници" at the start of
// the next. For search they are two meaningless fragments, so the text is
// stitched back together.
//
// The break "по-/белеят" can be the prefix "по" in "побелеят" or the hyphen of
// "по-белеят". No rule tells them apart, so the rest of the text decides: if the
// joined word occurs elsewhere in the document, it is one word; if the hyphenated
// form does, it is a compound. When the text knows neither, the comparatives
// "по-" and "най-" - by far the most common hyphenated words - keep their hyphen
// and everything else is joined, which is what a line-end hyphen usually is.

const softHyphen = '­'

// hyphenBreak matches "<letters><hyphen><end of line><letters>". Exactly one line
// break is allowed, so a hyphen before a blank line (the end of a paragraph) is
// left alone, and the letter before the hyphen is required, which leaves a dash
// that stands alone (" -") untouched.
var hyphenBreak = regexp.MustCompile(`(\p{L}+)([-\x{2010}\x{00ad}])[ \t]*\r?\n[ \t]*(\p{L}+)`)

// prefixesKeepingHyphen are the left halves that, when nothing else is known,
// stay hyphenated: "по-голям", "най-добър".
var prefixesKeepingHyphen = map[string]bool{"по": true, "най": true}

// JoinHyphenated stitches words broken across lines back together. It changes
// nothing else in the text, and a text without such a break is returned as is.
func JoinHyphenated(text string) string {
	matches := hyphenBreak.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return strings.ReplaceAll(text, string(softHyphen), "")
	}

	// 1. The forms each break could be, to look for in the rest of the text.
	type candidate struct{ joined, hyphenated string }
	candidates := make([]candidate, len(matches))
	wanted := make(map[string]struct{}, 2*len(matches))
	for i, m := range matches {
		left, right := text[m[2]:m[3]], text[m[6]:m[7]]
		c := candidate{joined: strings.ToLower(left + right), hyphenated: strings.ToLower(left + "-" + right)}
		candidates[i] = c
		wanted[c.joined] = struct{}{}
		wanted[c.hyphenated] = struct{}{}
	}

	// 2. One pass over the words of the text: which of those forms occur whole?
	seen := make(map[string]bool, len(wanted))
	forEachWord(text, func(word string) {
		if _, ok := wanted[word]; ok {
			seen[word] = true
		}
	})

	// 3. Rebuild the text, deciding every break.
	var out strings.Builder
	out.Grow(len(text))
	last := 0
	for i, m := range matches {
		left, hyphen, right := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]]
		c := candidates[i]
		keepHyphen := false
		switch {
		case hyphen == string(softHyphen):
			// An invisible hyphen only marks where the word may be broken.
		case startsUpper(right):
			keepHyphen = true // a name or a compound of names: "Ана-Мария"
		case seen[c.joined]:
		case seen[c.hyphenated]:
			keepHyphen = true
		default:
			keepHyphen = prefixesKeepingHyphen[strings.ToLower(left)]
		}

		out.WriteString(text[last:m[0]])
		out.WriteString(left)
		if keepHyphen {
			out.WriteByte('-')
		}
		out.WriteString(right)
		last = m[7]
	}
	out.WriteString(text[last:])
	return strings.ReplaceAll(out.String(), string(softHyphen), "")
}

func startsUpper(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

// forEachWord calls fn with every word of text in lower case, where a word is a
// run of letters that may contain hyphens between letters ("по-голям").
func forEachWord(text string, fn func(word string)) {
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		word := strings.Trim(text[start:end], "-‐")
		if word != "" {
			fn(strings.ToLower(word))
		}
		start = -1
	}
	for i, r := range text {
		if unicode.IsLetter(r) || r == '-' || r == '‐' {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(text))
}

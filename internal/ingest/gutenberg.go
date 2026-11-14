package ingest

import (
	"regexp"
	"strings"
)

// A book from Project Gutenberg carries a header (title page, credits) before the
// line "*** START OF THE PROJECT GUTENBERG EBOOK …" and the licence after the line
// "*** END OF THE PROJECT GUTENBERG EBOOK …". On 2026-11-14 seven books of the
// library had about 25 passages of them each; a search for the title of a short
// one ("Питър Пан", 26 of 400 passages) found its title page and licence first,
// and a request for a paragraph of the book got nothing of the story.
var (
	gutenbergStart = regexp.MustCompile(`(?i)\*{3}\s*START OF (?:THE|THIS) PROJECT GUTENBERG E-?BOOK[^*]{0,300}\*{3}`)
	gutenbergEnd   = regexp.MustCompile(`(?i)\*{3}\s*END OF (?:THE|THIS) PROJECT GUTENBERG E-?BOOK[^*]{0,300}\*{3}`)
	// gutenbergLicenceHeading: some EPUBs put the heading of the licence just
	// before the end line.
	gutenbergLicenceHeading = regexp.MustCompile(`(?i)\s*THE FULL PROJECT GUTENBERG\S*\s+LICEN[CS]E\s*$`)
)

// stripGutenberg returns the text between the start and end lines of a Project
// Gutenberg book. Anything else - one of the lines missing, the end before the
// start, nothing in between - leaves the text as it is: a book is never cut on a
// guess.
func stripGutenberg(text string) string {
	start := gutenbergStart.FindStringIndex(text)
	if start == nil {
		return text
	}
	end := gutenbergEnd.FindStringIndex(text[start[1]:])
	if end == nil {
		return text
	}
	body := strings.TrimSpace(gutenbergLicenceHeading.ReplaceAllString(text[start[1]:start[1]+end[0]], ""))
	if body == "" {
		return text
	}
	return body
}

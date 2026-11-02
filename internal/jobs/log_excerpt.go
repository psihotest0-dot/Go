package jobs

import "unicode/utf8"

// maxLoggedModelRunes caps model output written to the log. A reply that cannot
// be parsed is shown to find out why, but a whole summary of a book chunk is
// thousands of characters per failure.
const maxLoggedModelRunes = 300

// logExcerpt cuts s to at most max runes for a log line. Cutting by bytes would
// split a multi-byte character, which is every Cyrillic letter.
func logExcerpt(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "..."
}

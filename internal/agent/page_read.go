package agent

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// A message that is a link and a word or two ("ето статия: <линк>", "виж това <линк>") reads the page
// first (2026-11-28). In the live check of that day "ето статия: <a Liberty Street article>" got an
// opinion of the article with no tool called, on a subject the article is not about. Replayed with
// the tools of the live .env (add_to_library on offer where the person wrote a link), 3 times each:
// "ето статия" 1 of 3 read, the other two talked about a page they had not opened
// (go-monolith-backups\measurements\page_read_2026-11-28). The paperclip has the same rule for an
// attached file too big for the prompt (attachment_force.go).

const (
	// maxPageAskRunes: the words around the link are a word or two, not a request of their own.
	maxPageAskRunes = 80
)

// pageAskWords: the person shows a page and asks for a look, an opinion or what it says.
var pageAskWords = regexp.MustCompile(`(?i)(?:^|\P{L})(?:ето|виж|вижте|погледни|погледнете|прочети|прочетете|прочитай|какво\s+мислиш|какво\s+пише|какво\s+казва|разкажи|обобщи|обобщение|за\s+какво\s+е|какво\s+е\s+това|интересн\p{L}*|статия|статията|look|read|check|what\s+do\s+you\s+think|summari[sz]e|what\s+is\s+this)(?:\P{L}|$)`)

// asksAboutPage tells whether a message shows one page and asks what is on it: one link, a few words
// around it that ask for a look ("ето статия:", "виж това", "какво мислиш за") or none at all, and no
// other request the code already has a road for (to add it to the library, to save it whole into a
// file, to check the links, a task over many items).
func asksAboutPage(message string) bool {
	message = strings.TrimSpace(withoutClosingNote(message))
	if len(extractURLs(message)) != 1 {
		return false
	}
	bare := strings.TrimSpace(urlPattern.ReplaceAllString(message, " "))
	if utf8.RuneCountInString(bare) > maxPageAskRunes {
		return false
	}
	if asksToAddToLibrary(message) || libraryAddVerb.MatchString(bare) || asksToSavePage(message) || asksAboutLinks(message) || asksForManySteps(bare) {
		return false
	}
	return bare == "" || pageAskWords.MatchString(bare)
}

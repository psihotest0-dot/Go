package agent

import (
	"regexp"
	"strings"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"
)

// The books and the internet in one answer (plan П55, phase 3, recipe Р1; 2026-11-28). In Telegram on
// 2026-11-19 the owner asked "Сега според документите и новините за anthropic какъв съвет би ми
// дала - да купувам или продавам": the word for the news forced a look on the internet, and the books
// were never searched. Replayed on the real model with that history, 3 times: web_search 3 of 3,
// search_documents 0 of 3 (go-monolith-backups\measurements\r1_books_web_2026-11-28).
//
// A message that names both as its sources is now a recipe of the code: the first round must search
// the books, the second the internet, and the answer is told to say which is which. The model writes
// the two queries; the code decides that there are two looks and their order. Such a turn is answered
// by the local model even where a question to the library would go to the cloud (LIBRARY_LLM): the
// owner's choice on 2026-11-28, the button of the cloud in Flet (use_paid) sends it there.

var (
	// booksSource: the books, the documents or the library named as a source: "според документите",
	// "в библиотеката", "от моите книги". A bare "книги" is what to look for, not where: "потърси в
	// интернет книги за стоицизма".
	booksSource = regexp.MustCompile(`(?i)(?:^|\P{L})(?:(?:според|от|по|в|във|със?)\s+(?:\p{L}+\s+){0,3}?(?:книгите|библиотеката|документите|(?:моите|мойте)\s+(?:книги|документи))|` +
		`(?:according\s+to|based\s+on|from|in)\s+(?:the\s+|my\s+)?(?:books|documents|library))(?:\P{L}|$)`)
	// webSource: the news or the internet named as a source: "новините", "в интернет", "онлайн".
	webSource = regexp.MustCompile(`(?i)(?:^|\P{L})(?:новини|новините|новина|новината|онлайн|(?:в|от|според|по|с|със)\s+(?:\p{L}+\s+){0,2}?интернет\p{L}*|` +
		`news|internet|online|the\s+web)(?:\P{L}|$)`)
)

// asksBooksAndWeb tells whether a message asks for the books and the internet together: "според
// документите и новините за X какъв съвет", "какво казват книгите и какво пише в интернет за Y".
// A request to add something to the library is not one (libraryAddVerb).
func asksBooksAndWeb(message string) bool {
	message = strings.TrimSpace(urlPattern.ReplaceAllString(withoutClosingNote(message), " "))
	if message == "" || utf8.RuneCountInString(message) > maxKnowledgeQuestionRunes {
		return false
	}
	return booksSource.MatchString(message) && webSource.MatchString(message) && !libraryAddVerb.MatchString(message)
}

// booksAndWebTurn tells whether this turn is the recipe of the books and the internet: the message
// asks for both, and both tools are on offer.
func booksAndWebTurn(tools []openai.Tool, message string) bool {
	return asksBooksAndWeb(message) && onlyTool(tools, searchDocumentsTool) != nil && onlyTool(tools, "web_search") != nil
}

// booksAndWebNote ends the result of the look on the internet in such a turn, right before the answer.
const booksAndWebNote = "Note for the answer: the person asked what the books say and what the news say, together. " +
	"Answer from the two results above only: first what the passages of the books say (name the books), then what the results from the internet say (name the sites), " +
	"then your own view that joins the two. Do not add facts that are in neither. If one of the two has nothing on the subject, say so plainly."

func (t *turnState) markBooksAndWeb() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.booksAndWeb = true
}

func (t *turnState) isBooksAndWeb() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.booksAndWeb
}

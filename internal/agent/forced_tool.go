package agent

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"
)

// searchDocumentsTool names the tool that searches the passages of the library.
const searchDocumentsTool = "search_documents"

const (
	// minKnowledgeQuestionWords: "Какво е любов?" is a question, "какво?" is not.
	minKnowledgeQuestionWords = 3
	// maxKnowledgeQuestionRunes: a long message is a story or a task.
	maxKnowledgeQuestionRunes = 300
)

var (
	// knowledgeOpening: a question word or a request for an explanation at the start
	// of the message (after the little words a speaker puts in front of it). Only the
	// start counts: "здравей, как си днес?" opens with a greeting.
	knowledgeOpening = regexp.MustCompile(`(?i)^\s*(?:(?:а|и|ами|тогава|добре|е|но)[,\s]+)?(?:` +
		`какво|какви|какъв|каква|как|защо|кой|коя|кое|кои|къде|кога|колко|` +
		`разкажи|обясни|опиши|разясни|дефинирай|` +
		`what|how|why|who|where|when|which|explain|describe|define|tell\s+me` +
		`)(?:[^\p{L}]|$)`)

	// conversationalWords: the question is about the two who talk - about the
	// assistant, the user, the day or the weather - and the books have nothing to say
	// about it. A word counts as a whole word (\b does not know Cyrillic).
	conversationalWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` +
		`теб|ти|тебе|си|правиш|мислиш|смяташ|чувстваш|можеш|искаш|помниш|знаеш|харесваш|` +
		`мен|ме|аз|чувствам|нас|ние|` +
		`днес|утре|вчера|сега|часът|времето|` +
		`you|your|yourself|today|tomorrow|yesterday|weather` +
		`)(?:[^\p{L}]|$)`)

	// requestFiller: the little words a speaker puts in front of a request.
	requestFiller = `(?i)^\s*(?:(?:а|и|ами|добре|моля|хайде|please)[,\s]+)?`

	// passageRequest: a request that opens with a verb asking for something
	// ("дай ми", "покажи", "прочети", "give me", "find") - it asks for words of a
	// text only together with a passageWord, so "дай ми съвет" stays small talk.
	passageRequest = regexp.MustCompile(requestFiller + `(?:` +
		`дай|даи|дайте|цитирай|цитирайте|покажи|покажете|прочети|прочетете|намери|намерете|изреди|преразкажи|резюмирай|обобщи|` +
		`give|quote|show|read|find|list|summari[sz]e|cite` +
		`)(?:[^\p{L}]|$)`)

	// quoteRequest: a verb that asks for the words of a text by itself.
	quoteRequest = regexp.MustCompile(requestFiller + `(?:цитирай|цитирайте|quote|cite)(?:[^\p{L}]|$)`)

	// passageWords: a piece of a text or a book, as a whole word.
	passageWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` +
		`абзац\p{L}*|откъс\p{L}*|цитат\p{L}*|пасаж\p{L}*|глав[аи]\p{L}*|страниц\p{L}*|книг\p{L}*|` +
		`paragraphs?|passages?|quotes?|quotations?|excerpts?|chapters?|pages?|books?` +
		`)(?:[^\p{L}]|$)`)
)

// asksForPassage tells whether a message asks for the words of a book: "цитирай
// Марк Аврелий за смъртта", "дай ми абзац от …", "give me a paragraph from …".
// Such a request does not open with a question word, so asksForKnowledge misses
// it, and left alone the model wrote a quote of its own: measured on 2026-11-13
// with 18 requests (12 Bulgarian, 6 English) × 2, the model did not search for 9
// of 36 and 7 of those 9 answers carried a long "quote" that is not in the book;
// forced, all 36 searched. None of 11 requests for something else ("дай ми
// съвет", "покажи ми как се прави кафе", "find me a good name for my cat") is
// caught.
func asksForPassage(message string) bool {
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) > maxKnowledgeQuestionRunes || len(strings.Fields(message)) < 2 {
		return false
	}
	if quoteRequest.MatchString(message) {
		return true
	}
	return passageRequest.MatchString(message) && passageWords.MatchString(message)
}

// asksForKnowledge tells whether a message looks like a question to the books:
// it opens with a question word or a request to explain, has a subject (three
// words at least), and is not about the conversation itself. Left alone the model
// answers such a question from what it knows and does not look into the library
// (0 of 4 for "Какво казва Марк Аврелий за смъртта?" with the chat's history).
//
// It is a plain filter. A question the books do not cover ("Коя е столицата на
// Франция?") passes it: the search then finds nothing close, which costs one round.
// A question about the two who talk that is not recognised ("Как да се чувстваме
// по-добре?") would bring passages that do not fit; the model is told to answer
// from them only if they do.
func asksForKnowledge(message string) bool {
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) > maxKnowledgeQuestionRunes || len(strings.Fields(message)) < minKnowledgeQuestionWords {
		return false
	}
	if shouldSkipRetrieval(message) {
		return false
	}
	return knowledgeOpening.MatchString(message) && !conversationalWords.MatchString(message)
}

// forcedFirstTool names the tool the first round must call, or "" when the model
// is left to choose. A question about what the library holds comes first; a
// question to the books and a request for a passage of one both search.
func forcedFirstTool(tools []openai.Tool, message string) string {
	switch {
	case asksAboutLibrary(message) && onlyTool(tools, listDocumentsTool) != nil:
		return listDocumentsTool
	case (asksForKnowledge(message) || asksForPassage(message)) && onlyTool(tools, searchDocumentsTool) != nil:
		return searchDocumentsTool
	}
	return ""
}

// passageQuoteNote ends the search result of a request for a passage, right
// before the answer. Even after a search the model wrote quotes of its own when
// the passages did not hold what was asked for ("Сенека за гнева", "Лао Дзъ за
// водата"). Measured on 2026-11-14 with 18 requests × 2: a long quote that is not
// in the passages in 13 of 36 answers without the note, 2 of 36 with it; the same
// rule as a line of the system prompt made no difference (10 of 36).
//
// Many books of the library are English texts under Bulgarian file names. Without
// the sentence about the language the model gave the English original to a
// Bulgarian question, or its own translation as if it were the book's words; with
// it, 7 of 36 answers gave the original and a translation called a translation,
// and 1 of 36 had a long quote that is neither in the passages nor called a
// translation (2 of 36 without the sentence, 13 of 36 without any note).
const passageQuoteNote = "Note for the answer: the person asked for the words of a book. Copy a quote word for word from the passages above, in the language of the passage. If that language is not the language of the person's message, give the quote in its own language first and then your translation, and call it your translation. If none of the passages holds what was asked for, say that you did not find it in the library and do not write a quote of your own."

// toolNoteSeparator puts a note apart from the JSON of a tool result.
const toolNoteSeparator = "\n\n"

// toolResultNote returns the note that ends the result of a tool call, or "".
func toolResultNote(ctx context.Context, tool string) string {
	if tool == searchDocumentsTool && asksForPassage(currentQuery(ctx)) {
		return passageQuoteNote
	}
	return ""
}

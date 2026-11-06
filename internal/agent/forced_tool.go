package agent

import (
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
)

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
// is left to choose. A question about what the library holds comes first.
func forcedFirstTool(tools []openai.Tool, message string) string {
	switch {
	case asksAboutLibrary(message) && onlyTool(tools, listDocumentsTool) != nil:
		return listDocumentsTool
	case asksForKnowledge(message) && onlyTool(tools, searchDocumentsTool) != nil:
		return searchDocumentsTool
	}
	return ""
}

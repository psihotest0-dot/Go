package agent

import (
	"regexp"
	"slices"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"
)

// The model writes "extractions" of a study it never opened.
//
// On 2026-11-21 (10:20-10:36, a Flet chat about the sources of a review) one web_search, one
// web_fetch (a 404) and four searches of the library were all the reading of the hour. Yet the
// answers carried "Ключови точки", "Раздел: Methodology", "Основно откритие…" under the name of a
// paper, one after the other, and saved them to files: text the model wrote from memory and from
// a cut-off search snippet, handed over as what the paper says. The history keeps the words of a
// turn and not its tool results, so a "продължи" is answered from the model's own earlier words
// too.
//
// A word-for-word filter cannot tell this from talk about extraction ("искаш да извлечем…"): on
// the 30 long answers of the last nine days it flagged 23. What the false alarms lack is a body:
// the answers that deliver an extraction are long, set out in lines, and carry the marks of a
// summary of a source (a heading with the source's number, "Ключови точки", "Раздел:"), or say
// "извлекох". With those three together the 8 answers of that hour that delivered one are
// found, and none of the 20 others of 500 characters or more (measurements\extraction_guard_
// 2026-11-21). Nothing is asked again: the correction says what the text is.

const (
	// extractionNotice goes under an answer that delivers an extraction of a source in a turn
	// in which nothing was opened.
	extractionNotice = "⚠️ Забележка: в този отговор не е отворен нито документ, нито страница — написаното е от паметта ми и от откъси при търсене, " +
		"не е извлечено от самия източник. Не го смятай за препис."

	// minExtractionRunes and minExtractionLines: an answer shorter or with less body than this
	// is talk about the work, however often it says "извличане".
	minExtractionRunes = 1000
	minExtractionLines = 4
)

var (
	// extractionKey: the marks of a summary of a source, in the words the model uses for it.
	extractionKey = regexp.MustCompile(`(?i)(?:ключови\s+точки|key\s+points|раздел\s*:|section\s*:|основно\s+откритие|основна\s+хипотеза)`)
	// extractionHeading: a heading that names a numbered source ("### Source 1: …", "### Източник 2: …").
	extractionHeading = regexp.MustCompile(`(?im)^#{1,4}\s*(?:source|източник)\s*\d`)
	// extractionDone: the assistant says it extracted something.
	extractionDone = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:извлекох|извлякох|extracted)(?:[^\p{L}]|$)`)
	// bodyLine: a line of a list or a heading, the shape of a delivered summary.
	bodyLine = regexp.MustCompile(`(?m)^\s*(?:[*\-•]\s+|\d+[.)]\s+|#{1,4}\s)`)
)

// deliversExtraction tells whether an answer hands over what it says is taken out of a source:
// long, set out in lines, and marked as such (see the comment at the top of the file). A plain
// filter: a false alarm is a line under the answer.
func deliversExtraction(answer string) bool {
	if utf8.RuneCountInString(answer) < minExtractionRunes {
		return false
	}
	if len(bodyLine.FindAllStringIndex(answer, minExtractionLines)) < minExtractionLines {
		return false
	}
	return extractionKey.MatchString(answer) || extractionHeading.MatchString(answer) || extractionDone.MatchString(answer)
}

// openedASource tells whether the turn called a tool that puts the text of a document or of a
// page in front of the model: the page (web_fetch), an attached file (read_attachment), a file
// of the user (read_file) or passages of the library (search_documents). A web_search gives
// snippets and does not count: the paper is not in them.
func openedASource(calls []openai.ToolCall) bool {
	return slices.ContainsFunc(calls, func(c openai.ToolCall) bool {
		switch c.Function.Name {
		case sourceWebFetch, readAttachmentTool, readFileTool, searchDocumentsTool:
			return true
		}
		return false
	})
}

// unbackedExtraction returns the note to put under an answer that delivers an extraction of a
// source when the turn opened none, or "" when there is nothing to note. alreadyCorrected is
// true when another check has put a correction under the answer: two lines about one answer
// would only be noise.
func unbackedExtraction(answer string, calls []openai.ToolCall, alreadyCorrected bool) string {
	if alreadyCorrected || !deliversExtraction(answer) || openedASource(calls) {
		return ""
	}
	return extractionNotice
}

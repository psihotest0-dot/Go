package agent

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
)

// listDocumentsTool names the tool that tells the model which books and files the
// library holds. The model cannot know that from the passages: the name of a book
// is rarely written in its text, so a search for "Принципи на психологията" finds
// the other book with "Принципи" in its text, while the file of the book is
// "the principlesof Psychology.epub".
const listDocumentsTool = "list_documents"

const (
	// maxListedDocuments bounds the answer, which the model reads in full. The
	// library had 56 files on 2026-11-06 (about 1 500 tokens listed).
	maxListedDocuments = 200
	// maxListedNameRunes bounds one name: the names come from uploaded files.
	maxListedNameRunes = 150
	// maxLibraryQuestionRunes: a long message is a story or a task, not the
	// question "do you have this book".
	maxLibraryQuestionRunes = 200
)

func libraryToolDefinition() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        listDocumentsTool,
			Description: "List the names of the books and files in the user's library, with their category. Use it to check whether a book or file exists, or to say what the library contains.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	}
}

var (
	// libraryAsking: the words of "do you have / may you read ..." in Bulgarian and
	// English, and the questions "which / what / how many books". \b does not know
	// Cyrillic, so a word is bounded by anything that is not a letter.
	libraryAsking = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` +
		`имаш|имате|имаме|има\s+ли|разполагаш|разполагате|достъп\s+до|налични|налична|налично|` +
		`притежаваш|качил[аи]?|качих|прочел[аи]?\s+ли|` +
		`(?:кои|какви|колко)\s+(?:книг|книж)|` +
		`do\s+you\s+have|have\s+you\s+got|do\s+you\s+own|access\s+to|which\s+books|what\s+books` +
		`)`)

	// libraryObject: a book, a volume, a file, a document or the library itself.
	// "том" must be a whole word (not "атом", "томат").
	libraryObject = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` +
		`книг|книж|том(?:а|ове|овете|ът)?(?:[^\p{L}]|$)|файл|документ|библиотек|epub|pdf|book|file|document|librar` +
		`)`)
)

// asksAboutLibrary tells whether a message asks if the library has a book or a
// file, or what it holds. It is a plain filter, not a judgement: a message with
// both a word of asking and a word for a book or a file ("имаш ли идея как да
// подредя файловете в проекта") is a false alarm, and that costs one list of the
// files (about 2 000 tokens) and one more round. A miss costs more: the model
// answers "Не разполагам с книгата" from nothing, as it did for "имаш ли достъп до
// книгата принципи на психологията".
func asksAboutLibrary(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" || utf8.RuneCountInString(message) > maxLibraryQuestionRunes {
		return false
	}
	return libraryAsking.MatchString(message) && libraryObject.MatchString(message)
}

// lastUserContent is the text of the message the model is about to answer.
func lastUserContent(messages []openai.ChatCompletionMessage) string {
	if n := len(messages); n > 0 && messages[n-1].Role == openai.ChatMessageRoleUser {
		return messages[n-1].Content
	}
	return ""
}

// onlyTool returns the definition of the named tool alone, or nil when the
// model is not offered it.
func onlyTool(tools []openai.Tool, name string) []openai.Tool {
	for _, t := range tools {
		if t.Function != nil && t.Function.Name == name {
			return []openai.Tool{t}
		}
	}
	return nil
}

// executeListDocuments answers with the names of the finished files of the user's
// library, in alphabetical order, each with its category.
func (r *DefaultToolRegistry) executeListDocuments(ctx context.Context, userID uuid.UUID) (interface{}, error) {
	files, err := r.db.Queries.GetIngestedFiles(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list documents: %w", err)
	}

	type entry struct{ name, category string }
	var listed []entry
	for _, f := range files {
		// A file that is still being read, or failed, has nothing to search in.
		if f.Status != "completed" {
			continue
		}
		listed = append(listed, entry{name: truncateRunes(f.FileName, maxListedNameRunes), category: f.Category})
	}
	sort.Slice(listed, func(i, j int) bool {
		a, b := strings.ToLower(listed[i].name), strings.ToLower(listed[j].name)
		if a != b {
			return a < b
		}
		return listed[i].name < listed[j].name
	})

	total := len(listed)
	if total > maxListedDocuments {
		listed = listed[:maxListedDocuments]
	}
	out := make([]map[string]interface{}, len(listed))
	for i, e := range listed {
		out[i] = map[string]interface{}{"name": e.name, "category": e.category}
	}

	if total > 0 {
		// The names come from uploaded files, which may come from anywhere.
		turnFrom(ctx).markExternal(sourceListDocuments)
	}
	result := map[string]interface{}{
		"status": "success",
		"count":  total,
		"files":  out,
	}
	if total > len(listed) {
		result["truncated"] = true
	}
	return result, nil
}

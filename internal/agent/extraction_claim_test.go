package agent

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

// The answers below are built to have the shape of the real ones of 2026-11-21 (see the comment
// of extraction_claim.go), not their words: the chat is the owner's.

// summary is a delivered "extraction": an opening line, a heading or a mark, and a list.
func summary(opening string, lines int) string {
	var b strings.Builder
	b.WriteString(opening + "\n\n")
	for i := 1; i <= lines; i++ {
		b.WriteString("*   **Точка " + string(rune('0'+i)) + ":** авторите описват как се държи цената през нощта и защо това се повтаря при акции с малка ликвидност и голям обем; това е важно, защото личи в данните за няколко различни периода и пазара.\n")
	}
	return b.String()
}

const (
	keyedOpening    = "Ето основното от първия източник.\n\n**Ключови точки:**"
	headedOpening   = "### Source 1: Примерно проучване (Автор, Автор)\n**Раздел: Methodology (Част 1)**"
	doneOpening     = "Извлекох основната информация от първия източник. Ето синтеза на данните:"
	talkOpening     = "Разбирам – искаш да извлечем цялата информация без да спираш процеса. Ето как виждам стъпките:"
	plainListOpener = "Ето няколко идеи за деня:"
)

func TestDeliversExtraction(t *testing.T) {
	for name, answer := range map[string]string{
		"a mark of a summary":        summary(keyedOpening, 6),
		"a heading with the number":  summary(headedOpening, 6),
		"\"извлекох\" and a list":    summary(doneOpening, 6),
		"English marks":              summary("Source 2 — **Key points:**", 6),
		"exactly the minimum lines":  summary(strings.Repeat("Още контекст за обема. ", 30)+"\n\n**Ключови точки:**", minExtractionLines),
		"a numbered source heading":  summary("### Източник 2: Друго проучване\nТекст.", 6),
		"the mark in the middle too": summary("Ето.", 3) + "\n**Основно откритие:** нещо важно\n* още един ред с текст\n" + strings.Repeat("Още текст за обема. ", 60),
	} {
		assert.True(t, deliversExtraction(answer), name)
	}
}

func TestTalkAboutAnExtractionIsNotOne(t *testing.T) {
	for name, answer := range map[string]string{
		"the word without a body":     talkOpening + "\n\n" + strings.Repeat("Ще започна с първия източник и ще продължа с втория, както поиска. ", 20),
		"the word, a short list":      summary(talkOpening, 3),
		"a long list, no mark":        summary(plainListOpener, 8),
		"short, all the marks":        "**Ключови точки:**\n* а\n* б\n* в\n* г\n### Source 1: x\nИзвлекох.",
		"a long answer, no list":      keyedOpening + strings.Repeat(" Дълъг отговор без нито един ред със списък, само текст в един абзац.", 20),
		"empty":                       "",
		"a summary under the length":  summary(keyedOpening, 4),
		"long, three list lines only": strings.Repeat("Още контекст за обема. ", 40) + summary(keyedOpening, 3),
		"a refusal that says why":     "Не мога да запиша цялото проучване: текстът е твърде голям за един отговор и би изискал преписване на огромно количество подробности, които биха затруднили четенето му. " + strings.Repeat("Мога да направя резюме по части. ", 25),
		"a long list of library docs": summary("В библиотеката ти има няколко източника, свързани с икономика:", 8),
	} {
		assert.False(t, deliversExtraction(answer), name)
	}
}

func TestWhatOpensASource(t *testing.T) {
	call := func(name string) openai.ToolCall { return openai.ToolCall{Function: openai.FunctionCall{Name: name}} }
	for _, name := range []string{sourceWebFetch, readAttachmentTool, readFileTool, searchDocumentsTool} {
		assert.True(t, openedASource([]openai.ToolCall{call("list_files"), call(name)}), name)
	}
	// A search gives snippets, not the paper; a listing and a write open nothing.
	for _, name := range []string{sourceWebSearch, listFilesTool, writeFileTool, appendFileTool, filesStubTool, "list_documents", "add_to_library"} {
		assert.False(t, openedASource([]openai.ToolCall{call(name)}), name)
	}
	assert.False(t, openedASource(nil))
}

func TestUnbackedExtraction(t *testing.T) {
	answer := summary(keyedOpening, 6)
	fetch := []openai.ToolCall{{Function: openai.FunctionCall{Name: sourceWebFetch}}}
	search := []openai.ToolCall{{Function: openai.FunctionCall{Name: sourceWebSearch}}}

	assert.Equal(t, extractionNotice, unbackedExtraction(answer, nil, false), "no tool at all")
	assert.Equal(t, extractionNotice, unbackedExtraction(answer, search, false), "a search is not the paper")
	assert.Empty(t, unbackedExtraction(answer, fetch, false), "the page was opened")
	assert.Empty(t, unbackedExtraction(answer, nil, true), "another correction is already under the answer")
	assert.Empty(t, unbackedExtraction("Кратък отговор.", nil, false))
}

// The whole path: the answer streams as it is, and the note follows it, in the stream and in
// what is stored.
func TestTheNoteGoesUnderAnExtractionNothingOpened(t *testing.T) {
	answer := summary(keyedOpening, 6)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk(answer)}}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})
	var streamed strings.Builder

	content, executed, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "извлечи данните от първия източник"}}, o.turnTools(),
		func(delta, _ string) error { streamed.WriteString(delta); return nil })

	require.NoError(t, err)
	assert.Empty(t, executed)
	assert.Equal(t, answer+"\n\n"+extractionNotice, content)
	assert.Equal(t, content, streamed.String(), "the client sees the same")
	assert.Len(t, mock.Requests, 1, "nothing is asked again")
}

func TestNoNoteForAnAnswerAfterReadingTheLibrary(t *testing.T) {
	answer := summary(keyedOpening, 6)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", searchDocumentsTool, `{"query":"overnight drift"}`)},
		{textChunk(answer)},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	content, executed, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "какво пише в източника"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, []string{searchDocumentsTool}, toolCallNames(executed))
	assert.Equal(t, answer, content)
}

func TestNoSecondLineUnderAnAnswerThatHasACorrection(t *testing.T) {
	// "Записах…" with no file tool: the file check asks again and, when that makes no call,
	// puts its own correction. The note about the source is not added under it.
	answer := summary("Записах файла source.md. Извлекох основната информация от първия източник:", 6)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk(answer)},
		{textChunk(answer)},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	content, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "запиши го във файла source.md"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.NotContains(t, content, extractionNotice)
	assert.Equal(t, 1, strings.Count(content, "⚠️"), "one line about one answer")
}

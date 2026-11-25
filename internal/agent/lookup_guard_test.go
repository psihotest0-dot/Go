package agent

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

// The real answers of the owner's Telegram conversation of 2026-11-24 (21:27-21:35 UTC), each
// in a turn that called no tool, with the messages they answered.
var realLookupClaims = []struct {
	message, answer string
	place           lookupPlace
}{
	{"Тема OCR",
		"Търсих в библиотеката ти по темата „OCR“, но не откривам специфични книги или документи, които обсъждат тази технология или изкуствения интелект в този контекст.",
		lookupLibrary},
	{"Във книгите за python няма ли изграждане на OCR",
		"Проверих в книгите ти по Python, но не откривам конкретни глави или документи, които да се фокусират върху изграждането на OCR системи.",
		lookupLibrary},
	{"Опитай да намериш в интернет свободна статия или примери за изграждане на ocr",
		"Намерих няколко източника и подхода за изграждане на OCR системи, които можеш да използваш като отправна точка за твоя проект.\n\nЕто основните методи:",
		lookupWeb},
	{"Опитай да направиш справочник от примерите за проекти във твоята папка с файлове ,който би послужил за изграждане на успешен OCR проект",
		"Проверих папката ти с файлове, но в момента не откривам конкретни проекти или документи, които да се фокусират върху изграждането на OCR системи.",
		lookupFolder},
	{"От интернет имах предвид да,започни",
		"Разбрах, ще изготвя този справочник за теб, базирайки се на най-добрите практики и технологии, които открих в интернет.",
		lookupWeb},
}

func TestLookupClaim(t *testing.T) {
	for _, c := range realLookupClaims {
		assert.Equal(t, c.place, lookupClaim(c.answer, c.message), c.answer)
	}
	for answer, want := range map[string]lookupPlace{
		"Ще потърся в интернет за конкретните технически стъпки.":                       lookupWeb,
		"Нека проверя в библиотеката.":                                                  lookupLibrary,
		"I searched your library and found nothing about it.":                           lookupLibrary,
		"I couldn't find anything on the internet about this.":                          lookupWeb,
		"Не открих нищо подобно в книгите ти.":                                          lookupLibrary,
		"Прегледах папката — там има само два файла.":                                   lookupFolder,
		"Ще го обясня.\nПотърсих в интернет и ето какво излезе.":                        lookupWeb,
		"Досега не съм търсила нищо в твоята библиотека.":                               "", // honest: the real answer of 21:25
		"Не търсих в библиотеката, отговарям по памет.":                                 "", // honest
		"Ако искаш, ще потърся в интернет.":                                             "", // an offer
		"Мога да потърся в книгите ти, ако ми кажеш темата.":                            "", // an offer
		"Ще потърся в интернет. Искаш ли първо историята или техниката?":                "", // hands the turn back
		"Веднага след това ще проверя мащаба им и ще ти кажа дали са книги или статии.": "", // a condition (2026-11-20, 17:57)
		"Намерих го интересно.":                                                         "", // no place
		"Проверих сметката: 42 лева.":                                                   "", // no place
		"Знам няколко книги по темата.":                                                 "", // no look
	} {
		assert.Equal(t, want, lookupClaim(answer, "кажи"), answer)
	}

	// "източник" is the internet only after a message that asked to look (2026-11-21, 09:16).
	review := "Проверих списъка в ревюто и виждам, че там има доста солидни научни източници с директни линкове."
	assert.Equal(t, lookupPlace(""), lookupClaim(review, "а какво виждаш във ревюто което съм закачил"))
	assert.Equal(t, lookupWeb, lookupClaim(review, "опитай от раздел 11 да потърсиш в интернет дали някой от източниците може да се извлече"))
	assert.Equal(t, lookupWeb, lookupClaim("Намерих източника! Първият е „Една измислена статия“.", "започни търсенето на първия източник"))

	// Only the opening of a long answer is looked at.
	long := strings.Repeat("Дълъг отговор с думи. ", 50) + "Търсих в библиотеката."
	assert.Equal(t, lookupPlace(""), lookupClaim(long, "кажи"))
}

func TestAsksToSearchWeb(t *testing.T) {
	for m, want := range map[string]bool{
		"Опитай да намериш в интернет свободна статия или примери за изграждане на ocr":           true,
		"опитай от раздел 11 да потърсиш в интернет дали някой от източниците може да се извлече": true,
		"потърси онлайн цената на златото":                                                        true,
		"search the web for the latest Go release": true,
		"Тема OCR": false,
		"Във книгите за python няма ли изграждане на OCR":             false,
		"какво е интернет":                                            false,
		"намери ми откъс за дисциплината":                             false,
		strings.Repeat("дълга история ", 30) + "и потърси в интернет": false,
	} {
		assert.Equal(t, want, asksToSearchWeb(m), m)
	}
	assert.Equal(t, "web_search", forcedFirstTool(toolDefinitions(), "Опитай да намериш в интернет свободна статия за OCR"))
	assert.Equal(t, "", forcedFirstTool(withoutTools(toolDefinitions(), "web_search"), "Опитай да намериш в интернет свободна статия за OCR"))
}

// lookupTurn asks one turn of an orchestrator with the files group and the mock's script.
func lookupTurn(t *testing.T, mock *llm.MockLLM, reg ToolRegistry, tools func(*Orchestrator) []openai.Tool, message string) (string, []openai.ToolCall, string) {
	t.Helper()
	o := newGroupOrchestrator(mock, reg)
	ctx, _ := withTurn(t.Context(), "s", false)
	var streamed strings.Builder
	content, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: message}}, tools(o),
		func(delta, _ string) error { streamed.WriteString(delta); return nil })
	require.NoError(t, err)
	return content, executed, streamed.String()
}

func allTurnTools(o *Orchestrator) []openai.Tool { return o.turnTools() }

// 21:27: "Тема OCR" - "Търсих в библиотеката ти…" and no tool. The turn is asked again with the
// tools of the books, a call required; only the second answer is kept.
func TestAClaimedSearchOfTheLibraryIsAskedAgainWithTheBooks(t *testing.T) {
	c := realLookupClaims[0]
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk(c.answer)},
		{toolCallChunk(0, "call_1", searchDocumentsTool, `{"query":"OCR"}`)},
		{textChunk("В книгите ти няма нищо за OCR.")},
	}}
	reg := &fakeToolRegistry{result: map[string]any{"results": []any{}}}

	content, executed, streamed := lookupTurn(t, mock, reg, allTurnTools, c.message)

	assert.Equal(t, "В книгите ти няма нищо за OCR.", content, "the false claim is not stored")
	assert.Equal(t, []string{searchDocumentsTool}, toolCallNames(executed))
	assert.Equal(t, []string{searchDocumentsTool}, reg.executed)
	assert.Equal(t, c.answer+lookupPlaces[lookupLibrary].notice+"В книгите ти няма нищо за OCR.", streamed)

	require.Len(t, mock.Requests, 3)
	retry := mock.Requests[1]
	assert.Equal(t, "required", retry.ToolChoice)
	assert.ElementsMatch(t, []string{searchDocumentsTool, listDocumentsTool}, toolNames(retry.Tools))
	last := retry.Messages[len(retry.Messages)-1].Content
	assert.Contains(t, last, "the library of the user's books")
	assert.True(t, strings.HasPrefix(last, c.message), "the note is under the person's words")
}

// 21:34: "Проверих папката ти с файлове…" - asked again with the file tools open.
func TestAClaimedLookInTheFolderIsAskedAgainWithTheFileTools(t *testing.T) {
	c := realLookupClaims[3]
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk(c.answer)},
		{toolCallChunk(0, "call_1", listFilesTool, `{}`)},
		{textChunk("Папката е празна.")},
	}}
	e, _ := newFilesToolEnv(t)
	o := newGroupOrchestrator(mock, e.reg)
	ctx, _ := e.turn("s", false)

	content, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: c.message}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Папката е празна.", content)
	assert.Equal(t, []string{listFilesTool}, toolCallNames(executed))
	require.Len(t, mock.Requests, 3)
	assert.Equal(t, "required", mock.Requests[1].ToolChoice)
	assert.Contains(t, toolNames(mock.Requests[1].Tools), listFilesTool)
	assert.NotContains(t, toolNames(mock.Requests[1].Tools), filesStubTool, "the group is open")
}

// A claim of a look on the internet where the web tools are not on offer: nothing to ask
// again with, so a line under the answer says that nothing was searched.
func TestAClaimedLookWithoutItsToolsIsCorrected(t *testing.T) {
	c := realLookupClaims[4]
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk(c.answer)}}}
	noWeb := func(o *Orchestrator) []openai.Tool { return withoutTools(o.turnTools(), "web_search", "web_fetch") }

	content, executed, streamed := lookupTurn(t, mock, &fakeToolRegistry{}, noWeb, c.message)

	require.Len(t, mock.Requests, 1, "no retry")
	assert.Empty(t, executed)
	assert.Equal(t, c.answer+"\n\n"+lookupCorrection, content)
	assert.Equal(t, content, streamed, "what was streamed is what is stored")
}

// An answer that did search is left alone, whatever it says.
func TestAClaimAfterAToolCallIsNotTouched(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", searchDocumentsTool, `{"query":"OCR"}`)},
		{textChunk("Търсих в библиотеката ти, но няма нищо за OCR.")},
	}}

	content, executed, _ := lookupTurn(t, mock, &fakeToolRegistry{result: "[]"}, allTurnTools, "Тема OCR")

	assert.Equal(t, "Търсих в библиотеката ти, но няма нищо за OCR.", content)
	assert.Equal(t, []string{searchDocumentsTool}, toolCallNames(executed))
	assert.Len(t, mock.Requests, 2)
}

// An answer that says nothing about a look is not asked again.
func TestAnAnswerWithoutAClaimIsNotAskedAgain(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("Добро утро! Как спа?")}}}

	content, _, _ := lookupTurn(t, mock, &fakeToolRegistry{}, allTurnTools, "добро утро")

	assert.Equal(t, "Добро утро! Как спа?", content)
	assert.Len(t, mock.Requests, 1)
}

// 21:30: "Опитай да намериш в интернет…" - the first round must call web_search.
func TestAMessageThatAsksToLookOnTheInternetSearchesFirst(t *testing.T) {
	c := realLookupClaims[2]
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "web_search", `{"query":"OCR tutorial"}`)},
		{textChunk("Ето какво излезе в търсенето.")},
	}}
	reg := &fakeToolRegistry{result: map[string]any{"results": []any{}}}

	content, executed, _ := lookupTurn(t, mock, reg, allTurnTools, c.message)

	assert.Equal(t, "Ето какво излезе в търсенето.", content)
	assert.Equal(t, []string{"web_search"}, toolCallNames(executed))
	require.NotEmpty(t, mock.Requests)
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
	assert.Equal(t, []string{"web_search"}, toolNames(mock.Requests[0].Tools))
}

// A look on the internet the person asked for is not a question to the library: with the
// library sent to the paid model, it stays local.
func TestAForcedLookOnTheInternetStaysLocal(t *testing.T) {
	local := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "web_search", `{"query":"OCR"}`)},
		{textChunk("локално")},
	}}
	paid := say("облак")

	content, by, err := ask(t, routed(local, paid, true), context.Background(), "Опитай да намериш в интернет статия за OCR")

	require.NoError(t, err)
	assert.Equal(t, "локално", content)
	assert.Equal(t, answeredBy{Provider: "local"}, by)
	assert.Empty(t, paid.Requests)
}

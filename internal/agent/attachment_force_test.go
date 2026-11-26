package agent

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

func TestAsksAboutAttachment(t *testing.T) {
	names := []string{"main.py"}
	for _, m := range []string{
		"какво мислиш за този код",
		"закъчен е към чата ,но не мисля че го виждаш правилно",
		"прикачих ти файла, виж го",
		"има ли грешка в main.py?",
		"какво прави скриптът",
		"look at the attached file",
	} {
		assert.True(t, asksAboutAttachment(m, names), m)
	}
	for _, m := range []string{
		"здравей",
		"какви са днешните новини",
		"запиши кода във файл бележки.md", // a request for the file tools
		"",
	} {
		assert.False(t, asksAboutAttachment(m, names), m)
	}
}

func TestAsksForNews(t *testing.T) {
	for _, m := range []string{
		"Здравей Лили ,какви са днешните водещи световни новини",
		"какво се случва по света днес?",
		"any news about the elections?",
	} {
		assert.True(t, asksForNews(m), m)
	}
	for _, m := range []string{"здравей", "какво мислиш за този код", "новинарски сайт ли е това"} {
		assert.False(t, asksForNews(m), m)
	}
}

// 2026-11-26, Flet: main.py (507 lines) is not shown in the prompt; "какво мислиш за този код" must
// read it first.
func TestAMessageAboutAHiddenAttachmentReadsItFirst(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", readAttachmentTool, `{"name":"main.py"}`)},
		{textChunk("Това е входната точка на клиента на Flet.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{result: map[string]any{"lines": "1: import asyncio"}})
	ctx, turn := withTurn(t.Context(), "s", false)
	turn.setHiddenAttachments([]string{"main.py"})
	tools := append(o.turnTools(), attachmentToolDefinition())

	content, executed, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "какво мислиш за този код"}}, tools, nil)

	require.NoError(t, err)
	assert.Equal(t, "Това е входната точка на клиента на Flet.", content)
	assert.Equal(t, []string{readAttachmentTool}, toolCallNames(executed))
	require.NotEmpty(t, mock.Requests)
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
	assert.Equal(t, []string{readAttachmentTool}, toolNames(mock.Requests[0].Tools))
}

// A file the prompt shows in full is not read again; with no attachment the model chooses.
func TestAShownAttachmentIsLeftToTheModel(t *testing.T) {
	for _, hidden := range [][]string{nil, {}} {
		mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("Кодът е кратък и ясен.")}}}
		o := newGroupOrchestrator(mock, &fakeToolRegistry{})
		ctx, turn := withTurn(t.Context(), "s", false)
		turn.setHiddenAttachments(hidden)
		_, _, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "какво мислиш за този код"}},
			append(o.turnTools(), attachmentToolDefinition()), nil)
		require.NoError(t, err)
		require.NotEmpty(t, mock.Requests)
		assert.Nil(t, mock.Requests[0].ToolChoice)
	}
}

// "Какви са днешните новини" must search first.
func TestAQuestionAboutTheNewsSearchesFirst(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "web_search", `{"query":"world news today"}`)},
		{textChunk("Ето новините от днес.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{result: map[string]any{"results": []any{}}})
	ctx, _ := withTurn(t.Context(), "s", false)
	_, executed, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "Здравей Лили ,какви са днешните водещи световни новини"},
	}, o.turnTools(), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"web_search"}, toolCallNames(executed))
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
}

// Through the whole turn: a file too big for the prompt is read first; a small one, shown in
// full, is left to the model.
func TestTheTurnReadsABigAttachedFileFirst(t *testing.T) {
	e := newAttachmentEnv(t)
	e.attach(t, e.userID, "big", "huge.go", bigFile())
	model := answeringModel()
	askAboutFiles(t, orchestratorWith(t, e, model), e.userID, "big")
	require.NotEmpty(t, model.Requests)
	assert.Equal(t, "required", model.Requests[0].ToolChoice)
	assert.Equal(t, []string{readAttachmentTool}, toolNames(model.Requests[0].Tools))

	e.attach(t, e.userID, "small", "calc.py", "def add(a, b):\n    return a - b\n")
	model = answeringModel()
	askAboutFiles(t, orchestratorWith(t, e, model), e.userID, "small")
	require.NotEmpty(t, model.Requests)
	assert.Nil(t, model.Requests[0].ToolChoice, "the file is in the prompt")
}

func TestAsksByTheBooks(t *testing.T) {
	for _, m := range []string{
		"според базата данни от книгите ,какъв съвет би ми дала",
		"за кода става дума ,след като го виждаш -кажи ми според книгите как е по добре да го преструкторирам или убдейтна",
		"какво пише в книгите за конкурентността",
		"дай ми съвет от библиотеката",
		"what do my books say, according to the books?",
	} {
		assert.True(t, asksByTheBooks(m), m)
	}
	for _, m := range []string{
		"Добави в библиотеката: https://example.org/a.pdf",
		"качи книгата в библиотеката",
		"свали ми книгата от библиотеката",
		"according to you, is it fine?",
		"какво мислиш за този код",
		"обичам книгите",
	} {
		assert.False(t, asksByTheBooks(m), m)
	}
}

// Flet, 2026-11-26 17:53: "кажи ми според книгите как да го преструктурирам" about main.py, which the
// prompt does not show. The file is read first and the books are searched in the round after it.
func TestTheFileIsReadAndThenTheBooksAreSearched(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", readAttachmentTool, `{"name":"main.py"}`)},
		{toolCallChunk(0, "call_2", searchDocumentsTool, `{"query":"преструктуриране на код","query_en":"refactoring Python code structure"}`)},
		{textChunk("По Python Cookbook: изнеси настройките в отделен модул.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{result: map[string]any{"lines": "1: import asyncio"}})
	ctx, turn := withTurn(t.Context(), "s", false)
	turn.setHiddenAttachments([]string{"main.py"})
	tools := append(o.turnTools(), attachmentToolDefinition())

	_, executed, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser,
		Content: "за кода става дума ,след като го виждаш -кажи ми според книгите как е по добре да го преструкторирам или убдейтна"}}, tools, nil)

	require.NoError(t, err)
	assert.Equal(t, []string{readAttachmentTool, searchDocumentsTool}, toolCallNames(executed))
	require.Len(t, mock.Requests, 3)
	assert.Equal(t, []string{readAttachmentTool}, toolNames(mock.Requests[0].Tools))
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
	assert.Equal(t, []string{searchDocumentsTool}, toolNames(mock.Requests[1].Tools))
	assert.Equal(t, "required", mock.Requests[1].ToolChoice)
	assert.Nil(t, mock.Requests[2].ToolChoice, "the answer is free")
}

// The books without a file: searched in the first round.
func TestAQuestionByTheBooksSearchesFirst(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", searchDocumentsTool, `{"query":"съвет"}`)},
		{textChunk("Ето съвета.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{result: map[string]any{"results": []any{}}})
	ctx, _ := withTurn(t.Context(), "s", false)
	_, executed, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "според базата данни от книгите ,какъв съвет би ми дала"},
	}, o.turnTools(), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{searchDocumentsTool}, toolCallNames(executed))
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
}

package agent

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

func TestAsksToAddToLibrary(t *testing.T) {
	yes := []string{
		"Добави в библиотеката: https://arxiv.org/abs/1810.04805",
		"Добави и това в библиотеката: https://arxiv.org/abs/1512.03385",
		"Свали тази статия в библиотеката ми: https://arxiv.org/abs/2005.14165",
		"Добави в библиотеката под заглавие „Измислено заглавие“: https://example.org/a.pdf",
		"https://example.org/a.pdf https://example.org/b.pdf качи ги в библиотеката",
		"Add this to my library: https://example.org/a.pdf",
	}
	no := []string{
		"Добави в библиотеката статията за вниманието", // no link
		"Прочети https://example.org/a и ми кажи за какво е",
		"Прочети https://example.org/library/add/a.pdf и ми кажи за какво е", // the words are only in the link
		"Запиши цялата статия във файл: https://arxiv.org/abs/1706.03762",
		"Има ли в библиотеката ми нещо като https://example.org/a?",
		"Какво е качеството на https://example.org/a в сравнение с библиотеката?",
	}
	for _, m := range yes {
		assert.True(t, asksToAddToLibrary(m), m)
	}
	for _, m := range no {
		assert.False(t, asksToAddToLibrary(m), m)
	}
}

// The second link of a conversation opens with add_to_library alone, required: left free, the
// model said "Добавих и този линк" with no call.
func TestALinkForTheLibraryIsForced(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", addToLibraryTool, `{"urls":["https://example.org/b.pdf"]}`)},
		{textChunk("Добавих го.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{result: map[string]string{"status": addQueued}})
	tools := append(o.turnTools(), addToLibraryDefinition())

	_, executed, err := o.generateResponse(t.Context(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "Добави в библиотеката: https://example.org/a.pdf"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Добавих го в библиотеката."},
		{Role: openai.ChatMessageRoleUser, Content: "Добави и това в библиотеката: https://example.org/b.pdf"},
	}, tools, nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 2)
	assert.Equal(t, []string{addToLibraryTool}, toolNames(mock.Requests[0].Tools))
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
	assert.Equal(t, []string{addToLibraryTool}, toolCallNames(executed))
	assert.Empty(t, mock.Requests[1].ToolChoice, "the rest of the turn as before")
}

// Where the turn does not offer the tool (no link the person wrote, or the tool is off), nothing is forced.
func TestNoLibraryAddIsForcedWithoutTheTool(t *testing.T) {
	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Добре.")}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})
	_, _, err := o.generateResponse(t.Context(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "Добави в библиотеката: https://example.org/a.pdf"},
	}, o.turnTools(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, mock.Requests)
	assert.NotContains(t, toolNames(mock.Requests[0].Tools), addToLibraryTool)
	assert.Empty(t, mock.Requests[0].ToolChoice)
}

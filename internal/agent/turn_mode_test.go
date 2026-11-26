package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/llm"
)

func TestValidModes(t *testing.T) {
	for _, m := range append(knownModes(), "") {
		assert.True(t, validMode(m), m)
	}
	for _, m := range []string{"Books", "search", "files ", "x"} {
		assert.False(t, validMode(m), m)
	}
	for _, m := range knownModes() {
		assert.NotEmpty(t, modeLabels[m], m)
	}
}

// In a mode the first round offers only the mode's tools and requires a call; the rounds after it
// are as before.
func TestAModeRequiresItsToolsInTheFirstRound(t *testing.T) {
	cases := map[string]struct {
		call string
		want []string
	}{
		ModeBooks:   {searchDocumentsTool, []string{searchDocumentsTool, listDocumentsTool}},
		ModeWeb:     {"web_search", []string{"web_search", "web_fetch"}},
		ModeLibrary: {addToLibraryTool, []string{addToLibraryTool}},
		ModeFiles:   {listFilesTool, []string{listFilesTool, readFileTool, writeFileTool, appendFileTool, savePageTool}},
		ModeMemory:  {"create_memory", []string{"create_memory", "search_memories"}},
		ModeStyle:   {"save_global_rule", []string{"save_global_rule", styleNoteTool}},
	}
	for mode, c := range cases {
		t.Run(mode, func(t *testing.T) {
			mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
				{toolCallChunk(0, "call_1", c.call, `{"query":"x","content":"x","urls":["https://example.org/a"]}`)},
				{textChunk("Готово.")},
			}}
			o := newGroupOrchestrator(mock, &fakeToolRegistry{result: map[string]string{"status": "ok"}})
			tools := append(o.turnTools(), addToLibraryDefinition())
			ctx, turn := withTurn(t.Context(), "s", false)
			turn.setMode(mode)

			_, executed, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: "здравей"},
			}, tools, nil)

			require.NoError(t, err)
			require.Len(t, mock.Requests, 2)
			assert.ElementsMatch(t, c.want, toolNames(mock.Requests[0].Tools))
			assert.Equal(t, "required", mock.Requests[0].ToolChoice)
			assert.Equal(t, []string{c.call}, toolCallNames(executed))
			assert.Empty(t, mock.Requests[1].ToolChoice, "the rest of the turn is free")
		})
	}
}

func TestWithoutAModeNothingIsRequired(t *testing.T) {
	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Здравей!")}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})
	_, _, err := o.generateResponse(t.Context(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "здравей"},
	}, o.turnTools(), nil)
	require.NoError(t, err)
	assert.Empty(t, mock.Requests[0].ToolChoice)
}

func (e *taskRunEnv) sayIn(t *testing.T, session, message, mode string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(ChatRequest{SessionID: session, Content: message, Mode: mode})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	e.o.HandleStreamChat(rec, req)
	return rec.Code, rec.Body.String()
}

func TestAnUnknownModeIsRefused(t *testing.T) {
	e := newTaskRunEnv(t)
	code, _ := e.sayIn(t, "m", "здравей", "everything")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Empty(t, e.model.Requests)
}

// The library mode needs a link the person wrote; without one the code says so.
func TestTheLibraryModeWithoutALinkIsAnsweredByTheCode(t *testing.T) {
	e := newTaskRunEnv(t)
	e.o.WithLibraryAdd()
	code, out := e.sayIn(t, "m", "добави статията за вниманието", ModeLibrary)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, out, "ми трябва линк")
	assert.Empty(t, e.model.Requests, "the model is not asked")
}

// "Какво можеш?" in a mode goes down the road of the mode, not to the help list of the code.
func TestInAModeTheCodeDoesNotAnswerInsteadOfTheModel(t *testing.T) {
	e := newTaskRunEnv(t)
	e.o.WithHelpAnswer()
	code, out := e.sayIn(t, "m", "Какви инструменти имаш?", ModeMemory)
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, out, "Ето какво мога")
	require.NotEmpty(t, e.model.Requests)
	assert.ElementsMatch(t, []string{"create_memory", "search_memories"}, toolNames(e.model.Requests[0].Tools))
	assert.Equal(t, "required", e.model.Requests[0].ToolChoice)

	_, out = e.sayIn(t, "m2", "Какви инструменти имаш?", "")
	assert.Contains(t, out, "Ето какво мога", "without a mode as before")
}

// The refusal names every mode the server takes (it named six of eight until 2026-11-26).
func TestAnUnknownModeIsRefusedWithAllTheModes(t *testing.T) {
	e := newTaskRunEnv(t)
	code, body := e.sayIn(t, "m", "здравей", "everything")
	assert.Equal(t, http.StatusBadRequest, code)
	for _, mode := range knownModes() {
		assert.Contains(t, body, mode)
	}
}

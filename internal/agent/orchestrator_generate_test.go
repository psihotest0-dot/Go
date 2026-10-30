package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

// fakeToolRegistry records tool executions and returns a canned result.
type fakeToolRegistry struct {
	executed []string
	args     []map[string]interface{}
	result   interface{}
	err      error
}

func (f *fakeToolRegistry) ExecuteTool(ctx context.Context, name string, args map[string]interface{}) (interface{}, error) {
	f.executed = append(f.executed, name)
	f.args = append(f.args, args)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func textChunk(content string) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{
			{Delta: openai.ChatCompletionStreamChoiceDelta{Content: content}},
		},
	}
}

func reasoningChunk(reasoning string) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{
			{Delta: openai.ChatCompletionStreamChoiceDelta{ReasoningContent: reasoning}},
		},
	}
}

func toolCallChunk(index int, id, name, args string) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{
			{Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{
					Index:    &index,
					ID:       id,
					Type:     openai.ToolTypeFunction,
					Function: openai.FunctionCall{Name: name, Arguments: args},
				}},
			}},
		},
	}
}

func newGenerateOrchestrator(mock *llm.MockLLM, tools ToolRegistry) *Orchestrator {
	return &Orchestrator{llmClient: mock, tools: tools}
}

func TestMergeToolCallDeltasIgnoresOutOfRangeIndex(t *testing.T) {
	var calls []openai.ToolCall

	// A hostile or broken stream can claim any index; it must not be able to
	// grow the slice on demand.
	mergeToolCallDeltas(&calls, []openai.ToolCall{
		{Index: intPtr(1_000_000_000)},
		{Index: intPtr(-1)},
		{Index: nil},
	})

	assert.Empty(t, calls, "out-of-range and index-less deltas must be dropped")

	mergeToolCallDeltas(&calls, []openai.ToolCall{{
		Index:    intPtr(0),
		ID:       "call_1",
		Type:     openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "echo", Arguments: `{"q":`},
	}})
	mergeToolCallDeltas(&calls, []openai.ToolCall{{
		Index:    intPtr(0),
		Function: openai.FunctionCall{Arguments: `"hi"}`},
	}})

	require.Len(t, calls, 1)
	assert.Equal(t, "call_1", calls[0].ID)
	assert.Equal(t, "echo", calls[0].Function.Name)
	assert.JSONEq(t, `{"q":"hi"}`, calls[0].Function.Arguments)
}

func intPtr(v int) *int { return &v }

func TestGenerateResponseRunsToolRoundThenAnswers(t *testing.T) {
	tools := &fakeToolRegistry{result: map[string]string{"answer": "42"}}
	mock := &llm.MockLLM{
		StreamScript: [][]openai.ChatCompletionStreamResponse{
			{toolCallChunk(0, "call_1", "echo", `{"q":"hi"}`)},
			{textChunk("Готово"), textChunk("!")},
		},
	}
	orchestrator := newGenerateOrchestrator(mock, tools)

	initial := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system"}}
	content, _, toolCalls, err := orchestrator.generateResponse(context.Background(), initial, nil, nil)

	require.NoError(t, err)
	assert.Equal(t, "Готово!", content)
	require.Len(t, toolCalls, 1)
	assert.Equal(t, []string{"echo"}, tools.executed)

	require.Len(t, mock.Requests, 2, "one request for the tool round and one for the answer")
	secondReq := mock.Requests[1]
	require.Len(t, secondReq.Messages, len(initial)+2, "assistant tool call plus one tool result")

	assert.Equal(t, openai.ChatMessageRoleAssistant, secondReq.Messages[1].Role)
	require.Len(t, secondReq.Messages[1].ToolCalls, 1)
	assert.Equal(t, openai.ChatMessageRoleTool, secondReq.Messages[2].Role)
	assert.Equal(t, "call_1", secondReq.Messages[2].ToolCallID)
	assert.Contains(t, secondReq.Messages[2].Content, "42")

	assert.Len(t, initial, 1, "the caller's slice must not be modified")
}

func TestGenerateResponsePropagatesSecondStreamError(t *testing.T) {
	tools := &fakeToolRegistry{result: "ok"}
	secondErr := errors.New("provider reset the connection")
	mock := &llm.MockLLM{
		StreamScript: [][]openai.ChatCompletionStreamResponse{
			{toolCallChunk(0, "call_1", "echo", `{}`)},
			{textChunk("never sent")},
		},
		StreamErrors: []error{nil, secondErr},
	}
	orchestrator := newGenerateOrchestrator(mock, tools)

	content, _, toolCalls, err := orchestrator.generateResponse(
		context.Background(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
		nil,
		nil,
	)

	require.ErrorIs(t, err, secondErr, "a failed follow-up stream must not look like success")
	assert.Empty(t, content)
	assert.Len(t, toolCalls, 1, "the executed tool call is still reported")
}

func TestGenerateResponsePropagatesDeltaCallbackError(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("hi")}}}
	orchestrator := newGenerateOrchestrator(mock, &fakeToolRegistry{})

	clientGone := errors.New("client disconnected")
	_, _, _, err := orchestrator.generateResponse(
		context.Background(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
		nil,
		func(content, reasoning string) error { return clientGone },
	)

	require.ErrorIs(t, err, clientGone)
}

func TestGenerateResponseStopsAfterToolRoundLimit(t *testing.T) {
	tools := &fakeToolRegistry{result: "ok"}
	// The model keeps asking for tools forever.
	mock := &llm.MockLLM{
		StreamScript: [][]openai.ChatCompletionStreamResponse{
			{toolCallChunk(0, "call_1", "echo", `{}`)},
		},
	}
	orchestrator := newGenerateOrchestrator(mock, tools)

	content, _, toolCalls, err := orchestrator.generateResponse(
		context.Background(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
		[]openai.Tool{{Type: openai.ToolTypeFunction}},
		nil,
	)

	require.NoError(t, err)
	assert.Empty(t, content)
	assert.Len(t, toolCalls, maxToolRounds, "tool execution stops at the round limit")

	require.Len(t, mock.Requests, maxToolRounds+2, "the final attempt runs without tools")
	lastReq := mock.Requests[len(mock.Requests)-1]
	assert.Empty(t, lastReq.Tools, "the final attempt must not offer tools again")

	// The fallback must not repeat the tool output as a synthetic user message.
	for _, msg := range lastReq.Messages {
		if msg.Role == openai.ChatMessageRoleUser {
			assert.Equal(t, "hi", msg.Content)
		}
	}
}

func TestExecuteToolCallsKeepsOneMessagePerCall(t *testing.T) {
	orchestrator := newGenerateOrchestrator(&llm.MockLLM{}, &fakeToolRegistry{err: errors.New("boom")})

	messages := orchestrator.executeToolCalls(context.Background(), []openai.ToolCall{
		{ID: "call_1", Function: openai.FunctionCall{Name: "search", Arguments: "{}"}},
		{ID: "call_2"},
	})

	require.Len(t, messages, 2, "every call needs a tool message or the next request is invalid")
	assert.Equal(t, "call_1", messages[0].ToolCallID)
	assert.Contains(t, messages[0].Content, "boom")
	assert.Equal(t, "call_2", messages[1].ToolCallID)
	assert.Contains(t, messages[1].Content, "tool name missing")
}

func TestExecuteToolCallsTruncatesHugeResults(t *testing.T) {
	tools := &fakeToolRegistry{result: strings.Repeat("a", maxToolResultChars+100)}
	orchestrator := newGenerateOrchestrator(&llm.MockLLM{}, tools)

	messages := orchestrator.executeToolCalls(context.Background(), []openai.ToolCall{
		{ID: "call_1", Function: openai.FunctionCall{Name: "search", Arguments: "{}"}},
	})

	require.Len(t, messages, 1)
	assert.LessOrEqual(t, len([]rune(messages[0].Content)), maxToolResultChars+2)
}

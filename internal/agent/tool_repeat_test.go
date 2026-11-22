package agent

import (
	"context"
	"errors"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/metrics"
)

func toolCall(id, name, args string) openai.ToolCall {
	return openai.ToolCall{ID: id, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: name, Arguments: args}}
}

// The same call twice in a turn runs once; the second gets a tool message (the API needs one per
// call) that says the result is above, the record of the turn says it and the counter counts it.
func TestTheSameCallInATurnRunsOnce(t *testing.T) {
	tools := &fakeToolRegistry{result: map[string]interface{}{"status": "success", "results": []interface{}{}}}
	o := newGenerateOrchestrator(&llm.MockLLM{}, tools)
	ctx, turn := withTurn(context.Background(), "s", false)
	before := counterValue(t, metrics.ToolCalls.WithLabelValues(searchDocumentsTool, toolResultDuplicate))

	first := o.executeToolCalls(ctx, []openai.ToolCall{toolCall("c1", searchDocumentsTool, `{"query":"Марк Аврелий","limit":3}`)}, defaultToolResultTokens)
	// The keys in another order, and the old name of the tool: the same call.
	again := o.executeToolCalls(ctx, []openai.ToolCall{
		toolCall("c2", searchDocumentsTool, `{"limit":3,"query":"Марк Аврелий"}`),
		toolCall("c3", "search_knowledge_base", `{"query":"Марк Аврелий","limit":3}`),
	}, defaultToolResultTokens)

	assert.Equal(t, []string{searchDocumentsTool}, tools.executed, "the tool ran once")
	require.Len(t, first, 1)
	require.Len(t, again, 2, "every call keeps its tool message")
	assert.Equal(t, repeatedCallResult(searchDocumentsTool), again[0].Content)
	assert.Equal(t, repeatedCallResult("search_knowledge_base"), again[1].Content)
	assert.Equal(t, before+2, counterValue(t, metrics.ToolCalls.WithLabelValues(searchDocumentsTool, toolResultDuplicate)))
	assert.Contains(t, turn.storedToolLog(), "search_documents: the same call again - not run")
}

// Other arguments are another call; a call that did not work may be tried again.
func TestAnotherArgumentOrAFailedCallRunsAgain(t *testing.T) {
	tools := &fakeToolRegistry{result: "ok"}
	o := newGenerateOrchestrator(&llm.MockLLM{}, tools)
	ctx, _ := withTurn(context.Background(), "s", false)

	o.executeToolCalls(ctx, []openai.ToolCall{toolCall("c1", "web_fetch", `{"url":"https://a.org/x"}`)}, defaultToolResultTokens)
	o.executeToolCalls(ctx, []openai.ToolCall{toolCall("c2", "web_fetch", `{"url":"https://a.org/y"}`)}, defaultToolResultTokens)
	assert.Len(t, tools.executed, 2, "another link is another call")

	tools.err = errors.New("timeout")
	o.executeToolCalls(ctx, []openai.ToolCall{toolCall("c3", "web_search", `{"query":"q"}`)}, defaultToolResultTokens)
	tools.err = nil
	msgs := o.executeToolCalls(ctx, []openai.ToolCall{toolCall("c4", "web_search", `{"query":"q"}`)}, defaultToolResultTokens)
	assert.Len(t, tools.executed, 4, "the call that failed ran again")
	assert.Contains(t, msgs[0].Content, "ok")
}

// "Read the file, write it, read it again": the second read is of another file and runs. The same
// write twice is still one.
func TestAChangeLetsTheEarlierCallsRunAgain(t *testing.T) {
	tools := &fakeToolRegistry{result: map[string]interface{}{"status": "saved"}}
	o := newGenerateOrchestrator(&llm.MockLLM{}, tools)
	ctx, _ := withTurn(context.Background(), "s", false)
	read := toolCall("r", readFileTool, `{"name":"a.txt"}`)
	write := toolCall("w", writeFileTool, `{"name":"a.txt","content":"x"}`)

	for _, c := range []openai.ToolCall{read, write, read, write} {
		o.executeToolCalls(ctx, []openai.ToolCall{c}, defaultToolResultTokens)
	}

	assert.Equal(t, []string{readFileTool, writeFileTool, readFileTool}, tools.executed,
		"the read after the write runs; the second write, right after the read that changed nothing, is the same write")
}

// Outside a chat turn there is nothing to remember the calls in, and nothing is held back.
func TestOutsideATurnEveryCallRuns(t *testing.T) {
	tools := &fakeToolRegistry{result: "ok"}
	o := newGenerateOrchestrator(&llm.MockLLM{}, tools)
	for range 2 {
		o.executeToolCalls(context.Background(), []openai.ToolCall{toolCall("c", "web_search", `{"query":"q"}`)}, defaultToolResultTokens)
	}
	assert.Len(t, tools.executed, 2)
}

// Through the rounds: the model asks for the same search in the second round; it is not run,
// and the model is answered from the first. A new run of the rounds (a retry of the turn, which
// starts from the messages without the earlier results) may make the call again.
func TestTheRoundsDoNotRunTheSameCallTwice(t *testing.T) {
	tools := &fakeToolRegistry{result: "passages"}
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", "web_search", `{"query":"q"}`)},
		{toolCallChunk(0, "c2", "web_search", `{"query":"q"}`)},
		{textChunk("Ето.")},
		{toolCallChunk(0, "c3", "web_search", `{"query":"q"}`)},
		{textChunk("Пак.")},
	}}
	o := newGenerateOrchestrator(mock, tools)
	ctx, _ := withTurn(context.Background(), "s", false)
	user := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "потърси q"}}
	offer := []openai.Tool{{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "web_search"}}}

	content, executed, err := o.runRounds(ctx, user, offer, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "Ето.", content)
	assert.Len(t, executed, 2, "the model asked twice")
	assert.Len(t, tools.executed, 1, "the search ran once")
	third := mock.Requests[2].Messages
	assert.Equal(t, repeatedCallResult("web_search"), third[len(third)-1].Content)

	_, _, err = o.runRounds(ctx, user, offer, nil, nil)
	require.NoError(t, err)
	assert.Len(t, tools.executed, 2, "a new run of the rounds does not see the earlier results, so the call runs")
}

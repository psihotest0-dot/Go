package agent

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/metrics"
)

func toolNames(tools []openai.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Function.Name
	}
	return names
}

var fileToolNames = []string{listFilesTool, readFileTool, writeFileTool, appendFileTool}

func newGroupOrchestrator(mock *llm.MockLLM, reg ToolRegistry) *Orchestrator {
	return &Orchestrator{llmClient: mock, tools: reg, groups: []toolGroup{filesGroup()}}
}

func TestATurnOffersOnlyTheStubOfAGroup(t *testing.T) {
	plain := &Orchestrator{}
	assert.Equal(t, toolNames(toolDefinitions()), toolNames(plain.turnTools()), "without groups the tools are as they were")

	grouped := newGroupOrchestrator(&llm.MockLLM{}, &fakeToolRegistry{})
	names := toolNames(grouped.turnTools())
	assert.Equal(t, append(toolNames(toolDefinitions()), filesStubTool), names)
	for _, tool := range fileToolNames {
		assert.NotContains(t, names, tool, "the tools of the group are not described until the model asks")
	}
}

// The stub is worth what the measurement said only with the wording it was measured with.
func TestTheStubAndTheToolsAreWhatWasMeasured(t *testing.T) {
	g := filesGroup()
	assert.Equal(t, filesStubTool, g.stubName())
	desc := g.stub.Function.Description
	for _, example := range []string{"save this", "put it in notes.txt", "read data.csv", "what is in the folder?", "even if they do not say 'file'"} {
		assert.Contains(t, desc, example)
	}
	assert.Equal(t, fileToolNames, toolNames(g.tools))
	assert.Equal(t, filesGroup().stub, filesGroup().stub)
}

func TestGenerateResponseUnlocksTheFileToolsWhenTheModelAsks(t *testing.T) {
	e, store := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
		{toolCallChunk(0, "call_2", writeFileTool, `{"name":"бележки.md","content":"мляко и хляб"}`)},
		{textChunk("Записах го в бележки.md.")},
	}}
	o := newGroupOrchestrator(mock, e.reg)
	before := counterValue(t, metrics.ToolGroupUnlocks.WithLabelValues("files"))

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши ми това във файл бележки.md"}},
		o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Записах го в бележки.md.", content)
	require.Len(t, executed, 2)
	require.Len(t, mock.Requests, 3)

	first := toolNames(mock.Requests[0].Tools)
	assert.Contains(t, first, filesStubTool)
	for _, tool := range fileToolNames {
		assert.NotContains(t, first, tool, "round 1: only the stub")
	}
	for i := 1; i <= 2; i++ {
		later := toolNames(mock.Requests[i].Tools)
		assert.NotContains(t, later, filesStubTool, "round %d: the stub is gone", i+1)
		for _, tool := range fileToolNames {
			assert.Contains(t, later, tool, "round %d: the tools came", i+1)
		}
		assert.Contains(t, later, "search_documents", "the other tools stay")
	}

	assert.Equal(t, "мляко и хляб", e.fileText(t, store, "бележки.md"))
	assert.InDelta(t, before+1, counterValue(t, metrics.ToolGroupUnlocks.WithLabelValues("files")), 0.001)
}

// Asking for the tools does nothing for the user. If it used up one of the two rounds for
// tools, a model that lists the folder before it writes would never get to write.
func TestUnlockingDoesNotUseUpARoundForTools(t *testing.T) {
	e, store := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
		{toolCallChunk(0, "call_2", listFilesTool, `{}`)},
		{toolCallChunk(0, "call_3", writeFileTool, `{"name":"a.txt","content":"text"}`)},
		{textChunk("Готово")},
	}}
	o := newGroupOrchestrator(mock, e.reg)

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши го във файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Готово", content)
	require.Len(t, executed, 3, "the unlock, the list and the write")
	assert.Equal(t, "text", e.fileText(t, store, "a.txt"))
}

// A group unlocks once. A model that goes on calling the stub is stopped by the limit of
// the rounds for tools, as any other.
func TestAGroupUnlocksOnce(t *testing.T) {
	e, _ := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
	}}
	o := newGroupOrchestrator(mock, e.reg)
	before := counterValue(t, metrics.ToolGroupUnlocks.WithLabelValues("files"))

	_, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши го във файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.LessOrEqual(t, len(mock.Requests), maxToolRounds+3, "the loop ends")
	assert.InDelta(t, before+1, counterValue(t, metrics.ToolGroupUnlocks.WithLabelValues("files")), 0.001, "unlocked once")
}

// Rounds that only unlock are free because a group unlocks once. If a stub were ever left
// on offer after its group unlocked, those rounds would never end the turn: the number of
// them is bounded by the number of groups.
func TestFreeUnlockRoundsAreBoundedEvenIfTheStubStays(t *testing.T) {
	stub := filesStubDefinition()
	stuck := toolGroup{name: "stuck", stub: stub, tools: []openai.Tool{stub}} // unlocks into itself
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
	}}
	o := &Orchestrator{llmClient: mock, tools: &fakeToolRegistry{result: "ok"}, groups: []toolGroup{stuck}}

	_, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши го във файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.LessOrEqual(t, len(mock.Requests), maxToolRounds+4, "the turn ends")
}

func TestAfterReadingAFileTheWebIsNotOffered(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "plan.txt", "private plans")
	require.NoError(t, err)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
		{toolCallChunk(0, "call_2", readFileTool, `{"name":"plan.txt"}`)},
		{textChunk("В плана пише: private plans")},
	}}
	o := newGroupOrchestrator(mock, e.reg)

	_, _, _, err = o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Прочети plan.txt"}}, o.turnTools(), nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 3)
	before := toolNames(mock.Requests[1].Tools)
	assert.Contains(t, before, "web_search")
	assert.Contains(t, before, "web_fetch")
	after := toolNames(mock.Requests[2].Tools)
	assert.NotContains(t, after, "web_search", "a turn that read a file sends nothing out")
	assert.NotContains(t, after, "web_fetch")
	assert.Contains(t, after, "search_documents", "the rest stays")
	assert.Contains(t, after, readFileTool)
}

func TestAfterTheWebReadFileIsNotOffered(t *testing.T) {
	e, _ := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "web_search", `{"query":"weather"}`)},
		{toolCallChunk(0, "call_2", filesStubTool, `{}`)},
		{textChunk("Ето времето")},
	}}
	o := newGroupOrchestrator(mock, e.reg)

	_, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Какво е времето и запиши го"}}, o.turnTools(), nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 3)
	assert.Contains(t, toolNames(mock.Requests[1].Tools), filesStubTool)
	unlocked := toolNames(mock.Requests[2].Tools)
	assert.Contains(t, unlocked, writeFileTool, "the group unlocks")
	assert.NotContains(t, unlocked, readFileTool, "but a turn that used the web cannot read a file")
	assert.Contains(t, unlocked, "web_search")
}

// A question about the library still opens with its one tool, with or without the stub.
func TestAGroupDoesNotChangeTheForcedFirstRound(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", listDocumentsTool, `{}`)},
		{textChunk("Да, имам я")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{result: map[string]string{"status": "ok"}})

	_, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Имаш ли книгата за готвене на баба?"}},
		o.turnTools(), nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 2)
	assert.Equal(t, []string{listDocumentsTool}, toolNames(mock.Requests[0].Tools))
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
	assert.Contains(t, toolNames(mock.Requests[1].Tools), filesStubTool, "the stub is back with the rest")
}

func TestNextToolsDoesNotTouchWhatWasNotAsked(t *testing.T) {
	o := newGroupOrchestrator(&llm.MockLLM{}, &fakeToolRegistry{})
	active := o.turnTools()

	same := o.nextTools(active, []openai.ToolCall{{Function: openai.FunctionCall{Name: "search_documents"}}}, nil)
	assert.Equal(t, toolNames(active), toolNames(same))

	unlocked := o.nextTools(active, []openai.ToolCall{{Function: openai.FunctionCall{Name: filesStubTool}}}, nil)
	assert.NotContains(t, toolNames(unlocked), filesStubTool)
	assert.Len(t, unlocked, len(active)-1+len(fileToolNames))
	assert.Len(t, active, len(toolDefinitions())+1, "the list the caller holds is not changed")

	again := o.nextTools(unlocked, []openai.ToolCall{{Function: openai.FunctionCall{Name: filesStubTool}}}, nil)
	assert.Equal(t, toolNames(unlocked), toolNames(again), "there is no stub to unlock the second time")
}

func TestOnlyUnlocksMeansEveryCallIsALockedStub(t *testing.T) {
	o := newGroupOrchestrator(&llm.MockLLM{}, &fakeToolRegistry{})
	active := o.turnTools()
	stub := openai.ToolCall{Function: openai.FunctionCall{Name: filesStubTool}}
	other := openai.ToolCall{Function: openai.FunctionCall{Name: "search_documents"}}

	assert.True(t, o.onlyUnlocks(active, []openai.ToolCall{stub}))
	assert.False(t, o.onlyUnlocks(active, []openai.ToolCall{stub, other}), "a call that does something is a round")
	assert.False(t, o.onlyUnlocks(active, []openai.ToolCall{other}))
	assert.False(t, o.onlyUnlocks(active, nil))
	assert.False(t, o.onlyUnlocks(o.nextTools(active, []openai.ToolCall{stub}, nil), []openai.ToolCall{stub}),
		"a stub that is gone is not an unlock")
}

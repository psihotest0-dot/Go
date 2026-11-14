package agent

import (
	"fmt"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

// A task with files can take three steps: here two new files and a line added to the first.
// With two rounds for tools the third was cut off, and the model - offered no tools - wrote
// the call as text, which the filter removes (the "retry" of the chat of 2026-11-14). A
// group that was unlocked buys more rounds.
func TestAnUnlockedGroupGetsRoomForThreeSteps(t *testing.T) {
	e, store := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
		{toolCallChunk(0, "call_2", writeFileTool, `{"name":"a.txt","content":"мляко"}`)},
		{toolCallChunk(0, "call_3", writeFileTool, `{"name":"b.txt","content":"сирене"}`)},
		{toolCallChunk(0, "call_4", appendFileTool, `{"name":"a.txt","content":"хляб"}`)},
		{textChunk("Готово: a.txt има две неща, b.txt едно.")},
	}}
	o := newGroupOrchestrator(mock, e.reg)

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши мляко в a.txt, сирене в b.txt и добави хляб в a.txt"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Готово: a.txt има две неща, b.txt едно.", content)
	require.Len(t, executed, 4, "the unlock and the three steps")
	assert.Equal(t, "мляко\nхляб", e.fileText(t, store, "a.txt"))
	assert.Equal(t, "сирене", e.fileText(t, store, "b.txt"))
}

// The room has an end: a group buys groupExtraRounds more rounds, not an endless turn.
func TestTheRoomOfAGroupHasAnEnd(t *testing.T) {
	e, _ := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)
	steps := [][]openai.ChatCompletionStreamResponse{{toolCallChunk(0, "call_0", filesStubTool, `{}`)}}
	for i := range 10 {
		steps = append(steps, []openai.ChatCompletionStreamResponse{
			toolCallChunk(0, fmt.Sprintf("call_%d", i+1), listFilesTool, `{}`)})
	}
	mock := &llm.MockLLM{StreamScript: steps}
	o := newGroupOrchestrator(mock, e.reg)

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Покажи файловете"}}, o.turnTools(), nil)

	require.NoError(t, err)
	require.Len(t, executed, 5, "the unlock, the two rounds every turn has and the two the group adds")
	assert.Equal(t, stuckAnswer, sanitizeAssistantText(content))
}

// The room is for a turn that unlocked a group; any other turn keeps the two rounds.
func TestATurnWithoutAGroupKeepsTwoRoundsForTools(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "echo", `{}`)},
	}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{result: "ok"})

	_, _, executed, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, []openai.Tool{{Type: openai.ToolTypeFunction}}, nil)

	require.NoError(t, err)
	assert.Len(t, executed, maxToolRounds)
}

// If the rounds run out and the model still ends without a word the user can see (it wrote
// the next call as text, which is removed), the user is told so instead of getting nothing.
func TestAnAnswerThatIsOnlyAToolCallWrittenAsTextIsReplacedByAnHonestOne(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "echo", `{}`)},
		{toolCallChunk(0, "call_2", "echo", `{}`)},
		{toolCallChunk(0, "call_3", "echo", `{}`)},
		{textChunk(`<|tool_call>call:append_file{content:"x",name:"a.txt"}<tool_call|>`)},
	}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{result: "ok"})
	var streamed strings.Builder

	content, _, executed, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, []openai.Tool{{Type: openai.ToolTypeFunction}},
		func(delta, _ string) error { streamed.WriteString(delta); return nil })

	require.NoError(t, err)
	require.Len(t, executed, maxToolRounds)
	assert.True(t, strings.HasSuffix(content, stuckAnswer), content)
	assert.Equal(t, stuckAnswer, sanitizeAssistantText(content), "this is all the user sees")
	assert.Contains(t, streamed.String(), stuckAnswer)
}

// The last request has no tools; DeepSeek still wrote a call in its own format (the comparison of
// 2026-11-14). The user gets the honest answer, not the markup, and that request carries the note.
func TestADeepSeekCallAsTheLastAnswerIsReplacedByAnHonestOne(t *testing.T) {
	dsml := "\n\n<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"echo\">\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "echo", `{}`)},
		{toolCallChunk(0, "call_2", "echo", `{}`)},
		{toolCallChunk(0, "call_3", "echo", `{}`)},
		{textChunk(dsml)},
	}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{result: "ok"})
	var streamed strings.Builder

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, []openai.Tool{{Type: openai.ToolTypeFunction}},
		func(delta, _ string) error { streamed.WriteString(delta); return nil })

	require.NoError(t, err)
	assert.Equal(t, stuckAnswer, sanitizeAssistantText(content), "this is all the user sees")
	assert.Contains(t, streamed.String(), stuckAnswer, "the stream carries the honest answer (the markup itself is dropped by the filter of the HTTP stream)")
	last := mock.Requests[len(mock.Requests)-1]
	assert.Empty(t, last.Tools)
	assert.Equal(t, roundsOverNote, last.Messages[len(last.Messages)-1].Content)
}

func TestAnAnswerWithWordsIsLeftAloneAfterTools(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "echo", `{}`)},
		{textChunk("Готово")},
	}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{result: "ok"})

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, []openai.Tool{{Type: openai.ToolTypeFunction}}, nil)

	require.NoError(t, err)
	assert.Equal(t, "Готово", content)
}

func TestNothingIsAddedWhenNoToolWasUsed(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("")}}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{})

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, nil, nil)

	require.NoError(t, err)
	assert.Equal(t, "", content, "an empty answer with no tool is another problem, with its own handling")
}

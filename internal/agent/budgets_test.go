package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/llm"
)

func TestNewBudgetsWithoutKnownWindowKeepsTheBuiltInValues(t *testing.T) {
	b := newBudgets(&config.Config{LLMEnableThinking: true})

	assert.Equal(t, 0, b.contextTokens)
	assert.Equal(t, 0, b.answerTokens, "no cap on the answer while the window is unknown")
	assert.Equal(t, 3000, b.promptContextTokens)
	assert.Equal(t, 3000, b.historyTokens)
	assert.Equal(t, 10, b.historyMessages)
	assert.Equal(t, defaultToolResultTokens, b.toolResultTokens)
	assert.True(t, b.thinking)
}

func TestNewBudgetsDerivesFromTheWindow(t *testing.T) {
	tests := []struct {
		name         string
		thinking     bool
		wantAnswer   int
		minHistory   int
		minPrompt    int
		wantMessages int
	}{
		{name: "thinking off", thinking: false, wantAnswer: defaultAnswerTokens, minHistory: 4000, minPrompt: 2800, wantMessages: 25},
		{name: "thinking on reserves more for the answer", thinking: true, wantAnswer: defaultThinkingAnswerTokens, minHistory: 3000, minPrompt: 2000, wantMessages: 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newBudgets(&config.Config{LLMContextTokens: 16384, LLMEnableThinking: tt.thinking})

			assert.Equal(t, tt.wantAnswer, b.answerTokens)
			assert.Equal(t, tt.wantMessages, b.historyMessages)
			assert.GreaterOrEqual(t, b.historyTokens, tt.minHistory)
			assert.GreaterOrEqual(t, b.promptContextTokens, tt.minPrompt)
			assert.Equal(t, defaultToolResultTokens, b.toolResultTokens)
			assert.LessOrEqual(t, b.worstCaseTokens(), 16384,
				"with every section full the request still fits the window")
		})
	}
}

func TestNewBudgetsExplicitValuesWin(t *testing.T) {
	b := newBudgets(&config.Config{
		LLMContextTokens:       16384,
		LLMAnswerTokens:        1234,
		LLMPromptContextTokens: 111,
		LLMHistoryTokens:       222,
		LLMHistoryMessages:     7,
		LLMToolResultTokens:    333,
		LLMEnableThinking:      true,
	})

	assert.Equal(t, budgets{
		contextTokens:       16384,
		answerTokens:        1234,
		promptContextTokens: 111,
		historyTokens:       222,
		historyMessages:     7,
		toolResultTokens:    333,
		thinking:            true,
	}, b)
}

func TestNewBudgetsSurviveATinyWindow(t *testing.T) {
	b := newBudgets(&config.Config{LLMContextTokens: 2048})

	assert.Positive(t, b.promptContextTokens)
	assert.Positive(t, b.historyTokens)
	assert.LessOrEqual(t, b.toolResultTokens, 2048/4)
}

func TestOrchestratorWithoutBudgetsUsesTheBuiltInOnes(t *testing.T) {
	o := &Orchestrator{}

	assert.Equal(t, legacyBudgets(), o.limits())
	assert.NotZero(t, o.limits().historyMessages)
}

func TestEstimatePromptTokensCountsEverythingTheModelReads(t *testing.T) {
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: strings.Repeat("a", 400)},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
			Function: openai.FunctionCall{Name: "search_documents", Arguments: strings.Repeat("b", 80)},
		}}},
		{Role: openai.ChatMessageRoleTool, Content: strings.Repeat("c", 800)},
	}

	without := estimatePromptTokens(messages, nil)
	with := estimatePromptTokens(messages, toolDefinitions())

	// 100 + 200 content tokens, 20 for the call arguments, plus the markers.
	assert.GreaterOrEqual(t, without, 100+200+20+3*messageOverheadTokens)
	assert.Greater(t, with-without, toolsTemplateOverheadTokens, "tool definitions are part of the prompt")
}

func TestToolBudgetShrinksWhenThePromptFillsTheWindow(t *testing.T) {
	b := budgets{contextTokens: 16384, answerTokens: 3000, toolResultTokens: 4000}

	small := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	assert.Equal(t, 4000, b.toolBudget(small, nil), "plenty of room: the configured cap applies")

	// The prompt costs 11000 tokens plus 6 for the message markers, so what is
	// left is the window, minus the prompt, minus the answer reserve, minus the
	// 5 % kept for the estimates being rough: 16384 - 11006 - 3000 - 819.
	crowded := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("x", 4*11000)}}
	assert.Equal(t, 1559, b.toolBudget(crowded, nil))

	full := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("x", 4*20000)}}
	assert.Equal(t, 0, b.toolBudget(full, nil), "an overfull prompt never gives a negative budget")
}

func TestToolBudgetWithoutKnownWindowIsTheConfiguredCap(t *testing.T) {
	b := budgets{toolResultTokens: 4000}
	huge := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("x", 4*50000)}}

	assert.Equal(t, 4000, b.toolBudget(huge, nil))
}

func TestAllocateTokens(t *testing.T) {
	tests := []struct {
		name  string
		sizes []int
		total int
		want  []int
	}{
		{name: "everything fits", sizes: []int{100, 200}, total: 1000, want: []int{100, 200}},
		{name: "small results keep what they need, large ones share the rest", sizes: []int{100, 5000, 5000}, total: 2100, want: []int{100, 1000, 1000}},
		{name: "equal results split evenly", sizes: []int{900, 900, 900}, total: 600, want: []int{200, 200, 200}},
		{name: "order of the input is kept", sizes: []int{5000, 100}, total: 1100, want: []int{1000, 100}},
		{name: "no budget", sizes: []int{10, 20}, total: 0, want: []int{0, 0}},
		{name: "negative budget", sizes: []int{10}, total: -5, want: []int{0}},
		{name: "nothing to allocate", sizes: nil, total: 100, want: []int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allocateTokens(tt.sizes, tt.total)

			assert.Equal(t, tt.want, got)
			sum := 0
			for _, n := range got {
				sum += n
			}
			assert.LessOrEqual(t, sum, max(tt.total, 0))
		})
	}
}

func TestFitToolResult(t *testing.T) {
	long := strings.Repeat("б", 4000)

	assert.Equal(t, "кратко", fitToolResult("кратко", 100), "a result that fits is untouched")

	cut := fitToolResult(long, 100)
	assert.LessOrEqual(t, estimateTokens(cut), 100)
	assert.True(t, strings.HasSuffix(cut, toolResultTruncatedMarker))
	assert.True(t, strings.HasPrefix(cut, "бб"), "the start of the result is what is kept")

	assert.Equal(t, toolResultOmittedNotice, fitToolResult(long, minToolResultTokens-1),
		"a stub too small to be useful is replaced by a note")
}

func TestGenerateResponseSendsAnswerLimitAndThinkingSwitch(t *testing.T) {
	for _, thinking := range []bool{true, false} {
		mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("ok")}}
		o := newGenerateOrchestrator(mock, &fakeToolRegistry{})
		o.budgets = newBudgets(&config.Config{LLMContextTokens: 16384, LLMEnableThinking: thinking})

		_, _, _, err := o.generateResponse(context.Background(),
			[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, nil, nil)
		require.NoError(t, err)

		require.Len(t, mock.Requests, 1)
		req := mock.Requests[0]
		assert.Equal(t, o.budgets.answerTokens, req.MaxTokens)
		assert.Equal(t, map[string]any{"enable_thinking": thinking}, req.ChatTemplateKwargs,
			"the switch is sent explicitly, whatever the server was started with")
	}
}

func TestGenerateResponseSendsTheSamplingSettings(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
	}{
		{name: "none configured (left to the server)", cfg: config.Config{}},
		{name: "all configured", cfg: config.Config{LLMTemperature: 0.7, LLMTopP: 0.8, LLMPresencePenalty: 0.6}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("ok")}}
			o := newGenerateOrchestrator(mock, &fakeToolRegistry{})
			tt.cfg.LLMContextTokens = 16384
			o.budgets = newBudgets(&tt.cfg)

			_, _, _, err := o.generateResponse(context.Background(),
				[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, nil, nil)
			require.NoError(t, err)

			require.Len(t, mock.Requests, 1)
			req := mock.Requests[0]
			assert.Equal(t, tt.cfg.LLMTemperature, req.Temperature)
			assert.Equal(t, tt.cfg.LLMTopP, req.TopP)
			assert.Equal(t, tt.cfg.LLMPresencePenalty, req.PresencePenalty)
		})
	}
}

func TestGenerateResponseWithoutKnownWindowSendsNoAnswerLimit(t *testing.T) {
	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("ok")}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{})

	_, _, _, err := o.generateResponse(context.Background(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, nil, nil)
	require.NoError(t, err)

	assert.Zero(t, mock.Requests[0].MaxTokens)
}

// TestToolRoundsStayInsideTheWindow is the scenario that used to overflow a
// 16k slot: several tool calls, each returning far more than fits.
func TestToolRoundsStayInsideTheWindow(t *testing.T) {
	const window = 16384

	hugeResult := strings.Repeat("Дунав е река в Европа. ", 3000) // ~69 000 bytes ~ 17 000 tokens each
	tools := &fakeToolRegistry{result: hugeResult}

	calls := []openai.ChatCompletionStreamResponse{
		toolCallChunk(0, "call_0", "search_documents", `{"query":"a"}`),
		toolCallChunk(1, "call_1", "search_documents", `{"query":"b"}`),
		toolCallChunk(2, "call_2", "search_documents", `{"query":"c"}`),
	}
	mock := &llm.MockLLM{
		StreamScript: [][]openai.ChatCompletionStreamResponse{
			calls,
			calls, // a second round, so the first round's results are already in the prompt
			{textChunk("Отговор")},
		},
	}
	o := newGenerateOrchestrator(mock, tools)
	o.budgets = newBudgets(&config.Config{LLMContextTokens: window, LLMEnableThinking: true})

	// A heavy prompt (the fixed part plus a user turn of 3500 tokens), so the
	// window - not the per-round cap - decides how much the tools may add.
	system := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: strings.Repeat("s", 4*fixedPromptTokens)}
	user := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("в", 4*3500)}

	content, _, _, err := o.generateResponse(context.Background(), []openai.ChatCompletionMessage{system, user}, toolDefinitions(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Отговор", content)
	require.Len(t, mock.Requests, 3)
	for i, req := range mock.Requests {
		prompt := estimatePromptTokens(req.Messages, req.Tools)
		assert.LessOrEqual(t, prompt+req.MaxTokens, window,
			"request %d: prompt (%d) plus the answer reserve (%d) must fit one slot", i, prompt, req.MaxTokens)
	}

	last := mock.Requests[2]
	toolMessages := 0
	for _, m := range last.Messages {
		if m.Role == openai.ChatMessageRoleTool {
			toolMessages++
			assert.NotEmpty(t, m.Content, "every call keeps a tool message")
		}
	}
	assert.Equal(t, 6, toolMessages)
}

func TestStreamOnceLogsRealUsageAndWarnsWhenTheAnswerWasCut(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{
		textChunk("част от отговор"),
		{Choices: []openai.ChatCompletionStreamChoice{{FinishReason: openai.FinishReasonLength}}},
		{Usage: &openai.Usage{PromptTokens: 1234, CompletionTokens: 567, TotalTokens: 1801}},
	}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{})
	o.budgets = newBudgets(&config.Config{LLMContextTokens: 16384, LLMEnableThinking: true})

	_, _, _, err := o.generateResponse(context.Background(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, nil, nil)
	require.NoError(t, err)

	output := logs.String()
	assert.Contains(t, output, "prompt_tokens=1234")
	assert.Contains(t, output, "completion_tokens=567")
	assert.Contains(t, output, "estimated_prompt_tokens=")
	assert.Contains(t, output, "finish_reason=length")
	assert.Contains(t, output, "LLM_ANSWER_TOKENS", "the warning names the setting to change")
}

// TestDerivedBudgetsAlwaysLeaveRoomForToolResults is the guarantee behind
// toolFloorTokens: however full the conversation and the dynamic prompt get
// within their budgets, a round of tool calls still has room for its results.
func TestDerivedBudgetsAlwaysLeaveRoomForToolResults(t *testing.T) {
	for _, thinking := range []bool{true, false} {
		b := newBudgets(&config.Config{LLMContextTokens: 16384, LLMEnableThinking: thinking})

		fullTokens := fixedPromptTokens + maxSummaryTokens + b.promptContextTokens + b.historyTokens
		full := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("x", bytesPerToken*fullTokens)}}

		got := b.toolBudget(full, nil)

		assert.GreaterOrEqual(t, got, toolFloorTokens-messageOverheadTokens, "thinking=%v", thinking)
	}
}

package agent

import (
	"encoding/json"
	"log/slog"
	"sort"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/config"
)

// Token budgets of one chat request.
//
// The model server splits its context between parallel slots, so one request
// gets only a slot's share (32768 / 2 = 16384 tokens for the production setup).
// That window has to hold, at the same time:
//
//	fixed prompt        system template + tool definitions
//	rolling summary     at most maxSummaryTokens
//	dynamic sections    rules, profile facts, retrieved memories, concepts
//	history             the replayed conversation
//	tool results        everything the tools return in a round
//	answer              max_tokens, which includes the reasoning when thinking is on
//
// LLM_CONTEXT_TOKENS tells the agent the window; every budget not set
// explicitly is derived from it (see newBudgets). Without it the agent keeps
// the conservative built-in budgets it shipped with.
const (
	// The built-in budgets, used while the context window is unknown. They fit
	// a model with roughly 8k of context.
	defaultHistoryTokens       = 3000
	defaultHistoryMessages     = 10
	defaultPromptContextTokens = 3000

	// defaultToolResultTokens caps all tool results of one round together.
	defaultToolResultTokens = 4000

	// The answer reserve (max_tokens) once the window is known. With thinking on
	// it also pays for the reasoning: the model needed ~470 tokens to think about
	// 17 x 23, so 5000 is generous for chat while still bounded. A reply cut by
	// the limit is logged (see streamOnce) so the limit can be raised.
	defaultAnswerTokens         = 3000
	defaultThinkingAnswerTokens = 5000

	// derivedHistoryMessages is how many messages of history are read when the
	// window is known.
	derivedHistoryMessages = 25

	// fixedPromptTokens reserves room for the system template and the tool
	// definitions. Measured on 2026-11-01 with the llama-server tokenizer
	// (Qwen3.5-9B): template 1402 tokens, tool definitions 831, together 2233.
	// On 2026-11-02 the template grew by 389 tokens (the same tokenizer, the old
	// and the new text rendered the same way: 1531 and 1920), so together they
	// are now about 2620; the rest is the slack the first figure also had.
	// On 2026-11-07 it was measured again with the Gemma-4-12B tokenizer, because the
	// chat prompt had become Bulgarian in the character part (2 691 tokens before) and
	// got a section for attached files and a tool for reading them: template 2 823 plus
	// tool definitions 796 = 3 619 (the text of a section for attached files comes on top,
	// and is paid from the room of those files). 3 750 keeps about 130 of slack.
	fixedPromptTokens = 3750

	// toolFloorTokens is what the derived budgets leave free for tool results
	// even when the conversation and the dynamic prompt are as large as they may
	// get. The live check showed why: with everything full and no floor, a round
	// of tool calls got nothing and every result was replaced by a note.
	toolFloorTokens = 1500

	// minDerivedRoom keeps the derived budgets usable on a tiny window.
	minDerivedRoom = 1000

	// promptContextShare is the part, in percent, of the room left after the
	// fixed parts that goes to the dynamic system prompt; history gets the rest.
	promptContextShare = 40

	// messageOverheadTokens is what the chat template adds around each message,
	// and toolsTemplateOverheadTokens what it adds around the tool definitions
	// (measured: 831 rendered against 630 for the bare JSON).
	messageOverheadTokens       = 6
	toolsTemplateOverheadTokens = 250

	// minToolResultTokens is the smallest allowance worth keeping a tool result
	// for: below it the result is replaced by a note instead of a stub.
	minToolResultTokens = 32

	toolResultTruncatedMarker = " [result truncated]"
	toolResultOmittedNotice   = "Error: this tool result was left out because the context window is full. Answer with what you already have."
)

// budgets are the token limits one chat request is built with.
type budgets struct {
	// contextTokens is the window of one request; 0 means unknown.
	contextTokens int
	// answerTokens is sent as max_tokens; 0 means no cap.
	answerTokens        int
	promptContextTokens int
	historyTokens       int
	historyMessages     int
	toolResultTokens    int
	// thinking lets the model reason before it answers.
	thinking bool
	// The sampling settings sent with the chat requests. Zero is left out of the
	// request, so the server's own value stays in force (what tests that build an
	// Orchestrator without the configuration get).
	temperature     float32
	topP            float32
	presencePenalty float32
}

// legacyBudgets are the built-in budgets for an unknown window.
func legacyBudgets() budgets {
	return budgets{
		promptContextTokens: defaultPromptContextTokens,
		historyTokens:       defaultHistoryTokens,
		historyMessages:     defaultHistoryMessages,
		toolResultTokens:    defaultToolResultTokens,
		thinking:            true,
	}
}

// newBudgets builds the budgets from the configuration. A budget set in the
// configuration wins; the others are derived from the context window when it is
// known and fall back to the built-in values otherwise.
func newBudgets(cfg *config.Config) budgets {
	b := budgets{
		contextTokens:       cfg.LLMContextTokens,
		answerTokens:        cfg.LLMAnswerTokens,
		promptContextTokens: cfg.LLMPromptContextTokens,
		historyTokens:       cfg.LLMHistoryTokens,
		historyMessages:     cfg.LLMHistoryMessages,
		toolResultTokens:    cfg.LLMToolResultTokens,
		thinking:            cfg.LLMEnableThinking,
		temperature:         cfg.LLMTemperature,
		topP:                cfg.LLMTopP,
		presencePenalty:     cfg.LLMPresencePenalty,
	}

	if b.contextTokens > 0 {
		if b.answerTokens == 0 {
			b.answerTokens = defaultAnswerTokens
			if b.thinking {
				b.answerTokens = defaultThinkingAnswerTokens
			}
		}

		// Room for the dynamic prompt and the history once everything with a
		// fixed size is paid for, including 5 % for the estimates being rough.
		room := b.contextTokens - b.answerTokens - fixedPromptTokens - maxSummaryTokens - toolFloorTokens - b.contextTokens/20
		if room < minDerivedRoom {
			room = minDerivedRoom
		}
		promptShare := room * promptContextShare / 100

		if b.promptContextTokens == 0 {
			b.promptContextTokens = promptShare
		}
		if b.historyTokens == 0 {
			b.historyTokens = room - promptShare
		}
		if b.historyMessages == 0 {
			b.historyMessages = derivedHistoryMessages
		}
		if b.toolResultTokens == 0 {
			b.toolResultTokens = min(defaultToolResultTokens, b.contextTokens/4)
		}
	}

	legacy := legacyBudgets()
	if b.promptContextTokens == 0 {
		b.promptContextTokens = legacy.promptContextTokens
	}
	if b.historyTokens == 0 {
		b.historyTokens = legacy.historyTokens
	}
	if b.historyMessages == 0 {
		b.historyMessages = legacy.historyMessages
	}
	if b.toolResultTokens == 0 {
		b.toolResultTokens = legacy.toolResultTokens
	}
	return b
}

// worstCaseTokens is the largest request the budgets allow when every section
// is full and the answer uses all of its reserve.
func (b budgets) worstCaseTokens() int {
	return fixedPromptTokens + maxSummaryTokens + b.promptContextTokens + b.historyTokens + b.answerTokens
}

// log reports the effective budgets, and warns when they cannot all fit.
func (b budgets) log() {
	slog.Info("LLM token budgets",
		"context_tokens", b.contextTokens,
		"answer_tokens", b.answerTokens,
		"prompt_context_tokens", b.promptContextTokens,
		"history_tokens", b.historyTokens,
		"history_messages", b.historyMessages,
		"tool_result_tokens", b.toolResultTokens,
		"thinking", b.thinking)

	if b.contextTokens > 0 && b.worstCaseTokens() > b.contextTokens {
		slog.Warn("LLM token budgets can add up to more than the context window; tool results will be cut to what is left",
			"worst_case_tokens", b.worstCaseTokens(), "context_tokens", b.contextTokens)
	}
}

// estimatePromptTokens approximates how many tokens a request occupies:
// message texts, tool call arguments, the chat template's per-message markers
// and the tool definitions.
func estimatePromptTokens(messages []openai.ChatCompletionMessage, tools []openai.Tool) int {
	total := 0
	for _, m := range messages {
		total += messageOverheadTokens + estimateTokens(m.Content)
		for _, call := range m.ToolCalls {
			total += messageOverheadTokens + estimateTokens(call.Function.Name) + estimateTokens(call.Function.Arguments)
		}
	}

	if len(tools) > 0 {
		if raw, err := json.Marshal(tools); err == nil {
			total += estimateTokens(string(raw)) + toolsTemplateOverheadTokens
		}
	}
	return total
}

// toolBudget is how many tokens the results of the next tool round may use
// together: the configured cap, or less when the prompt already fills most of
// the window. Without a known window only the cap applies.
func (b budgets) toolBudget(messages []openai.ChatCompletionMessage, tools []openai.Tool) int {
	budget := b.toolResultTokens
	if b.contextTokens <= 0 {
		return budget
	}

	room := b.contextTokens - estimatePromptTokens(messages, tools) - b.answerTokens - b.contextTokens/20
	if room < budget {
		budget = room
	}
	return max(budget, 0)
}

// allocateTokens splits total between results of the given sizes: small results
// keep everything they need and the large ones share what is left evenly.
func allocateTokens(sizes []int, total int) []int {
	order := make([]int, len(sizes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return sizes[order[a]] < sizes[order[b]] })

	allowed := make([]int, len(sizes))
	remaining := max(total, 0)
	for pos, idx := range order {
		share := remaining / (len(order) - pos)
		allowed[idx] = min(sizes[idx], share)
		remaining -= allowed[idx]
	}
	return allowed
}

// fitToolResult shortens a tool result to at most maxTokens, saying so, or
// replaces it with a note when there is no room worth keeping it for.
func fitToolResult(content string, maxTokens int) string {
	if estimateTokens(content) <= maxTokens {
		return content
	}
	if maxTokens < minToolResultTokens {
		return toolResultOmittedNotice
	}
	return truncateTokens(content, maxTokens-estimateTokens(toolResultTruncatedMarker)) + toolResultTruncatedMarker
}

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

const (
	// maxToolRounds bounds how many tool rounds one turn may run. Each round is
	// a full completion round trip, so an unbounded loop lets a confused model
	// keep the request alive until the client times out.
	maxToolRounds = 2

	// maxToolCalls bounds the number of tool calls per round. The indexes come
	// from the model, so this also caps the slice built while merging deltas.
	maxToolCalls = 8

	// maxLoggedToolArgsRunes caps how much of an unusable argument payload is
	// written to the log. The full value is model output and can be huge, while
	// a short prefix is enough to see what went wrong.
	maxLoggedToolArgsRunes = 256
)

// deltaFunc receives streamed content and reasoning chunks. Returning an error
// aborts the stream.
type deltaFunc func(content, reasoning string) error

// streamResult is the accumulated output of a single streaming completion.
type streamResult struct {
	content   string
	reasoning string
	toolCalls []openai.ToolCall
}

// chatRequest builds one chat completion request with the answer limit, the
// thinking switch and the sampling settings of the configuration.
func (o *Orchestrator) chatRequest(messages []openai.ChatCompletionMessage, tools []openai.Tool) openai.ChatCompletionRequest {
	limits := o.limits()
	return openai.ChatCompletionRequest{
		Messages:  messages,
		Stream:    true,
		Tools:     tools,
		MaxTokens: limits.answerTokens,
		// A zero value is left out of the request (omitempty), so the server's own
		// setting stays in force.
		Temperature:     limits.temperature,
		TopP:            limits.topP,
		PresencePenalty: limits.presencePenalty,
		// Sent explicitly so the behaviour does not depend on how the model
		// server was started.
		ChatTemplateKwargs: map[string]any{"enable_thinking": limits.thinking},
	}
}

// logUsage reports what a finished completion really cost next to the estimate
// the budgets were planned with, so the estimates can be checked and the
// budgets tuned. usage is nil when the server did not report any.
func (o *Orchestrator) logUsage(req openai.ChatCompletionRequest, usage *openai.Usage, finishReason openai.FinishReason) {
	limits := o.limits()
	attrs := []any{
		"estimated_prompt_tokens", estimatePromptTokens(req.Messages, req.Tools),
		"context_tokens", limits.contextTokens,
		"answer_budget", limits.answerTokens,
		"finish_reason", string(finishReason),
	}
	if usage != nil {
		attrs = append(attrs,
			"prompt_tokens", usage.PromptTokens,
			"completion_tokens", usage.CompletionTokens)
	}
	slog.Info("LLM completion finished", attrs...)

	if finishReason == openai.FinishReasonLength {
		slog.Warn("LLM answer was cut at the token limit; raise LLM_ANSWER_TOKENS or turn thinking off (LLM_ENABLE_THINKING=false)",
			"answer_budget", limits.answerTokens)
	}
}

// streamOnce runs one streaming completion and collects text, reasoning and tool
// call deltas. Provider, network and callback errors are all returned: a
// truncated answer must never look like a finished one.
func (o *Orchestrator) streamOnce(ctx context.Context, req openai.ChatCompletionRequest, onDelta deltaFunc) (streamResult, error) {
	stream, err := o.llmClient.ChatStream(ctx, req)
	if err != nil {
		return streamResult{}, err
	}
	defer stream.Close()

	var content, reasoning strings.Builder
	var toolCalls []openai.ToolCall
	var usage *openai.Usage
	var finishReason openai.FinishReason

	snapshot := func() streamResult {
		return streamResult{
			content:   content.String(),
			reasoning: reasoning.String(),
			toolCalls: toolCalls,
		}
	}

	for {
		resp, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			o.logUsage(req, usage, finishReason)
			break
		}
		if recvErr != nil {
			return snapshot(), recvErr
		}
		// The usage arrives in a final chunk that has no choices.
		if resp.Usage != nil {
			usage = resp.Usage
		}
		if len(resp.Choices) == 0 {
			continue
		}
		if reason := resp.Choices[0].FinishReason; reason != "" {
			finishReason = reason
		}

		delta := resp.Choices[0].Delta
		if delta.ReasoningContent != "" {
			reasoning.WriteString(delta.ReasoningContent)
		}

		mergeToolCallDeltas(&toolCalls, delta.ToolCalls)

		chunk := delta.Content
		if chunk != "" {
			content.WriteString(chunk)
		}

		if onDelta != nil && (chunk != "" || delta.ReasoningContent != "") {
			if cbErr := onDelta(chunk, delta.ReasoningContent); cbErr != nil {
				return snapshot(), cbErr
			}
		}
	}

	return snapshot(), nil
}

// mergeToolCallDeltas folds streamed tool call fragments into calls. Both the
// slice size and the number of calls are bounded because the indexes are chosen
// by the model: without the guard a single delta claiming index 1e9 would
// allocate gigabytes.
func mergeToolCallDeltas(calls *[]openai.ToolCall, deltas []openai.ToolCall) {
	for _, delta := range deltas {
		if delta.Index == nil {
			continue
		}

		idx := *delta.Index
		if idx < 0 || idx >= maxToolCalls {
			slog.Warn("ignoring tool call with out-of-range index", "index", idx, "max", maxToolCalls)
			continue
		}

		for len(*calls) <= idx {
			*calls = append(*calls, openai.ToolCall{})
		}

		call := &(*calls)[idx]
		if delta.ID != "" {
			call.ID = delta.ID
		}
		if delta.Type != "" {
			call.Type = delta.Type
		}
		if delta.Function.Name != "" {
			call.Function.Name = delta.Function.Name
		}
		if delta.Function.Arguments != "" {
			call.Function.Arguments += delta.Function.Arguments
		}
	}
}

// executeToolCalls runs every requested tool and returns the tool messages the
// API expects: one per call, in the order the model asked for them. Together the
// results may use at most budgetTokens; larger ones are cut so that every call
// still gets an answer.
func (o *Orchestrator) executeToolCalls(ctx context.Context, calls []openai.ToolCall, budgetTokens int) []openai.ChatCompletionMessage {
	contents := make([]string, len(calls))

	for i, tc := range calls {
		var content string

		if tc.Function.Name == "" {
			// The conversation still needs a tool message for this call ID,
			// otherwise the next request is rejected as malformed.
			slog.Warn("tool call without a name", "id", tc.ID)
			content = "Error: tool name missing"
		} else {
			args := map[string]interface{}{}
			var parseErr error
			rawArgs := strings.TrimSpace(tc.Function.Arguments)
			if rawArgs != "" {
				if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
					parseErr = fmt.Errorf("arguments are not valid JSON: %w", err)
				} else if args == nil {
					parseErr = errors.New("arguments are not a JSON object")
				}
			}

			if parseErr != nil {
				// A half parsed map would make the tool look for arguments that
				// are not there and report a misleading error, so the model is
				// asked to repeat the call instead.
				slog.Warn("tool arguments are unusable",
					"tool", tc.Function.Name,
					"err", parseErr,
					"raw", truncateRunes(rawArgs, maxLoggedToolArgsRunes))
				content = fmt.Sprintf("Error: %v. Call %s again with a JSON object that matches its schema.",
					parseErr, tc.Function.Name)
			} else {
				res, err := o.tools.ExecuteTool(ctx, tc.Function.Name, args)
				switch {
				case err != nil:
					var argErr *toolArgumentError
					if errors.As(err, &argErr) {
						// The model got the call wrong, the server did not fail;
						// the reason goes back to the model as tool output.
						slog.Warn("tool call rejected", "tool", tc.Function.Name, "err", err)
					} else {
						slog.Error("tool execution failed", "tool", tc.Function.Name, "err", err)
					}
					content = fmt.Sprintf("Error: %v", err)
				default:
					resBytes, marshalErr := json.Marshal(res)
					if marshalErr != nil {
						slog.Error("failed to marshal tool result", "tool", tc.Function.Name, "err", marshalErr)
						content = fmt.Sprintf("Error: %v", marshalErr)
						break
					}
					content = string(resBytes)
					slog.Info("tool execution succeeded", "tool", tc.Function.Name, "result_bytes", len(resBytes))
				}
			}
		}

		contents[i] = content
	}

	sizes := make([]int, len(contents))
	for i, content := range contents {
		sizes[i] = estimateTokens(content)
	}
	allowed := allocateTokens(sizes, budgetTokens)

	messages := make([]openai.ChatCompletionMessage, 0, len(calls))
	for i, tc := range calls {
		content := contents[i]
		if sizes[i] > allowed[i] {
			slog.Warn("tool result cut to fit the context window",
				"tool", tc.Function.Name, "result_tokens", sizes[i], "allowed_tokens", allowed[i], "round_budget", budgetTokens)
			content = fitToolResult(content, allowed[i])
		}

		messages = append(messages, openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			Content:    content,
			ToolCallID: tc.ID,
		})
	}

	return messages
}

// generateResponse handles the LLM generation and tool calling logic. Unlike
// streamResponse it does not write SSE events; it accumulates the answer and
// returns it, so streaming and regenerate share the same code path.
func (o *Orchestrator) generateResponse(
	ctx context.Context,
	finalMessages []openai.ChatCompletionMessage,
	tools []openai.Tool,
	onDelta deltaFunc,
) (string, string, []openai.ToolCall, error) {
	// The caller's slice is copied because tool rounds append to it and the
	// caller may still use its own view (e.g. for tracing).
	messages := slices.Clone(finalMessages)

	var assistantContent, reasoningContent strings.Builder
	var executed []openai.ToolCall

	toolRounds := 0
	for {
		result, err := o.streamOnce(ctx, o.chatRequest(messages, tools), onDelta)
		assistantContent.WriteString(result.content)
		reasoningContent.WriteString(result.reasoning)
		if err != nil {
			return assistantContent.String(), reasoningContent.String(), executed, err
		}

		if len(result.toolCalls) == 0 {
			return assistantContent.String(), reasoningContent.String(), executed, nil
		}

		if toolRounds >= maxToolRounds {
			slog.Warn("tool round limit reached, answering without further tools",
				"rounds", toolRounds,
				"pending_tool_calls", len(result.toolCalls))
			break
		}

		toolRounds++
		executed = append(executed, result.toolCalls...)
		messages = append(messages, openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			Content:   result.content,
			ToolCalls: result.toolCalls,
		})
		// The room left for the results is measured on the conversation as it
		// stands now, including the tool calls just appended and the results of
		// an earlier round.
		toolBudget := o.limits().toolBudget(messages, tools)
		messages = append(messages, o.executeToolCalls(ctx, result.toolCalls, toolBudget)...)
	}

	// The model asked for tools but never produced prose. Ask again with tools
	// disabled so it has to answer in text. The tool results are already part of
	// the conversation, so nothing is repeated here: an earlier version appended
	// them a second time, which doubled the prompt of the final call.
	if strings.TrimSpace(assistantContent.String()) == "" {
		result, err := o.streamOnce(ctx, o.chatRequest(messages, nil), onDelta)
		assistantContent.WriteString(result.content)
		reasoningContent.WriteString(result.reasoning)
		if err != nil {
			return assistantContent.String(), reasoningContent.String(), executed, err
		}
	}

	return assistantContent.String(), reasoningContent.String(), executed, nil
}

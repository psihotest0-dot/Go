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
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
)

// generateResponse handles the LLM generation and tool calling logic, but unlike streamResponse, it
// does not write SSE events to an HTTP ResponseWriter. Instead, it accumulates the result and returns it.
func (o *Orchestrator) generateResponse(
	ctx context.Context,
	finalMessages []openai.ChatCompletionMessage,
	tools []openai.Tool,
	sources []api.SourceInfo,
	onDelta func(content, reasoning string) error,
) (string, string, []openai.ToolCall, error) {
	chatReq := openai.ChatCompletionRequest{
		Messages: finalMessages,
		Stream:   true,
		Tools:    tools,
	}

	stream, err := o.llmClient.ChatStream(ctx, chatReq)
	if err != nil {
		slog.Error("llm stream failed", "err", err)
		return "", "", nil, err
	}
	defer stream.Close()

	var fullAssistantMessage strings.Builder
	var fullReasoningMessage strings.Builder
	var toolCalls []openai.ToolCall

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			slog.Error("stream reading failed", "err", err)
			return "", "", nil, err
		}

		if len(resp.Choices) > 0 {
			delta := resp.Choices[0].Delta

			reasoningChunk := delta.ReasoningContent
			if reasoningChunk != "" {
				fullReasoningMessage.WriteString(reasoningChunk)
			}

			if len(delta.ToolCalls) > 0 {
				for _, tc := range delta.ToolCalls {
					if tc.Index != nil {
						idx := *tc.Index
						for len(toolCalls) <= idx {
							toolCalls = append(toolCalls, openai.ToolCall{})
						}

						if tc.ID != "" {
							toolCalls[idx].ID = tc.ID
						}
						if tc.Type != "" {
							toolCalls[idx].Type = tc.Type
						}
						if tc.Function.Name != "" {
							toolCalls[idx].Function.Name = tc.Function.Name
						}
						if tc.Function.Arguments != "" {
							toolCalls[idx].Function.Arguments += tc.Function.Arguments
						}
					}
				}
			}

			chunk := delta.Content
			if chunk != "" {
				fullAssistantMessage.WriteString(chunk)
			}

			if onDelta != nil && (chunk != "" || reasoningChunk != "") {
				if err := onDelta(chunk, reasoningChunk); err != nil {
					slog.Error("onDelta callback failed", "err", err)
					return "", "", nil, err
				}
			}
		}
	}

	if len(toolCalls) > 0 {
		finalMessages = append(finalMessages, openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			Content:   fullAssistantMessage.String(),
			ToolCalls: toolCalls,
		})

		for _, tc := range toolCalls {
			if tc.Function.Name != "" {
				var args map[string]interface{}
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				}

				res, err := o.tools.ExecuteTool(ctx, tc.Function.Name, args)

				var contentStr string
				if err != nil {
					slog.Error("tool execution failed", "tool", tc.Function.Name, "err", err)
					contentStr = fmt.Sprintf("Error: %v", err)
				} else {
					slog.Info("tool execution succeeded", "tool", tc.Function.Name, "result", res)
					resBytes, _ := json.Marshal(res)
					contentStr = string(resBytes)

					if utf8.RuneCountInString(contentStr) > 8192 {
						runes := []rune(contentStr)
						contentStr = string(runes[:8192]) + "... [TRUNCATED]"
					}
				}

				finalMessages = append(finalMessages, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					Content:    contentStr,
					ToolCallID: tc.ID,
				})
			}
		}

		chatReq2 := openai.ChatCompletionRequest{
			Messages: finalMessages,
			Stream:   true,
			Tools:    tools,
		}

		stream2, err := o.llmClient.ChatStream(ctx, chatReq2)
		if err != nil {
			slog.Error("llm second stream failed", "err", err)
			return fullAssistantMessage.String(), fullReasoningMessage.String(), toolCalls, err
		}

		var secondCallText strings.Builder

		func() {
			defer stream2.Close()
			for {
				resp2, err := stream2.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					break
				}
				if len(resp2.Choices) > 0 {
					reasoningChunk := resp2.Choices[0].Delta.ReasoningContent
					if reasoningChunk != "" {
						fullReasoningMessage.WriteString(reasoningChunk)
					}

					chunk := resp2.Choices[0].Delta.Content
					if chunk != "" {
						secondCallText.WriteString(chunk)
						fullAssistantMessage.WriteString(chunk)
					}

					if onDelta != nil && (chunk != "" || reasoningChunk != "") {
						if err := onDelta(chunk, reasoningChunk); err != nil {
							break // error in callback, stop processing
						}
					}
				}
			}
		}()

		if strings.TrimSpace(secondCallText.String()) == "" {
			var toolResponses strings.Builder
			for _, tcMsg := range finalMessages {
				if tcMsg.Role == openai.ChatMessageRoleTool {
					toolResponses.WriteString(tcMsg.Content)
					toolResponses.WriteString("\n\n")
				}
			}

			fallbackFinalMessages := slices.Clone(finalMessages)
			fallbackFinalMessages = append(fallbackFinalMessages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: "Отговори на въпроса на потребителя въз основа на намерените документи по-долу.\n\n" + toolResponses.String(),
			})

			chatReqFallbackTool := chatReq2
			chatReqFallbackTool.Messages = fallbackFinalMessages
			chatReqFallbackTool.Tools = nil

			stream3, err := o.llmClient.ChatStream(ctx, chatReqFallbackTool)
			if err != nil {
				slog.Error("llm third stream failed", "err", err)
				return fullAssistantMessage.String(), fullReasoningMessage.String(), toolCalls, err
			}
			defer stream3.Close()

			for {
				resp3, err := stream3.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					break
				}
				if len(resp3.Choices) > 0 {
					reasoningChunk := resp3.Choices[0].Delta.ReasoningContent
					if reasoningChunk != "" {
						fullReasoningMessage.WriteString(reasoningChunk)
					}

					chunk := resp3.Choices[0].Delta.Content
					if chunk != "" {
						fullAssistantMessage.WriteString(chunk)
					}

					if onDelta != nil && (chunk != "" || reasoningChunk != "") {
						if err := onDelta(chunk, reasoningChunk); err != nil {
							break // error in callback, stop processing
						}
					}
				}
			}
		}
	}

	return fullAssistantMessage.String(), fullReasoningMessage.String(), toolCalls, nil
}

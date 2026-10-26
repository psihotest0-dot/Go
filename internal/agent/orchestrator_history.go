package agent

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/prompts"
)

func pruneHistory(historyMessages []openai.ChatCompletionMessage) ([]openai.ChatCompletionMessage, []openai.ChatCompletionMessage, int) {
	var oldMessages []openai.ChatCompletionMessage
	var reversedKeep []openai.ChatCompletionMessage
	currentChars := 0

	for i := len(historyMessages) - 1; i >= 0; i-- {
		msgLen := utf8.RuneCountInString(historyMessages[i].Content)

		if i != len(historyMessages)-1 {
			if (currentChars+msgLen > maxContextChars && len(reversedKeep) > 0) || len(reversedKeep) >= maxHistoryMessages {
				oldMessages = historyMessages[:i+1]
				break
			}
		}
		reversedKeep = append(reversedKeep, historyMessages[i])
		currentChars += msgLen
	}

	var finalHistory []openai.ChatCompletionMessage
	for i := len(reversedKeep) - 1; i >= 0; i-- {
		finalHistory = append(finalHistory, reversedKeep[i])
	}

	droppedMessagesCount := len(oldMessages)
	return finalHistory, oldMessages, droppedMessagesCount
}

func (o *Orchestrator) rollingSummary(ctx context.Context, sessionID string, oldMessages []openai.ChatCompletionMessage, droppedMessagesCount int) string {
	v, _ := o.sessionSummaries.LoadOrStore(sessionID, &sessionSummary{})
	summaryState := v.(*sessionSummary)

	summaryState.mu.Lock()
	rollingSummary := summaryState.text
	lastDropCount := summaryState.lastDropCount
	isGenerating := summaryState.isGenerating

	shouldGenerate := false
	if droppedMessagesCount > 0 && (droppedMessagesCount-lastDropCount) >= 6 && !isGenerating {
		summaryState.isGenerating = true
		shouldGenerate = true
	}
	summaryState.mu.Unlock()

	if shouldGenerate {
		var newOlds []openai.ChatCompletionMessage
		if droppedMessagesCount > lastDropCount {
			newOlds = oldMessages[lastDropCount:]
		}

		go func(sessID string, olds []openai.ChatCompletionMessage, dropCount int, previousSummary string) {
			defer func() {
				v, ok := o.sessionSummaries.Load(sessID)
				if ok {
					state := v.(*sessionSummary)
					state.mu.Lock()
					state.isGenerating = false
					state.mu.Unlock()
				}
			}()

			if len(olds) == 0 {
				return
			}

			var oldTexts []string
			for _, m := range olds {
				oldTexts = append(oldTexts, string(m.Role)+": "+m.Content)
			}

			// Render rolling summary prompt
			summaryPrompt, err := prompts.GlobalManager.Render("rolling_summary.tmpl", map[string]interface{}{
				"PreviousSummary": previousSummary,
				"MessagesText":    strings.Join(oldTexts, "\n"),
			})
			if err == nil {
				summaryReq := openai.ChatCompletionRequest{
					Messages: []openai.ChatCompletionMessage{
						{
							Role:    openai.ChatMessageRoleUser,
							Content: summaryPrompt,
						},
					},
					MaxTokens:   300,
					Temperature: 0.3,
				}
				summaryResp, sumErr := o.llmClient.Chat(context.Background(), summaryReq)
				if sumErr == nil && len(summaryResp.Choices) > 0 {
					if v, ok := o.sessionSummaries.Load(sessID); ok {
						state := v.(*sessionSummary)
						state.mu.Lock()
						state.text = summaryResp.Choices[0].Message.Content
						state.lastDropCount = dropCount
						state.mu.Unlock()
					}
				} else {
					slog.Error("failed to generate rolling summary", "err", sumErr)
				}
			} else {
				slog.Error("failed to render rolling summary prompt", "err", err)
			}
		}(sessionID, newOlds, droppedMessagesCount, rollingSummary)
	}

	return rollingSummary
}

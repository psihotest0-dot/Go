package agent

import (
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// dropTrailingAssistantMessages removes assistant replies from the end of the
// history. Regenerating keeps the previous answer in the database until a new
// one is ready, so it has to be dropped from the prompt: otherwise the model is
// asked to answer a question it has already answered, with that answer in view.
func dropTrailingAssistantMessages(historyMessages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	end := len(historyMessages)
	for end > 0 && historyMessages[end-1].Role == openai.ChatMessageRoleAssistant {
		end--
	}
	if end == len(historyMessages) {
		return historyMessages
	}
	return historyMessages[:end]
}

// pruneHistory keeps the newest messages that fit maxTokens (estimated) and
// maxMessages. The newest message is always kept, however large. What is left
// out is not lost: the rolling summary picks it up (see rollingSummary).
func pruneHistory(historyMessages []openai.ChatCompletionMessage, maxTokens, maxMessages int) []openai.ChatCompletionMessage {
	var reversedKeep []openai.ChatCompletionMessage
	currentTokens := 0

	for i := len(historyMessages) - 1; i >= 0; i-- {
		msgTokens := estimateTokens(historyMessages[i].Content)

		if i != len(historyMessages)-1 {
			if (currentTokens+msgTokens > maxTokens && len(reversedKeep) > 0) || len(reversedKeep) >= maxMessages {
				break
			}
		}
		reversedKeep = append(reversedKeep, historyMessages[i])
		currentTokens += msgTokens
	}

	finalHistory := make([]openai.ChatCompletionMessage, 0, len(reversedKeep))
	for i := len(reversedKeep) - 1; i >= 0; i-- {
		finalHistory = append(finalHistory, reversedKeep[i])
	}
	return finalHistory
}

// windowStart is the time of the oldest message of the prompt after pruning:
// the history is cut to its newest keptCount messages, so that is the keptCount-th
// from the end. It is the zero time when the history carries no timestamps.
func (c chatContext) windowStart(keptCount int) time.Time {
	if keptCount <= 0 || len(c.HistoryAt) != len(c.History) || keptCount > len(c.HistoryAt) {
		return time.Time{}
	}
	return c.HistoryAt[len(c.HistoryAt)-keptCount]
}

package llm

import (
	"context"

	openai "github.com/sashabaranov/go-openai"
)

// ContextManager is a decorator for LLMClient that manages history pruning
// and sliding window for context.
type ContextManager struct {
	client      LLMClient
	maxMessages int
}

// NewContextManager creates a new ContextManager.
func NewContextManager(client LLMClient, maxMessages int) *ContextManager {
	return &ContextManager{
		client:      client,
		maxMessages: maxMessages,
	}
}

// pruneHistory processes the messages to enforce a max message limit
// while preserving the system prompt (if present).
func (cm *ContextManager) pruneHistory(messages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	if cm.maxMessages <= 0 {
		return messages // No limit
	}

	if len(messages) <= cm.maxMessages {
		return messages
	}

	var hasSystemPrompt bool
	var systemPrompt openai.ChatCompletionMessage

	if len(messages) > 0 && messages[0].Role == openai.ChatMessageRoleSystem {
		hasSystemPrompt = true
		systemPrompt = messages[0]
	}

	// Calculate how many messages we can keep (excluding the system prompt)
	keepCount := cm.maxMessages
	if hasSystemPrompt {
		keepCount--
	}

	if keepCount <= 0 {
		if hasSystemPrompt {
			return []openai.ChatCompletionMessage{systemPrompt}
		}
		return []openai.ChatCompletionMessage{}
	}

	prunedMessages := make([]openai.ChatCompletionMessage, 0, cm.maxMessages)
	if hasSystemPrompt {
		prunedMessages = append(prunedMessages, systemPrompt)
	}

	// Append the latest 'keepCount' messages
	startIndex := len(messages) - keepCount
	prunedMessages = append(prunedMessages, messages[startIndex:]...)

	return prunedMessages
}

func (cm *ContextManager) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	req.Messages = cm.pruneHistory(req.Messages)
	return cm.client.Chat(ctx, req)
}

func (cm *ContextManager) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (*openai.ChatCompletionStream, error) {
	req.Messages = cm.pruneHistory(req.Messages)
	return cm.client.ChatStream(ctx, req)
}

func (cm *ContextManager) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error) {
	req.Messages = cm.pruneHistory(req.Messages)
	return cm.client.ChatWithJSONSchema(ctx, req, schema)
}

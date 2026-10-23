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

	// Traverse backwards to select which messages to keep
	keptMessages := make([]openai.ChatCompletionMessage, 0)
	i := len(messages) - 1

	for i >= 0 && (len(keptMessages) < keepCount || (messages[i].Role == openai.ChatMessageRoleTool || (messages[i].Role == openai.ChatMessageRoleAssistant && len(messages[i].ToolCalls) > 0))) {
		// Ignore the system prompt during backward traversal as it's handled separately
		if i == 0 && hasSystemPrompt {
			break
		}

		msg := messages[i]

		if msg.Role == openai.ChatMessageRoleTool {
			// We encountered a tool message. We MUST greedily consume all adjacent tool messages
			// and their parent assistant message containing ToolCalls to avoid breaking OpenAI API validation.

			// Find all contiguous preceding tool messages
			toolBlockEnd := i
			for i >= 0 && messages[i].Role == openai.ChatMessageRoleTool {
				if i == 0 && hasSystemPrompt {
					break
				}
				i--
			}

			// Now i is at the message preceding the tool block.
			// It MUST be the assistant message with tool calls. Let's include it.
			var block []openai.ChatCompletionMessage
			if i >= 0 && messages[i].Role == openai.ChatMessageRoleAssistant && len(messages[i].ToolCalls) > 0 {
				// Include the assistant message
				block = append(block, messages[i])

				// Include the tool block
				for j := i + 1; j <= toolBlockEnd; j++ {
					block = append(block, messages[j])
				}
				i-- // move past the assistant message
			} else {
				// Fallback if structure is malformed: just include the tool block we found
				for j := i + 1; j <= toolBlockEnd; j++ {
					block = append(block, messages[j])
				}
				// i is already decremented
			}
			keptMessages = append(block, keptMessages...)
		} else {
			// Normal message
			keptMessages = append([]openai.ChatCompletionMessage{msg}, keptMessages...)
			i--
		}
	}

	prunedMessages := make([]openai.ChatCompletionMessage, 0, len(keptMessages)+1)
	if hasSystemPrompt {
		prunedMessages = append(prunedMessages, systemPrompt)
	}
	prunedMessages = append(prunedMessages, keptMessages...)

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

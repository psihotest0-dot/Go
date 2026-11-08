package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
)

// preparedTurn is everything the model call of one chat turn needs. prepareTurn builds it
// for the three ways a turn starts (the chat stream, regenerate and Telegram), which
// differ only in what comes before (the HTTP request, the user message) and after (the
// stream, replacing the old answer, the returned answer).
type preparedTurn struct {
	// ctx carries the state of the turn (see withTurn); the model call and the tools run with it.
	ctx  context.Context
	turn *turnState
	// messages are the system prompt followed by the pruned history.
	messages []openai.ChatCompletionMessage
	tools    []openai.Tool
	// sources are the passages the prompt itself contains, for the sources the client shows.
	sources []api.SourceInfo
}

// prepareTurn builds the prompt of one turn: the files attached to the conversation, the
// context (rules, memories, concepts, history), the turn state, the pruned history, the
// rolling summary and the system prompt, with the trace steps of each stage.
//
// The user message of the turn must already be stored: the history is read back from the
// database. With regenerate the answer being replaced is left out of the prompt, because
// the model must write a new reply, not read (and copy) the one it replaces.
//
// A failure means that no model call was made; the caller reports it its own way (an HTTP
// error before the stream starts, or a returned error).
func (o *Orchestrator) prepareTurn(ctx context.Context, userID uuid.UUID, req ChatRequest, regenerate bool) (preparedTurn, error) {
	// The room for the files attached to the conversation comes out of the history and
	// the dynamic part of the prompt.
	plan := o.planAttachments(ctx, userID, req.SessionID, o.limits())
	chatCtx, memoryIDs, err := o.loadContextWithLimits(ctx, userID, req, plan.limits)
	chatCtx.Attachments = plan.files
	if err != nil {
		return preparedTurn{}, fmt.Errorf("failed to load context: %w", err)
	}

	if regenerate {
		chatCtx.History = dropTrailingAssistantMessages(chatCtx.History)
		if len(chatCtx.HistoryAt) > len(chatCtx.History) {
			chatCtx.HistoryAt = chatCtx.HistoryAt[:len(chatCtx.History)]
		}
	}

	// The tools of this turn share its state: what untrusted text it has read and
	// which proposals it made (see turnState).
	ctx, turn := withTurn(ctx, req.SessionID, len(chatCtx.Chunks) > 0)
	if plan.hasText() {
		turn.markExternal(sourceAttachments)
	}

	o.trace(ctx, userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history := pruneHistory(chatCtx.History, plan.limits.historyTokens, plan.limits.historyMessages)
	chatCtx.RollingSum = o.rollingSummary(ctx, userID, req.SessionID, chatCtx.windowStart(len(history)))

	systemMessage, err := buildSystemPrompt(chatCtx)
	if err != nil {
		return preparedTurn{}, fmt.Errorf("failed to render system prompt: %w", err)
	}

	messages := make([]openai.ChatCompletionMessage, 0, len(history)+1)
	messages = append(messages, systemMessage)
	messages = append(messages, history...)

	o.trace(ctx, userID, req.SessionID, "prune", map[string]interface{}{
		"total_messages": len(chatCtx.History) + 1,
		"pruned_to":      len(messages),
	})

	return preparedTurn{ctx: ctx, turn: turn, messages: messages, tools: toolDefinitions(), sources: chatCtx.Sources}, nil
}

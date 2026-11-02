package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
)

func (o *Orchestrator) saveUserMessage(ctx context.Context, userID uuid.UUID, sessionID, content string) error {
	_, err := o.db.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID:    userID,
		SessionID: sessionID,
		Role:      "user",
		Content:   content,
		SourceChunkIds: pqtype.NullRawMessage{
			RawMessage: []byte("[]"),
			Valid:      true,
		},
	})
	return err
}

func (o *Orchestrator) persistAssistantMessage(ctx context.Context, userID uuid.UUID, sessionID string, content string, toolCalls []openai.ToolCall, sources []api.SourceInfo) error {
	// Raw model output can carry <think> blocks and tool markup that the live
	// stream hides; they must not end up in the history either.
	contentToSave := sanitizeAssistantText(content)
	if contentToSave == "" {
		if len(toolCalls) == 0 {
			slog.WarnContext(ctx, "assistant answer is empty once hidden reasoning is removed; not stored",
				"session_id", sessionID)
			return nil
		}
		tcBytes, _ := json.Marshal(toolCalls)
		contentToSave = fmt.Sprintf("ToolCalls: %s", string(tcBytes))
	}

	var dbSources []api.DBChunkInfo
	for _, s := range sources {
		dbSources = append(dbSources, api.DBChunkInfo{
			ChunkID: s.ChunkID,
			Score:   s.Score,
		})
	}
	dbSourcesBytes, _ := json.Marshal(dbSources)

	_, err := o.db.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID:    userID,
		SessionID: sessionID,
		Role:      "assistant",
		Content:   contentToSave,
		SourceChunkIds: pqtype.NullRawMessage{
			RawMessage: dbSourcesBytes,
			Valid:      true,
		},
	})
	return err
}

// storeAnswer saves the finished answer. The user already has the text on
// screen, so a failure is not returned, but it must not pass silently: the
// answer is then missing from the history and from every later context.
func (o *Orchestrator) storeAnswer(ctx context.Context, userID uuid.UUID, sessionID string, content string, toolCalls []openai.ToolCall, sources []api.SourceInfo) {
	if err := o.persistAssistantMessage(ctx, userID, sessionID, content, toolCalls, sources); err != nil {
		slog.ErrorContext(ctx, "failed to store the assistant answer", "err", err, "session_id", sessionID)
	}
}

// trace writes a diagnostic step. The trace is for debugging only, so a failure
// is logged as a warning and never stops the answer.
func (o *Orchestrator) trace(ctx context.Context, userID uuid.UUID, sessionID, step string, details interface{}) {
	if err := o.writeTrace(ctx, userID, sessionID, step, details); err != nil {
		slog.WarnContext(ctx, "failed to write the agent trace", "step", step, "err", err, "session_id", sessionID)
	}
}

func (o *Orchestrator) writeTrace(ctx context.Context, userID uuid.UUID, sessionID, step string, details interface{}) error {
	traceDetails, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = o.db.Queries.CreateAgentTrace(ctx, db.CreateAgentTraceParams{
		UserID:    userID,
		SessionID: sessionID,
		Step:      step,
		Details: pqtype.NullRawMessage{
			RawMessage: traceDetails,
			Valid:      true,
		},
	})
	return err
}

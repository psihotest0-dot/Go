package agent

import (
	"context"
	"encoding/json"
	"fmt"

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
	contentToSave := content
	if contentToSave == "" && len(toolCalls) > 0 {
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

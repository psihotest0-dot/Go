package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

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

// persistAssistantMessage writes the answer through q, which is the orchestrator's
// own queries or those of a transaction.
func (o *Orchestrator) persistAssistantMessage(ctx context.Context, q *db.Queries, userID uuid.UUID, sessionID string, content string, toolCalls []openai.ToolCall, sources []api.SourceInfo) error {
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
			ChunkID:   s.ChunkID,
			Score:     s.Score,
			ScoreType: s.ScoreType,
		})
	}
	dbSourcesBytes, _ := json.Marshal(dbSources)

	_, err := q.CreateMessage(ctx, db.CreateMessageParams{
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

// answerSources are the sources a stored answer is based on: those of the prompt,
// then the passages the tools found during the turn. Without the second part the
// history would lose, after a restart, the books the panel showed while the answer
// was written.
func answerSources(turn *turnState, promptSources []api.SourceInfo) []api.SourceInfo {
	return append(slices.Clone(promptSources), withoutKnownSources(promptSources, turn.foundSources())...)
}

// storeAnswer saves the finished answer. The user already has the text on
// screen, so a failure is not returned, but it must not pass silently: the
// answer is then missing from the history and from every later context.
func (o *Orchestrator) storeAnswer(ctx context.Context, userID uuid.UUID, sessionID string, content string, toolCalls []openai.ToolCall, sources []api.SourceInfo) {
	if err := o.persistAssistantMessage(ctx, o.db.Queries, userID, sessionID, content, toolCalls, sources); err != nil {
		slog.ErrorContext(ctx, "failed to store the assistant answer", "err", err, "session_id", sessionID)
	}
}

// errAnswerAlreadyReplaced means that an answer a regeneration was to replace is no
// longer stored: another regeneration of the same turn got there first.
var errAnswerAlreadyReplaced = errors.New("the answer was already replaced")

// replaceAnswer swaps the answers in replaced (their ids, read when the regeneration
// started) for a new one in one transaction. Done as two separate writes, a reader in
// between saw the question without an answer, and a failed insert after the delete lost
// both answers. By id, a turn sent meanwhile keeps its answer: "the newest answer of the
// session" deleted that one. With no replaced answer (the turn had failed) the new one is
// only added. When one of them is gone already, nothing is written, so the turn does not
// end up with two answers.
func (o *Orchestrator) replaceAnswer(ctx context.Context, userID uuid.UUID, sessionID string, replaced []uuid.UUID, content string, toolCalls []openai.ToolCall, sources []api.SourceInfo) error {
	tx, err := o.db.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// A no-op after Commit.
	defer func() { _ = tx.Rollback() }()

	q := o.db.Queries.WithTx(tx)
	if len(replaced) > 0 {
		deleted, err := q.DeleteAssistantMessages(ctx, db.DeleteAssistantMessagesParams{
			UserID:    userID,
			SessionID: sessionID,
			Ids:       replaced,
		})
		if err != nil {
			return fmt.Errorf("delete the previous answer: %w", err)
		}
		if deleted != int64(len(replaced)) {
			return errAnswerAlreadyReplaced
		}
	}
	if err := o.persistAssistantMessage(ctx, q, userID, sessionID, content, toolCalls, sources); err != nil {
		return fmt.Errorf("store the new answer: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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

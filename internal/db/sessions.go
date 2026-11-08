package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// RetiredSessionPrefix marks the session id of messages that belong to a deleted chat
// but are still waiting for the memory analysis (see DeleteSession). The SQL of
// PurgeRetiredMessages uses the same literal.
const RetiredSessionPrefix = "deleted:"

// DeletedSession says what DeleteSession removed.
type DeletedSession struct {
	Messages    int64 // deleted at once
	Retired     int64 // kept for the memory analysis, removed after it
	Attachments int64
	Traces      int64
	Summaries   int64
}

// DeleteSession removes a chat session of a user: its messages, the rolling summary, the
// agent traces and the attached files, in one transaction. A session that does not exist
// is not an error (all counts are zero).
//
// Memories are kept. A memory only points at the message it came from, and that link is
// ON DELETE SET NULL. A user message the memory analysis has not read yet is not deleted
// but moved to a "deleted:" session (RetireSessionMessages) and purged by the periodic
// memory job once it has been read, so a fact told shortly before the chat was deleted
// still becomes a memory.
func (d *Database) DeleteSession(ctx context.Context, userID uuid.UUID, sessionID string) (DeletedSession, error) {
	var res DeletedSession

	tx, err := d.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	q := d.Queries.WithTx(tx)

	if res.Attachments, err = q.DeleteSessionAttachments(ctx, DeleteSessionAttachmentsParams{UserID: userID, SessionID: sessionID}); err != nil {
		return res, fmt.Errorf("delete attachments: %w", err)
	}
	if res.Traces, err = q.DeleteSessionTraces(ctx, DeleteSessionTracesParams{UserID: userID, SessionID: sessionID}); err != nil {
		return res, fmt.Errorf("delete traces: %w", err)
	}
	if res.Summaries, err = q.DeleteSessionSummary(ctx, DeleteSessionSummaryParams{UserID: userID, SessionID: sessionID}); err != nil {
		return res, fmt.Errorf("delete summary: %w", err)
	}
	if res.Messages, err = q.DeleteSessionMessages(ctx, DeleteSessionMessagesParams{UserID: userID, SessionID: sessionID}); err != nil {
		return res, fmt.Errorf("delete messages: %w", err)
	}
	// Whatever is left of the session is waiting for the memory analysis.
	if res.Retired, err = q.RetireSessionMessages(ctx, RetireSessionMessagesParams{
		UserID: userID, SessionID: sessionID, RetiredSessionID: RetiredSessionPrefix + sessionID,
	}); err != nil {
		return res, fmt.Errorf("retire messages: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DeletedSession{}, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

func deleteSession(t *testing.T, a *api.API, userID uuid.UUID, sessionID string) int {
	t.Helper()
	mux := http.NewServeMux()
	a.RegisterSessionRoutes(mux)
	req := httptest.NewRequest(http.MethodDelete, "/chat/sessions/"+sessionID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, withUser(req, userID))
	return rec.Code
}

// fillSession puts every kind of data a conversation has into session: both roles of
// messages (all of them already read for memories), a summary, a trace and an attached
// file. It returns the first message.
func fillSession(t *testing.T, d *db.Database, userID uuid.UUID, session string) db.Message {
	t.Helper()
	ctx := context.Background()
	var first db.Message
	for i, m := range []struct{ role, text string }{{"user", "hello"}, {"assistant", "hi"}, {"user", "my cat is called Mia"}, {"assistant", "nice"}} {
		msg, err := d.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: userID, SessionID: session, Role: m.role, Content: m.text})
		require.NoError(t, err)
		require.NoError(t, d.Queries.MarkMessageAnalyzed(ctx, db.MarkMessageAnalyzedParams{ID: msg.ID, UserID: userID}))
		if i == 0 {
			first = msg
		}
	}
	require.NoError(t, d.Queries.UpsertSessionSummary(ctx, db.UpsertSessionSummaryParams{
		UserID: userID, SessionID: session, Summary: "talk about a cat", CoveredUntilMessageID: first.ID, CoveredUntilAt: time.Now(),
	}))
	_, err := d.Queries.CreateAgentTrace(ctx, db.CreateAgentTraceParams{UserID: userID, SessionID: session, Step: "plan"})
	require.NoError(t, err)
	_, err = d.Queries.UpsertChatAttachment(ctx, db.UpsertChatAttachmentParams{
		UserID: userID, SessionID: session, Name: "a.go", Content: "package a", ContentHash: strings.Repeat("0", 64), Bytes: 9, LineCount: 1,
	})
	require.NoError(t, err)
	return first
}

func countMessages(t *testing.T, d *db.Database, userID uuid.UUID, session string) int {
	t.Helper()
	msgs, err := d.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: userID, SessionID: session, Limit: 100})
	require.NoError(t, err)
	return len(msgs)
}

func countRows(t *testing.T, d *db.Database, table string, userID uuid.UUID, session string) int {
	t.Helper()
	var n int
	require.NoError(t, d.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM "+table+" WHERE user_id=$1 AND session_id=$2", userID, session).Scan(&n))
	return n
}

func TestDeleteSessionRemovesTheConversationAndKeepsTheMemories(t *testing.T) {
	a, d, userID := setupAPI(t)
	ctx := context.Background()
	first := fillSession(t, d, userID, "chat-a")
	fillSession(t, d, userID, "chat-b")
	mem, err := d.Queries.CreateMemory(ctx, db.CreateMemoryParams{
		UserID: userID, Content: "the cat is called Mia", Status: "active",
		Embedding: pgvector.NewVector(make([]float32, config.VectorDimension)), EmbeddingModel: config.DefaultEmbedModel,
		SourceMessageID: uuid.NullUUID{UUID: first.ID, Valid: true}, Importance: 3, ContentHash: uuid.NewString(),
	})
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, deleteSession(t, a, userID, "chat-a"))

	assert.Zero(t, countMessages(t, d, userID, "chat-a"), "the messages are gone")
	assert.Zero(t, countRows(t, d, "session_summaries", userID, "chat-a"), "the summary is gone")
	assert.Zero(t, countRows(t, d, "agent_traces", userID, "chat-a"), "the traces are gone")
	assert.Zero(t, countRows(t, d, "chat_attachments", userID, "chat-a"), "the attached files are gone")

	kept, err := d.Queries.GetMemoryByID(ctx, db.GetMemoryByIDParams{ID: mem.ID, UserID: userID})
	require.NoError(t, err, "the memory survives its chat")
	assert.Equal(t, "the cat is called Mia", kept.Content)
	assert.False(t, kept.SourceMessageID.Valid, "it only loses the link to the deleted message")

	assert.Equal(t, 4, countMessages(t, d, userID, "chat-b"), "another chat of the same user is untouched")
	assert.Equal(t, 1, countRows(t, d, "chat_attachments", userID, "chat-b"))
	assert.Equal(t, 1, countRows(t, d, "session_summaries", userID, "chat-b"))
	assert.Equal(t, 1, countRows(t, d, "agent_traces", userID, "chat-b"))
}

// The memory job reads user messages every 5 minutes. A fact told just before the chat was
// deleted must still reach it: such a message is moved out of the session, not deleted.
func TestDeleteSessionKeepsUnreadUserMessagesUntilTheyHaveBeenRead(t *testing.T) {
	a, d, userID := setupAPI(t)
	ctx := context.Background()
	retired := db.RetiredSessionPrefix + "chat-a"
	unread, err := d.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: userID, SessionID: "chat-a", Role: "user", Content: "I moved to Varna"})
	require.NoError(t, err)
	reply, err := d.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: userID, SessionID: "chat-a", Role: "assistant", Content: "ok"})
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, deleteSession(t, a, userID, "chat-a"))

	assert.Zero(t, countMessages(t, d, userID, "chat-a"), "nothing is left in the session")
	require.Equal(t, 1, countMessages(t, d, userID, retired), "the unread user message waits under the retired id")
	var n int
	require.NoError(t, d.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE id=$1", reply.ID).Scan(&n))
	assert.Zero(t, n, "the assistant's message is not needed for memories")

	// Still unread: the purge leaves it alone.
	_, err = d.Queries.PurgeRetiredMessages(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, countMessages(t, d, userID, retired), "an unread message is not purged")

	// The memory job reads it; the next purge removes it.
	require.NoError(t, d.Queries.MarkMessageAnalyzed(ctx, db.MarkMessageAnalyzedParams{ID: unread.ID, UserID: userID}))
	purged, err := d.Queries.PurgeRetiredMessages(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, purged, int64(1))
	assert.Zero(t, countMessages(t, d, userID, retired), "after it has been read it is gone for good")
}

func TestDeleteSessionDoesNotKeepMessagesTheJobWillNeverRead(t *testing.T) {
	a, d, userID := setupAPI(t)
	ctx := context.Background()
	for _, c := range []struct {
		content  string
		attempts int
	}{{"   ", 0}, {"failed three times", 3}} {
		m, err := d.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: userID, SessionID: "chat-a", Role: "user", Content: c.content})
		require.NoError(t, err)
		for i := 0; i < c.attempts; i++ {
			require.NoError(t, d.Queries.IncrementAnalysisAttempts(ctx, m.ID))
		}
	}

	require.Equal(t, http.StatusNoContent, deleteSession(t, a, userID, "chat-a"))

	assert.Zero(t, countMessages(t, d, userID, "chat-a")+countMessages(t, d, userID, db.RetiredSessionPrefix+"chat-a"),
		"an empty message and one that failed three times are deleted, not kept")
}

func TestDeleteSessionTouchesOnlyTheCallersData(t *testing.T) {
	a, d, owner := setupAPI(t)
	intruder, err := d.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, d.Pool, intruder.ID) })
	fillSession(t, d, owner, "shared-name")

	require.Equal(t, http.StatusNoContent, deleteSession(t, a, intruder.ID, "shared-name"), "no such session for this user: nothing to do")

	assert.Equal(t, 4, countMessages(t, d, owner, "shared-name"), "the owner's chat with the same id is intact")
	assert.Equal(t, 1, countRows(t, d, "chat_attachments", owner, "shared-name"))
}

func TestDeleteSessionIsIdempotentAndValidatesTheId(t *testing.T) {
	a, _, userID := setupAPI(t)

	assert.Equal(t, http.StatusNoContent, deleteSession(t, a, userID, "never-existed"))
	assert.Equal(t, http.StatusNoContent, deleteSession(t, a, userID, "never-existed"), "twice is fine")
	assert.Equal(t, http.StatusBadRequest, deleteSession(t, a, userID, strings.Repeat("x", 129)))
	assert.Equal(t, http.StatusBadRequest, deleteSession(t, a, userID, "bad%0Aid"), "control characters")
	assert.Equal(t, http.StatusBadRequest, deleteSession(t, a, userID, db.RetiredSessionPrefix+"x"), "the retired namespace is not a chat")

	mux := http.NewServeMux()
	a.RegisterSessionRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/chat/sessions/x", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "without a user")
}

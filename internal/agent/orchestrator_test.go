package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

func TestOrchestratorIntegration(t *testing.T) {
	connString := os.Getenv("DATABASE_URL")
	if connString == "" {
		t.Skip("Пропускане: няма база")
	}

	ctx := context.Background()

	// Run migrations
	err := db.RunMigrations(connString)
	require.NoError(t, err)

	// Connect to DB
	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	defer database.Pool.Close()

	// Clear DB for test
	_, _ = database.Pool.Exec(ctx, "DELETE FROM agent_traces")
	_, _ = database.Pool.Exec(ctx, "DELETE FROM messages")
	_, _ = database.Pool.Exec(ctx, "DELETE FROM memories")
	_, _ = database.Pool.Exec(ctx, "DELETE FROM users")

	// Set up mocks
	mockEmbedder := &llm.MockEmbedder{}
	mockLLM := &llm.MockLLM{
		StreamChunks: []openai.ChatCompletionStreamResponse{
			{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "Hello "}}}},
			{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "World"}}}},
		},
	}
	mockTools := &MockToolRegistry{}

	retrievalService := retrieval.NewRetrievalService(database, mockEmbedder)
	orchestrator := NewOrchestrator(database, retrievalService, mockLLM, mockTools)

	// Create user
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)

	// Create memory (which generates embedding via mock)
	_, err = retrievalService.CreateMemory(ctx, user.ID, "This is a memory", 1, "active", pqtype.NullRawMessage{})
	require.NoError(t, err)

	// Create some old messages to trigger pruning (length limit is 4000)
	sessionID := "sess-123"
	longString := strings.Repeat("a", 2500) // two of these will exceed 4000
	_, err = database.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID:    user.ID,
		SessionID: sessionID,
		Role:      "user",
		Content:   longString,
	})
	require.NoError(t, err)

	_, err = database.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID:    user.ID,
		SessionID: sessionID,
		Role:      "assistant",
		Content:   longString,
	})
	require.NoError(t, err)

	// Perform HTTP request
	reqBody := ChatRequest{
		SessionID: sessionID,
		Content:   "Hello from test",
	}
	reqBytes, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(reqBytes))
	req.Header.Set("X-User-ID", user.ID.String())

	rr := httptest.NewRecorder()
	orchestrator.HandleStreamChat(rr, req)

	// Assertions
	require.Equal(t, http.StatusOK, rr.Code)
	bodyStr := rr.Body.String()
	require.Contains(t, bodyStr, "data: {\"content\":\"Hello \"}")
	require.Contains(t, bodyStr, "data: {\"content\":\"World\"}")
	require.Contains(t, bodyStr, "data: [DONE]")

	// Verify DB messages (User message should be saved, Assistant message should be saved)
	msgs, err := database.Queries.GetMessagesBySession(ctx, db.GetMessagesBySessionParams{
		UserID:    user.ID,
		SessionID: sessionID,
	})
	require.NoError(t, err)
	require.Len(t, msgs, 4) // 2 long strings, 1 new user, 1 new assistant
	require.Equal(t, "Hello World", msgs[3].Content)

	// Verify Traces were created (Search, Prune, StreamEnd)
	var count int
	err = database.Pool.QueryRow(ctx, "SELECT count(*) FROM agent_traces WHERE session_id = $1", sessionID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 3, count) // search, prune, stream_end
}

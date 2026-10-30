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

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/retrieval"
)

func TestOrchestratorIntegration(t *testing.T) {
	connString := os.Getenv("DATABASE_URL")
	if connString == "" {
		t.Skip("Пропускане: няма база")
	}

	// db.RunMigrations resolves the "migrations" directory relative to the
	// working directory, which is the package directory when running
	// `go test`. Move to the module root so the migrations can be found.
	t.Chdir("../..")

	// The orchestrator renders system prompts through prompts.GlobalManager,
	// which is normally initialized by the application bootstrap.
	prompts.Init("prompts")

	ctx := context.Background()

	// Run migrations
	err := db.RunMigrations(connString)
	require.NoError(t, err)

	// Connect to DB
	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	defer func() {
		// Close the database/sql wrapper first, otherwise pgxpool.Close
		// blocks forever waiting for its connections to be released.
		_ = database.SqlDB.Close()
		database.Pool.Close()
	}()

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

	retrievalService := retrieval.NewRetrievalService(database, mockEmbedder, nil)
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
	// The handler reads the parsed user ID from the context, which is normally
	// injected by api.UserMiddleware. The test calls the handler directly, so
	// inject it manually. It must be a uuid.UUID to satisfy the handler's
	// type assertion (a plain string would still yield 401).
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, user.ID))

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
		Limit:     10,
	})
	require.NoError(t, err)
	require.Len(t, msgs, 4) // 2 long strings, 1 new user, 1 new assistant
	// GetMessagesBySession orders by created_at DESC, so the newest messages come first.
	require.Equal(t, "assistant", msgs[0].Role)
	require.Equal(t, "Hello World", msgs[0].Content)
	require.Equal(t, "user", msgs[1].Role)
	require.Equal(t, "Hello from test", msgs[1].Content)

	// Verify Traces were created (Search, Prune, StreamEnd)
	var count int
	err = database.Pool.QueryRow(ctx, "SELECT count(*) FROM agent_traces WHERE session_id = $1", sessionID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 3, count) // search, prune, stream_end
}

func TestShouldSkipRetrieval(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected bool
	}{
		{"Hello!", "Здравей!", true},
		{"Fine thanks", "добре благодаря", true},
		{"How are you?", "как си?", false}, // contains 'как'
		{"What do you think about investments", "какво мислиш за инвестициите", false},
		{"ok", "ok", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldSkipRetrieval(tt.query)
			if result != tt.expected {
				t.Errorf("shouldSkipRetrieval(%q) = %v; want %v", tt.query, result, tt.expected)
			}
		})
	}
}

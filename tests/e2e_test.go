// Package tests holds end-to-end checks that go through a real HTTP server,
// the user middleware and the same routes main.go registers, which the
// handler-level tests in internal/api and internal/agent bypass.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/agent"
	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/proposals"
	"go-monolith/internal/retrieval"

	"github.com/google/uuid"
)

// setupE2E starts the API on a test server and returns it with the id of a
// user created for this test.
func setupE2E(t *testing.T) (*httptest.Server, uuid.UUID) {
	connString := dbtest.ConnString(t)

	// db.RunMigrations and prompts.Init resolve their directories relative to
	// the working directory, which is tests/ here; the module root has both.
	t.Chdir("..")

	previous := prompts.GlobalManager
	prompts.Init("prompts")
	t.Cleanup(func() { prompts.GlobalManager = previous })

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(connString))

	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	// The test owns one user and every row it writes hangs off that user, so
	// it removes only that user. Registered after Close, it runs while the
	// pool is still open. A blanket `DELETE FROM users` is what emptied the
	// library on 2026-10-30.
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })

	embedder := &llm.MockEmbedder{}
	model := &llm.MockLLM{
		StreamChunks: []openai.ChatCompletionStreamResponse{
			{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "Hello "}}}},
			{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "E2E"}}}},
		},
	}

	apiService := api.New(database, embedder)
	retrievalService := retrieval.NewRetrievalService(database, embedder)
	proposalService := proposals.NewService(database, retrievalService)
	toolRegistry := agent.NewDefaultToolRegistry(retrievalService, database, proposalService)
	orchestrator := agent.NewOrchestrator(database, retrievalService, model, toolRegistry)

	// The same routes main.go registers for the endpoints under test.
	protected := http.NewServeMux()
	protected.HandleFunc("/chat/stream", orchestrator.HandleStreamChat)
	protected.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			apiService.CreateMessageHandler(w, r)
		case http.MethodGet:
			apiService.GetMessagesHandler(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	protected.HandleFunc("/memories", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			apiService.CreateMemoryHandler(w, r)
		case http.MethodGet:
			apiService.GetMemoriesHandler(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	protected.HandleFunc("/memories/", apiService.MemoryByIDHandler)
	// Uploads need a job queue; this test only lists files, so none is wired.
	apiService.RegisterFileRoutes(protected, nil)

	mux := http.NewServeMux()
	mux.Handle("/", api.UserMiddleware(database.Pool)(protected))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server, user.ID
}

// call sends one request as userID and returns the status and the body.
func call(t *testing.T, server *httptest.Server, userID uuid.UUID, method, path string, body any) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, server.URL+path, reader)
	require.NoError(t, err)
	if userID != uuid.Nil {
		req.Header.Set("X-User-ID", userID.String())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := server.Client().Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, data
}

func TestE2EFlow(t *testing.T) {
	server, userID := setupE2E(t)
	sessionID := "e2e-session-" + uuid.NewString()

	t.Run("Memories", func(t *testing.T) {
		status, data := call(t, server, userID, http.MethodPost, "/memories",
			map[string]any{"content": "I love E2E testing", "importance": 8})
		require.Equal(t, http.StatusCreated, status, string(data))

		var created api.MemoryDTO
		require.NoError(t, json.Unmarshal(data, &created))
		assert.Equal(t, userID, created.UserID)
		assert.Equal(t, "I love E2E testing", created.Content)

		status, data = call(t, server, userID, http.MethodGet, "/memories", nil)
		require.Equal(t, http.StatusOK, status, string(data))

		var list []api.MemoryDTO
		require.NoError(t, json.Unmarshal(data, &list))
		require.Len(t, list, 1)
		assert.Equal(t, created.ID, list[0].ID)
	})

	t.Run("Chat stream is saved with the conversation", func(t *testing.T) {
		status, data := call(t, server, userID, http.MethodPost, "/chat/stream",
			agent.ChatRequest{SessionID: sessionID, Content: "Hello E2E!"})
		require.Equal(t, http.StatusOK, status, string(data))

		stream := string(data)
		assert.Contains(t, stream, `data: {"content":"Hello "}`)
		assert.Contains(t, stream, `data: {"content":"E2E"}`)
		assert.Contains(t, stream, "data: [DONE]")

		// The messages the stream produced are readable through the API,
		// oldest first.
		status, data = call(t, server, userID, http.MethodGet, "/messages?session_id="+sessionID, nil)
		require.Equal(t, http.StatusOK, status, string(data))

		var msgs []api.MessageDTO
		require.NoError(t, json.Unmarshal(data, &msgs))
		require.Len(t, msgs, 2)
		assert.Equal(t, "user", msgs[0].Role)
		assert.Equal(t, "Hello E2E!", msgs[0].Content)
		assert.Equal(t, "assistant", msgs[1].Role)
		assert.Equal(t, "Hello E2E", msgs[1].Content)
	})

	t.Run("Files list is empty under both prefixes", func(t *testing.T) {
		for _, path := range api.FilePrefixes {
			status, data := call(t, server, userID, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, status, "%s: %s", path, data)

			var files []any
			require.NoError(t, json.Unmarshal(data, &files), path)
			assert.Empty(t, files, path)
		}
	})

	t.Run("Missing user header is rejected", func(t *testing.T) {
		status, _ := call(t, server, uuid.Nil, http.MethodGet, "/memories", nil)
		assert.Equal(t, http.StatusUnauthorized, status)
	})
}

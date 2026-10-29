package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go-monolith/internal/agent"
	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go-monolith/internal/prompts"
)

func setupTestE2E(t *testing.T) (*httptest.Server, *db.Database, uuid.UUID) {
	prompts.Init("../prompts") // Or whatever path tests run in

	// tests are run in tests/, so path is ../prompts. Let's make sure template loads
	if err := prompts.GlobalManager.Preload("chat_system.tmpl"); err != nil {
		t.Logf("Failed to preload template from disk (tests might be run from root), trying 'prompts': %v", err)
		prompts.Init("prompts")
		if err2 := prompts.GlobalManager.Preload("chat_system.tmpl"); err2 != nil {
			prompts.Init("../prompts") // Fall back
		}
	}

	connString := os.Getenv("DATABASE_URL")
	if connString == "" {
		t.Skip("DATABASE_URL not set, skipping E2E test")
	}

	ctx := context.Background()

	rawDB, err := sql.Open("pgx", connString)
	require.NoError(t, err)

	err = goose.SetDialect("postgres")
	require.NoError(t, err)
	err = goose.Up(rawDB, "../migrations")
	require.NoError(t, err)

	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)

	// Clean up previous runs
	_, _ = database.Pool.Exec(ctx, "DELETE FROM agent_traces")
	_, _ = database.Pool.Exec(ctx, "DELETE FROM messages")
	_, _ = database.Pool.Exec(ctx, "DELETE FROM memories")
	_, _ = database.Pool.Exec(ctx, "DELETE FROM users")

	// Set up mocks
	mockEmbedder := &llm.MockEmbedder{}
	mockLLM := &llm.MockLLM{}

	apiService := api.New(database, mockEmbedder)
	retrievalService := retrieval.NewRetrievalService(database, mockEmbedder, nil)
	toolRegistry := agent.NewDefaultToolRegistry(retrievalService, database)
	orchestrator := agent.NewOrchestrator(database, retrievalService, mockLLM, toolRegistry)

	// Create user
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)

	// Setup mux
	mux := http.NewServeMux()

	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("/chat/stream", orchestrator.HandleStreamChat)
	protectedMux.HandleFunc("/chat/regenerate", orchestrator.HandleRegenerate)

	protectedMux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			apiService.CreateMessageHandler(w, r)
		} else if r.Method == http.MethodGet {
			apiService.GetMessagesHandler(w, r)
		}
	})

	protectedMux.HandleFunc("/memories", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			apiService.CreateMemoryHandler(w, r)
		} else if r.Method == http.MethodGet {
			apiService.GetMemoriesHandler(w, r)
		}
	})
	protectedMux.HandleFunc("/memories/", apiService.MemoryByIDHandler)

	protectedMux.HandleFunc("/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			apiService.GetFilesHandler(w, r)
		}
	})

	// User Middleware
	userMiddleware := api.UserMiddleware(database.Pool)
	mux.Handle("/", userMiddleware(protectedMux))

	server := httptest.NewServer(mux)

	t.Cleanup(func() {
		server.Close()
		if database.SqlDB != nil {
			database.SqlDB.Close()
		}
		if database.Pool != nil {
			database.Pool.Close()
		}
		goose.DownTo(rawDB, "../migrations", 0)
		rawDB.Close()
	})

	return server, database, user.ID
}

func TestE2EFlow(t *testing.T) {
	server, _, userID := setupTestE2E(t)
	client := server.Client()

	sessionID := "e2e-session-123"

	// Scenario 1: Memories (Create and Get)
	t.Run("Memories", func(t *testing.T) {
		reqBody, _ := json.Marshal(map[string]interface{}{
			"content":    "I love E2E testing",
			"importance": 8,
		})
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/memories", bytes.NewBuffer(reqBody))
		req.Header.Set("X-User-ID", userID.String())
		req.Header.Set("Content-Type", "application/json")

		res, err := client.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusCreated, res.StatusCode)

		var createdMem db.Memory
		json.NewDecoder(res.Body).Decode(&createdMem)
		res.Body.Close()
		assert.Equal(t, "I love E2E testing", createdMem.Content)

		// Get memory
		req, _ = http.NewRequest(http.MethodGet, server.URL+"/memories", nil)
		req.Header.Set("X-User-ID", userID.String())

		res, err = client.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, res.StatusCode)

		var mems []db.Memory
		json.NewDecoder(res.Body).Decode(&mems)
		res.Body.Close()
		assert.Len(t, mems, 1)
		assert.Equal(t, "I love E2E testing", mems[0].Content)
	})

	// Scenario 2: Chat
	t.Run("Chat Stream", func(t *testing.T) {
		// Because we're using a mock LLM without initialized StreamChunks, we might get an empty stream,
		// but we can at least assert that the request hits the endpoint and parses our prompt properly.
		reqBody := agent.ChatRequest{
			SessionID: sessionID,
			Content:   "Hello E2E!",
		}
		reqBytes, _ := json.Marshal(reqBody)
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/chat/stream", bytes.NewReader(reqBytes))
		req.Header.Set("X-User-ID", userID.String())

		res, err := client.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, res.StatusCode)

		buf := new(bytes.Buffer)
		buf.ReadFrom(res.Body)
		res.Body.Close()

		bodyStr := buf.String()
		assert.Contains(t, bodyStr, "data: [DONE]")

		// Verify Message was saved to DB
		// Need a small sleep because HandleStreamChat saves message asynchronously
		// after responding.
		// msgs, err := database.Queries.GetMessagesBySession(...)
	})

	// Scenario 3: Files GET
	t.Run("Get Files", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/files", nil)
		req.Header.Set("X-User-ID", userID.String())

		res, err := client.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, res.StatusCode)

		var files []interface{}
		json.NewDecoder(res.Body).Decode(&files)
		res.Body.Close()
		assert.Len(t, files, 0) // no files uploaded yet
	})
}

package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go-monolith/internal/llm"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
)

func TestMain(m *testing.M) {
	connString := os.Getenv("DATABASE_URL")
	if connString != "" {
		// Run migrations once
		rawDB, err := sql.Open("pgx", connString)
		if err == nil {
			goose.SetDialect("postgres")
			goose.Up(rawDB, "../../migrations")
			rawDB.Close()
		}
	}

	os.Exit(m.Run())
}

func setupTestDB(t *testing.T) *db.Database {
	connString := os.Getenv("DATABASE_URL")
	if connString == "" {
		t.Skip("DATABASE_URL not set")
	}

	database, err := db.Connect(context.Background(), connString)
	require.NoError(t, err)

	t.Cleanup(func() {
		if database.SqlDB != nil {
			database.SqlDB.Close()
		}
		if database.Pool != nil {
			database.Pool.Close()
		}
	})

	return database
}

func setupAPI(t *testing.T) (*api.API, *db.Database, uuid.UUID) {
	database := setupTestDB(t)

	// We use a mock embedder for tests to avoid hitting the actual embedding API
	mockEmbedder := &llm.MockEmbedder{}
	apiService := api.New(database, mockEmbedder)

	// Clean up database before and after
	_, _ = database.Pool.Exec(context.Background(), "TRUNCATE TABLE agent_traces, messages, memories, document_chunks, ingested_files, users CASCADE")

	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), "TRUNCATE TABLE agent_traces, messages, memories, document_chunks, ingested_files, users CASCADE")
	})

	// Create test user
	user, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)

	return apiService, database, user.ID
}

func addContext(req *http.Request, userID uuid.UUID) *http.Request {
	ctx := context.WithValue(req.Context(), api.ParsedUserIDKey, userID)
	return req.WithContext(ctx)
}

func TestMessagesAPI(t *testing.T) {
	apiService, _, userID := setupAPI(t)

	sessionID := "test-session-123"

	// Create Message
	reqBody, _ := json.Marshal(map[string]string{
		"session_id": sessionID,
		"role":       "user",
		"content":    "Hello World",
	})
	req := httptest.NewRequest(http.MethodPost, "/messages", bytes.NewBuffer(reqBody))
	req.Header.Set("X-User-ID", userID.String())
	req.Header.Set("Content-Type", "application/json")
	req = addContext(req, userID)
	w := httptest.NewRecorder()

	apiService.CreateMessageHandler(w, req)

	res := w.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var createdMsg api.MessageDTO
	json.NewDecoder(res.Body).Decode(&createdMsg)
	res.Body.Close()

	assert.Equal(t, userID, createdMsg.UserID)
	assert.Equal(t, sessionID, createdMsg.SessionID)
	assert.Equal(t, "user", createdMsg.Role)
	assert.Equal(t, "Hello World", createdMsg.Content)

	// Get Messages
	req = httptest.NewRequest(http.MethodGet, "/messages?session_id="+sessionID, nil)
	req.Header.Set("X-User-ID", userID.String())
	req = addContext(req, userID)
	w = httptest.NewRecorder()

	apiService.GetMessagesHandler(w, req)

	res = w.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var msgs []api.MessageDTO
	json.NewDecoder(res.Body).Decode(&msgs)
	res.Body.Close()

	assert.Len(t, msgs, 1)
	assert.Equal(t, createdMsg.ID, msgs[0].ID)
}

func TestMemoriesAPI(t *testing.T) {
	apiService, _, userID := setupAPI(t)

	// 1. Create Memory
	reqBody, _ := json.Marshal(map[string]interface{}{
		"content":    "Test Memory",
		"importance": 5,
	})
	req := httptest.NewRequest(http.MethodPost, "/memories", bytes.NewBuffer(reqBody))
	req.Header.Set("X-User-ID", userID.String())
	req.Header.Set("Content-Type", "application/json")
	req = addContext(req, userID)
	w := httptest.NewRecorder()

	apiService.CreateMemoryHandler(w, req)
	res := w.Result()
	assert.Equal(t, http.StatusCreated, res.StatusCode)

	var createdMem api.MemoryDTO
	json.NewDecoder(res.Body).Decode(&createdMem)
	res.Body.Close()

	assert.Equal(t, userID, createdMem.UserID)
	assert.Equal(t, "Test Memory", createdMem.Content)
	assert.Equal(t, "candidate", createdMem.Status) // Default status

	// 2. Duplicate Memory Check (Should return 409)
	req = httptest.NewRequest(http.MethodPost, "/memories", bytes.NewBuffer(reqBody))
	req.Header.Set("X-User-ID", userID.String())
	req.Header.Set("Content-Type", "application/json")
	req = addContext(req, userID)
	w = httptest.NewRecorder()

	apiService.CreateMemoryHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusConflict, res.StatusCode)

	var dupMem api.MemoryDTO
	json.NewDecoder(res.Body).Decode(&dupMem)
	res.Body.Close()
	assert.Equal(t, createdMem.ID, dupMem.ID)

	// 3. Get Memories
	req = httptest.NewRequest(http.MethodGet, "/memories", nil)
	req.Header.Set("X-User-ID", userID.String())
	req = addContext(req, userID)
	w = httptest.NewRecorder()

	apiService.GetMemoriesHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var mems []api.MemoryDTO
	json.NewDecoder(res.Body).Decode(&mems)
	res.Body.Close()

	assert.Len(t, mems, 1)

	// 4. Update Memory
	reqBodyUpdate, _ := json.Marshal(map[string]interface{}{
		"content":    "Updated Memory",
		"status":     "active",
		"importance": 10,
	})
	req = httptest.NewRequest(http.MethodPut, "/memories/"+createdMem.ID.String(), bytes.NewBuffer(reqBodyUpdate))
	req.Header.Set("X-User-ID", userID.String())
	req.Header.Set("Content-Type", "application/json")
	req = addContext(req, userID)
	w = httptest.NewRecorder()

	apiService.MemoryByIDHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var updatedMem api.MemoryDTO
	json.NewDecoder(res.Body).Decode(&updatedMem)
	res.Body.Close()
	assert.Equal(t, "active", updatedMem.Status)
	assert.Equal(t, "Updated Memory", updatedMem.Content)

	// 5. Delete Memory
	req = httptest.NewRequest(http.MethodDelete, "/memories/"+createdMem.ID.String(), nil)
	req.Header.Set("X-User-ID", userID.String())
	req = addContext(req, userID)
	w = httptest.NewRecorder()

	apiService.MemoryByIDHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusNoContent, res.StatusCode)
}

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

func setupTestDB(t *testing.T) *db.Database {
	connString := os.Getenv("DATABASE_URL")
	if connString == "" {
		t.Skip("DATABASE_URL not set")
	}

	rawDB, err := sql.Open("pgx", connString)
	require.NoError(t, err)

	err = goose.SetDialect("postgres")
	require.NoError(t, err)
	err = goose.Up(rawDB, "../../migrations")
	require.NoError(t, err)

	database, err := db.Connect(context.Background(), connString)
	require.NoError(t, err)

	t.Cleanup(func() {
		database.Pool.Close()
		goose.DownTo(rawDB, "../../migrations", 0)
		rawDB.Close()
	})

	return database
}

type zeroDimEmbedder struct{}

func (m *zeroDimEmbedder) EmbedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	result := make([][]float32, len(texts))
	for i := range texts {
		result[i] = make([]float32, 0)
	}
	return result, nil
}


func setupAPI(t *testing.T) (*api.API, *db.Database, uuid.UUID) {
	database := setupTestDB(t)
	// We use a mock embedder for tests to avoid hitting the actual embedding API
	mockEmbedder := &llm.MockEmbedder{}
	apiService := api.New(database, mockEmbedder)

	// Create test user
	user, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)

	return apiService, database, user.ID
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
	w := httptest.NewRecorder()

	apiService.CreateMessageHandler(w, req)

	res := w.Result()
	assert.Equal(t, http.StatusCreated, res.StatusCode, "expected 201 Created or 200 OK")

	// The current code returns 200 OK for messages, let's just check success
	assert.True(t, res.StatusCode == 200 || res.StatusCode == 201)

	var createdMsg db.Message
	json.NewDecoder(res.Body).Decode(&createdMsg)
	res.Body.Close()

	assert.Equal(t, userID, createdMsg.UserID)
	assert.Equal(t, sessionID, createdMsg.SessionID)
	assert.Equal(t, "user", createdMsg.Role)
	assert.Equal(t, "Hello World", createdMsg.Content)

	// Get Messages
	req = httptest.NewRequest(http.MethodGet, "/messages?session_id="+sessionID, nil)
	req.Header.Set("X-User-ID", userID.String())
	w = httptest.NewRecorder()

	apiService.GetMessagesHandler(w, req)

	res = w.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var msgs []db.Message
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
	w := httptest.NewRecorder()

	apiService.CreateMemoryHandler(w, req)
	res := w.Result()
	assert.Equal(t, http.StatusCreated, res.StatusCode)

	var createdMem db.Memory
	json.NewDecoder(res.Body).Decode(&createdMem)
	res.Body.Close()

	assert.Equal(t, userID, createdMem.UserID)
	assert.Equal(t, "Test Memory", createdMem.Content)
	assert.Equal(t, "candidate", createdMem.Status) // Default status

	// 2. Duplicate Memory Check (Should return 409)
	req = httptest.NewRequest(http.MethodPost, "/memories", bytes.NewBuffer(reqBody))
	req.Header.Set("X-User-ID", userID.String())
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()

	apiService.CreateMemoryHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusConflict, res.StatusCode)

	var dupMem db.Memory
	json.NewDecoder(res.Body).Decode(&dupMem)
	res.Body.Close()
	assert.Equal(t, createdMem.ID, dupMem.ID)

	// 3. Get Memories
	req = httptest.NewRequest(http.MethodGet, "/memories", nil)
	req.Header.Set("X-User-ID", userID.String())
	w = httptest.NewRecorder()

	apiService.GetMemoriesHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var mems []db.Memory
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
	w = httptest.NewRecorder()

	apiService.MemoryByIDHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var updatedMem db.Memory
	json.NewDecoder(res.Body).Decode(&updatedMem)
	res.Body.Close()
	assert.Equal(t, "active", updatedMem.Status)
	assert.Equal(t, "Updated Memory", updatedMem.Content)

	// 5. Delete Memory
	req = httptest.NewRequest(http.MethodDelete, "/memories/"+createdMem.ID.String(), nil)
	req.Header.Set("X-User-ID", userID.String())
	w = httptest.NewRecorder()

	apiService.MemoryByIDHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusNoContent, res.StatusCode)
}

func TestMemoriesAPI_ZeroDimension(t *testing.T) {
	database := setupTestDB(t)
	apiService := api.New(database, &zeroDimEmbedder{})

	user, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)

	reqBody, _ := json.Marshal(map[string]interface{}{
		"content":    "Test Memory Zero Dim",
		"importance": 5,
	})
	req := httptest.NewRequest(http.MethodPost, "/memories", bytes.NewBuffer(reqBody))
	req.Header.Set("X-User-ID", user.ID.String())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	apiService.CreateMemoryHandler(w, req)
	res := w.Result()
	assert.Equal(t, http.StatusInternalServerError, res.StatusCode)
}

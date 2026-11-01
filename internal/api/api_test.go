package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-monolith/internal/llm"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

func setupTestDB(t *testing.T) *db.Database {
	connString := dbtest.ConnString(t)

	rawDB, err := sql.Open("pgx", connString)
	require.NoError(t, err)

	err = goose.SetDialect("postgres")
	require.NoError(t, err)
	err = goose.Up(rawDB, "../../migrations")
	require.NoError(t, err)

	database, err := db.Connect(context.Background(), connString)
	require.NoError(t, err)

	t.Cleanup(func() {
		// Close through Database.Close so the database/sql wrapper releases its
		// connections before the pool is closed (the reverse order hangs).
		//
		// The schema is deliberately left in place: dropping it (goose.DownTo
		// to 0) would erase the shared test database while other packages are
		// still using it, and it is the exact operation that once emptied the
		// live database (the 2026-10-30 incident).
		database.Close()
		rawDB.Close()
	})

	return database
}

func setupAPI(t *testing.T) (*api.API, *db.Database, uuid.UUID) {
	database := setupTestDB(t)
	// We use a mock embedder for tests to avoid hitting the actual embedding API
	mockEmbedder := &llm.MockEmbedder{}
	apiService := api.New(database, mockEmbedder)

	// Create test user
	user, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)

	// Remove only this test's own user. The cleanup runs before the pool is
	// closed because it is registered later than the one in setupTestDB.
	t.Cleanup(func() {
		dbtest.DeleteUser(t, database.Pool, user.ID)
	})

	return apiService, database, user.ID
}

// withUser attaches the parsed user id to the request the same way
// UserMiddleware does. Tests that call handlers directly - without the
// middleware chain - need it, otherwise the handlers answer 401.
func withUser(req *http.Request, userID uuid.UUID) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, userID))
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
	req = withUser(req, userID)
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
	req = withUser(req, userID)
	w = httptest.NewRecorder()

	apiService.GetMessagesHandler(w, req)

	res = w.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var msgs []db.Message
	json.NewDecoder(res.Body).Decode(&msgs)
	res.Body.Close()

	require.Len(t, msgs, 1)
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
	req = withUser(req, userID)
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
	req = withUser(req, userID)
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
	req = withUser(req, userID)
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
	req = withUser(req, userID)
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
	req = withUser(req, userID)
	w = httptest.NewRecorder()

	apiService.MemoryByIDHandler(w, req)
	res = w.Result()
	assert.Equal(t, http.StatusNoContent, res.StatusCode)
}
// createTestMessage stores a message through the API and returns the created message.
// The parsed user id is put in the context because the handlers are called
// directly, without the user middleware that normally resolves it.
func createTestMessage(t *testing.T, apiService *api.API, userID uuid.UUID, sessionID, role, content string) api.MessageDTO {
	t.Helper()

	reqBody, _ := json.Marshal(map[string]string{
		"session_id": sessionID,
		"role":       role,
		"content":    content,
	})
	req := httptest.NewRequest(http.MethodPost, "/messages", bytes.NewBuffer(reqBody))
	req.Header.Set("X-User-ID", userID.String())
	req.Header.Set("Content-Type", "application/json")
	req = withUser(req, userID)
	w := httptest.NewRecorder()

	apiService.CreateMessageHandler(w, req)

	res := w.Result()
	defer res.Body.Close()
	require.Equal(t, http.StatusCreated, res.StatusCode)

	var msg api.MessageDTO
	require.NoError(t, json.NewDecoder(res.Body).Decode(&msg))
	return msg
}

// getTestMessages calls GET /messages with the given query string and decodes the body.
func getTestMessages(t *testing.T, apiService *api.API, userID uuid.UUID, rawQuery string) []api.MessageDTO {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/messages"+rawQuery, nil)
	req.Header.Set("X-User-ID", userID.String())
	req = withUser(req, userID)
	w := httptest.NewRecorder()

	apiService.GetMessagesHandler(w, req)

	res := w.Result()
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var msgs []api.MessageDTO
	require.NoError(t, json.NewDecoder(res.Body).Decode(&msgs))
	return msgs
}

// assertOldestFirst fails when the transcript is not in ascending time order.
func assertOldestFirst(t *testing.T, msgs []api.MessageDTO) {
	t.Helper()

	for i := 1; i < len(msgs); i++ {
		require.NotNil(t, msgs[i-1].CreatedAt)
		require.NotNil(t, msgs[i].CreatedAt)
		assert.False(t, msgs[i].CreatedAt.Before(*msgs[i-1].CreatedAt),
			"message %d is older than message %d", i, i-1)
	}
}

func TestGetMessagesReturnsOldestFirst(t *testing.T) {
	apiService, _, userID := setupAPI(t)

	first := createTestMessage(t, apiService, userID, "chrono-session", "user", "first")
	second := createTestMessage(t, apiService, userID, "chrono-session", "assistant", "second")
	third := createTestMessage(t, apiService, userID, "chrono-session", "user", "third")

	msgs := getTestMessages(t, apiService, userID, "?session_id=chrono-session")
	require.Len(t, msgs, 3)

	// A transcript read top to bottom has to start with the oldest message.
	assert.Equal(t, first.ID, msgs[0].ID)
	assert.Equal(t, second.ID, msgs[1].ID)
	assert.Equal(t, third.ID, msgs[2].ID)
	assertOldestFirst(t, msgs)
}

func TestGetMessagesWithoutSessionReturnsRecentMessages(t *testing.T) {
	apiService, database, userID := setupAPI(t)

	old := createTestMessage(t, apiService, userID, "session-old", "user", "from the old session")
	newer := createTestMessage(t, apiService, userID, "session-new", "user", "from the new session")

	// Another user's history must never show up in the fallback.
	otherUser, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	createTestMessage(t, apiService, otherUser.ID, "session-old", "user", "not mine")

	msgs := getTestMessages(t, apiService, userID, "")
	require.Len(t, msgs, 2)
	assert.Equal(t, old.ID, msgs[0].ID)
	assert.Equal(t, newer.ID, msgs[1].ID)

	// Each message keeps its session id so the client can tell conversations apart.
	assert.Equal(t, "session-old", msgs[0].SessionID)
	assert.Equal(t, "session-new", msgs[1].SessionID)

	// The limit still applies without a session id.
	limited := getTestMessages(t, apiService, userID, "?limit=1")
	require.Len(t, limited, 1)
	assert.Equal(t, newer.ID, limited[0].ID)
}

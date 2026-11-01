package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// serveFiles sends a request through the file routes the way main wires them.
func serveFiles(a *api.API, userID uuid.UUID, method, path, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	a.RegisterFileRoutes(mux, nil)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withUser(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func newStranger(t *testing.T, database *db.Database) uuid.UUID {
	t.Helper()
	user, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })
	return user.ID
}

func TestAnotherUsersFileLooksLikeAMissingOne(t *testing.T) {
	a, database, owner := setupAPI(t)
	stranger := newStranger(t, database)
	file := createTestFile(t, database, owner, "private.txt", "completed")

	missing := uuid.NewString()
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"read", http.MethodGet, "/files/%s", ""},
		{"delete", http.MethodDelete, "/files/%s", ""},
		{"category", http.MethodPatch, "/files/%s/category", `{"category":"law"}`},
		{"read, versioned", http.MethodGet, "/v1/files/%s", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			theirs := serveFiles(a, stranger, tc.method, strings.Replace(tc.path, "%s", file.ID.String(), 1), tc.body)
			nothing := serveFiles(a, stranger, tc.method, strings.Replace(tc.path, "%s", missing, 1), tc.body)

			assert.Equal(t, http.StatusNotFound, theirs.Code, "403 would tell the stranger that the id exists")
			assert.Equal(t, nothing.Code, theirs.Code)
			assert.Equal(t, nothing.Body.String(), theirs.Body.String(), "the answer must not differ from the one for a missing file")
		})
	}

	stored, err := database.Queries.GetIngestedFileByID(context.Background(), file.ID)
	require.NoError(t, err)
	assert.False(t, stored.DeletedAt.Valid, "the stranger's DELETE must not have deleted anything")
	assert.NotEqual(t, "law", stored.Category)

	// The owner still sees it.
	assert.Equal(t, http.StatusOK, serveFiles(a, owner, http.MethodGet, "/files/"+file.ID.String(), "").Code)
}

func TestFileCategoryAcceptsOnlyKnownCategories(t *testing.T) {
	a, database, owner := setupAPI(t)
	file := createTestFile(t, database, owner, "book.txt", "completed")
	path := "/files/" + file.ID.String() + "/category"

	for _, category := range config.FileCategories {
		rec := serveFiles(a, owner, http.MethodPatch, path, `{"category":"`+category+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, category)
		var dto api.IngestedFileDTO
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dto))
		assert.Equal(t, category, dto.Category)
	}

	require.Equal(t, http.StatusOK, serveFiles(a, owner, http.MethodPatch, path, `{"category":"law"}`).Code)
	for _, bad := range []string{`""`, `"cooking"`, `"General"`, `" law"`, `"law,medicine"`, `null`} {
		rec := serveFiles(a, owner, http.MethodPatch, path, `{"category":`+bad+`}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
		assert.Contains(t, rec.Body.String(), "economics", "the answer lists the categories that are accepted")
	}
	rec := serveFiles(a, owner, http.MethodPatch, path, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a missing category is not 'general'")

	stored, err := database.Queries.GetIngestedFileByID(context.Background(), file.ID)
	require.NoError(t, err)
	assert.Equal(t, "law", stored.Category, "the refused requests changed nothing")
}

func TestCategoryOfADeletedFileIsNotChangeable(t *testing.T) {
	a, database, owner := setupAPI(t)
	file := createTestFile(t, database, owner, "gone.txt", "completed")
	require.Equal(t, http.StatusNoContent, serveFiles(a, owner, http.MethodDelete, "/files/"+file.ID.String(), "").Code)

	rec := serveFiles(a, owner, http.MethodPatch, "/files/"+file.ID.String()+"/category", `{"category":"law"}`)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	stored, err := database.Queries.GetIngestedFileByID(context.Background(), file.ID)
	require.NoError(t, err)
	assert.NotEqual(t, "law", stored.Category)
}

// createMemoryViaAPI stores a memory through the handler and returns its id.
func createMemoryViaAPI(t *testing.T, a *api.API, userID uuid.UUID, content string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"content": content, "status": "active", "importance": 5})
	req := withUser(httptest.NewRequest(http.MethodPost, "/memories", bytes.NewReader(body)), userID)
	rec := httptest.NewRecorder()
	a.CreateMemoryHandler(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	return created.ID
}

func memoryRequest(a *api.API, userID uuid.UUID, method, path string) *httptest.ResponseRecorder {
	req := withUser(httptest.NewRequest(method, path, strings.NewReader(`{"content":"x","status":"active","importance":5}`)), userID)
	rec := httptest.NewRecorder()
	a.MemoryByIDHandler(rec, req)
	return rec
}

func memoryCount(t *testing.T, database *db.Database, userID uuid.UUID) int {
	t.Helper()
	mems, err := database.Queries.GetMemories(context.Background(), db.GetMemoriesParams{UserID: userID})
	require.NoError(t, err)
	return len(mems)
}

func TestMemoryPathIsExactlyMemoriesAndTheId(t *testing.T) {
	a, database, owner := setupAPI(t)
	id := createMemoryViaAPI(t, a, owner, "Обича кафе "+uuid.NewString())

	for _, path := range []string{
		"/memories/" + id + "/something",
		"/memories/" + id + "/",
		"/memories/" + id + "/../" + id,
		"/memories/",
		"/memories",
		"/other/" + id,
	} {
		for _, method := range []string{http.MethodDelete, http.MethodPut} {
			rec := memoryRequest(a, owner, method, path)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", method, path)
		}
	}
	assert.Equal(t, 1, memoryCount(t, database, owner), "none of the odd paths deleted the memory")

	assert.Equal(t, http.StatusBadRequest, memoryRequest(a, owner, http.MethodDelete, "/memories/not-a-uuid").Code, "a malformed id is a bad request")
}

func TestDeletingAMemory(t *testing.T) {
	a, database, owner := setupAPI(t)
	stranger := newStranger(t, database)
	id := createMemoryViaAPI(t, a, owner, "Спомен за изтриване "+uuid.NewString())

	// Another user's memory is not there as far as the stranger can tell.
	assert.Equal(t, http.StatusNotFound, memoryRequest(a, stranger, http.MethodDelete, "/memories/"+id).Code)
	assert.Equal(t, 1, memoryCount(t, database, owner), "the stranger deleted nothing")

	assert.Equal(t, http.StatusNoContent, memoryRequest(a, owner, http.MethodDelete, "/memories/"+id).Code)
	assert.Zero(t, memoryCount(t, database, owner))

	// Deleting what is gone is a 404, not a second 'success'.
	assert.Equal(t, http.StatusNotFound, memoryRequest(a, owner, http.MethodDelete, "/memories/"+id).Code)
	assert.Equal(t, http.StatusNotFound, memoryRequest(a, owner, http.MethodDelete, "/memories/"+uuid.NewString()).Code)
}

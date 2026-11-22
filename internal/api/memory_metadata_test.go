package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
)

func sendMemory(t *testing.T, a *api.API, userID uuid.UUID, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := withUser(httptest.NewRequest(method, path, bytes.NewReader(raw)), userID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	if method == http.MethodPost {
		a.CreateMemoryHandler(w, req)
	} else {
		a.MemoryByIDHandler(w, req)
	}
	return w
}

func storedMetadata(t *testing.T, database *db.Database, userID, id uuid.UUID) map[string]any {
	t.Helper()
	mem, err := database.Queries.GetMemoryByID(context.Background(), db.GetMemoryByIDParams{ID: id, UserID: userID})
	require.NoError(t, err)
	if !mem.Metadata.Valid {
		return nil
	}
	var meta map[string]any
	require.NoError(t, json.Unmarshal(mem.Metadata.RawMessage, &meta))
	return meta
}

// The metadata of a memory sent through the API is stored. It used to be dropped without a word
// (pqtype.NullRawMessage has no UnmarshalJSON): a fact_key "name" sent with PUT never reached the
// database, and the reflector went on writing "Потребителят" (2026-11-22).
func TestTheMetadataOfAMemoryIsStored(t *testing.T) {
	a, database, userID := setupAPI(t)

	w := sendMemory(t, a, userID, http.MethodPost, "/memories", map[string]any{
		"content": "Ивайло се казва.", "importance": 7, "metadata": map[string]any{"source": "test"},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var created api.MemoryDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.Equal(t, map[string]any{"source": "test"}, storedMetadata(t, database, userID, created.ID), "POST stores it")

	path := "/memories/" + created.ID.String()
	w = sendMemory(t, a, userID, http.MethodPut, path, map[string]any{
		"content": "Ивайло се казва.", "status": "active", "importance": 7,
		"metadata": map[string]any{"source": "test", "fact_key": "name", "value": "Ивайло"},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	want := map[string]any{"source": "test", "fact_key": "name", "value": "Ивайло"}
	assert.Equal(t, want, storedMetadata(t, database, userID, created.ID), "PUT stores it, with the same text too")

	for _, body := range []map[string]any{
		{"content": "Ивайло се казва.", "status": "active", "importance": 7},
		{"content": "Ивайло се казва.", "status": "active", "importance": 7, "metadata": nil},
	} {
		w = sendMemory(t, a, userID, http.MethodPut, path, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, want, storedMetadata(t, database, userID, created.ID), "a PUT without metadata keeps it")
	}

	for _, bad := range []any{"name", []any{1}, 5} {
		w = sendMemory(t, a, userID, http.MethodPut, path, map[string]any{
			"content": "Ивайло се казва.", "status": "active", "importance": 7, "metadata": bad,
		})
		assert.Equal(t, http.StatusBadRequest, w.Code, "metadata %v is not an object", bad)
	}
	assert.Equal(t, want, storedMetadata(t, database, userID, created.ID))
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
)

func TestParseFilePath(t *testing.T) {
	fileID := uuid.New()

	tests := []struct {
		name         string
		path         string
		wantID       uuid.UUID
		wantCategory bool
		wantErr      error
	}{
		{name: "versioned item", path: "/v1/files/" + fileID.String(), wantID: fileID},
		{name: "ui item", path: "/files/" + fileID.String(), wantID: fileID},
		{name: "versioned category", path: "/v1/files/" + fileID.String() + "/category", wantID: fileID, wantCategory: true},
		{name: "ui category", path: "/files/" + fileID.String() + "/category", wantID: fileID, wantCategory: true},
		{name: "collection", path: "/v1/files", wantErr: errInvalidFilePath},
		{name: "trailing slash", path: "/v1/files/", wantErr: errInvalidFilePath},
		{name: "extra segment", path: "/v1/files/" + fileID.String() + "/chunks", wantErr: errInvalidFilePath},
		{name: "bad uuid", path: "/files/not-a-uuid", wantErr: errInvalidFileID},
		{name: "other endpoint", path: "/memories/" + fileID.String(), wantErr: errInvalidFilePath},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, category, err := parseFilePath(tc.path)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
			assert.Equal(t, tc.wantCategory, category)
		})
	}
}

func TestRegisterFileRoutesServesBothPrefixes(t *testing.T) {
	mux := http.NewServeMux()
	(&API{}).RegisterFileRoutes(mux, nil)

	fileID := uuid.New()

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		// Without a user in the context every handler answers 401, which proves
		// the route reached a handler. A 404 would mean it is not registered.
		{name: "versioned collection", method: http.MethodGet, path: "/v1/files", want: http.StatusUnauthorized},
		{name: "ui collection", method: http.MethodGet, path: "/files", want: http.StatusUnauthorized},
		{name: "ui item", method: http.MethodGet, path: "/files/" + fileID.String(), want: http.StatusUnauthorized},
		{name: "ui delete", method: http.MethodDelete, path: "/files/" + fileID.String(), want: http.StatusUnauthorized},
		{name: "ui category", method: http.MethodPatch, path: "/files/" + fileID.String() + "/category", want: http.StatusUnauthorized},
		{name: "unsupported method", method: http.MethodPut, path: "/v1/files", want: http.StatusMethodNotAllowed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestFileRoutesRejectMalformedPaths(t *testing.T) {
	mux := http.NewServeMux()
	(&API{}).RegisterFileRoutes(mux, nil)

	userID := uuid.New()

	paths := []string{
		"/files/not-a-uuid",
		"/files/" + uuid.NewString() + "/chunks",
		"/v1/files/",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(context.WithValue(req.Context(), ParsedUserIDKey, userID))
			mux.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestToIngestedFileDTOReportsEmbeddingState(t *testing.T) {
	file := db.IngestedFile{
		ID:       uuid.New(),
		UserID:   uuid.New(),
		FileName: "notes.txt",
		Status:   EmbeddingStatusReady,
	}

	ready := toIngestedFileDTO(file, 3)
	assert.Equal(t, int64(3), ready.ChunkCount)
	assert.True(t, ready.HasEmbeddings)
	assert.Equal(t, EmbeddingStatusReady, ready.EmbeddingStatus)

	empty := toIngestedFileDTO(file, 0)
	assert.Equal(t, int64(0), empty.ChunkCount)
	assert.False(t, empty.HasEmbeddings)
	assert.Equal(t, EmbeddingStatusEmpty, empty.EmbeddingStatus,
		"a completed file without vectors must not look searchable")

	pending := file
	pending.Status = EmbeddingStatusPending
	pendingDTO := toIngestedFileDTO(pending, 0)
	assert.Equal(t, EmbeddingStatusPending, pendingDTO.EmbeddingStatus)
	assert.False(t, pendingDTO.HasEmbeddings)
}

// TestIngestedFileDTOJSONKeys pins the wire format the UI reads.
func TestIngestedFileDTOJSONKeys(t *testing.T) {
	payload, err := json.Marshal(toIngestedFileDTO(db.IngestedFile{}, 0))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))

	for _, key := range []string{"chunk_count", "has_embeddings", "embedding_status"} {
		assert.Contains(t, decoded, key)
	}
}

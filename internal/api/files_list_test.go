package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
)

// createTestFile inserts an ingested_files row without going through the upload
// endpoint, so tests can choose the ingest status directly.
func createTestFile(t *testing.T, database *db.Database, userID uuid.UUID, name, status string) db.IngestedFile {
	t.Helper()

	file, err := database.Queries.CreateIngestedFile(context.Background(), db.CreateIngestedFileParams{
		UserID:    userID,
		FileName:  name,
		FileHash:  uuid.NewString(),
		MimeType:  "text/plain",
		FileSize:  42,
		Status:    status,
		LocalPath: "test-data/" + name,
	})
	require.NoError(t, err)
	return file
}

// createTestChunk stores one embedded chunk, which is what the embedding status
// counts.
func createTestChunk(t *testing.T, database *db.Database, fileID, userID uuid.UUID, index int32) {
	t.Helper()

	embedding := make([]float32, config.VectorDimension)
	for i := range embedding {
		embedding[i] = float32(i%7) / 10
	}

	// The batch insert is what ingestion uses; it records the embedding model too.
	err := database.Queries.CreateDocumentChunksBatch(context.Background(), db.CreateDocumentChunksBatchParams{
		FileIds:        []uuid.UUID{fileID},
		UserIds:        []uuid.UUID{userID},
		ChunkIndices:   []int32{index},
		Texts:          []string{"chunk text"},
		Embeddings:     []pgvector.Vector{pgvector.NewVector(embedding)},
		ChunkTypes:     []string{"raw_text"},
		Metadatas:      []string{"{}"},
		EmbeddingModel: config.DefaultEmbedModel,
	})
	require.NoError(t, err)
}

// listTestFiles calls GET /v1/files the way the client does and decodes the body.
func listTestFiles(t *testing.T, apiService *api.API, userID uuid.UUID) []api.IngestedFileDTO {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	req.Header.Set("X-User-ID", userID.String())
	req = withUser(req, userID)
	rec := httptest.NewRecorder()

	apiService.GetFilesHandler(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var files []api.IngestedFileDTO
	require.NoError(t, json.NewDecoder(res.Body).Decode(&files))
	return files
}

func TestGetFilesReportsEmbeddingStatus(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	ctx := context.Background()

	otherUser, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)

	ready := createTestFile(t, database, userID, "ready.txt", "completed")
	empty := createTestFile(t, database, userID, "empty.txt", "completed")
	pending := createTestFile(t, database, userID, "pending.txt", "pending")
	createTestChunk(t, database, ready.ID, userID, 0)
	createTestChunk(t, database, ready.ID, userID, 1)

	// Another user's file and its vectors must not leak into the list or the
	// per-file counts.
	otherFile := createTestFile(t, database, otherUser.ID, "other.txt", "completed")
	createTestChunk(t, database, otherFile.ID, otherUser.ID, 0)

	files := listTestFiles(t, apiService, userID)
	require.Len(t, files, 3, "only the caller's files may be listed")

	byName := make(map[string]api.IngestedFileDTO, len(files))
	for _, f := range files {
		byName[f.FileName] = f
	}

	assert.Equal(t, int64(2), byName["ready.txt"].ChunkCount)
	assert.True(t, byName["ready.txt"].HasEmbeddings)
	assert.Equal(t, api.EmbeddingStatusReady, byName["ready.txt"].EmbeddingStatus)

	assert.Equal(t, int64(0), byName["empty.txt"].ChunkCount, empty.FileName)
	assert.False(t, byName["empty.txt"].HasEmbeddings)
	assert.Equal(t, api.EmbeddingStatusEmpty, byName["empty.txt"].EmbeddingStatus)

	assert.False(t, byName["pending.txt"].HasEmbeddings, pending.FileName)
	assert.Equal(t, api.EmbeddingStatusPending, byName["pending.txt"].EmbeddingStatus)

	_, leaked := byName["other.txt"]
	assert.False(t, leaked, "another user's file must not be listed")
}

func TestFileByIDReportsEmbeddingCountThroughShortPath(t *testing.T) {
	apiService, database, userID := setupAPI(t)

	file := createTestFile(t, database, userID, "notes.txt", "completed")
	createTestChunk(t, database, file.ID, userID, 0)
	createTestChunk(t, database, file.ID, userID, 1)
	createTestChunk(t, database, file.ID, userID, 2)

	// The chat UI calls /files/{id} without the /v1 prefix.
	req := httptest.NewRequest(http.MethodGet, "/files/"+file.ID.String(), nil)
	req = withUser(req, userID)
	rec := httptest.NewRecorder()

	apiService.FileByIDHandler(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var dto api.IngestedFileDTO
	require.NoError(t, json.NewDecoder(res.Body).Decode(&dto))
	assert.Equal(t, file.ID, dto.ID)
	assert.Equal(t, int64(3), dto.ChunkCount)
	assert.True(t, dto.HasEmbeddings)
	assert.Equal(t, api.EmbeddingStatusReady, dto.EmbeddingStatus)
}

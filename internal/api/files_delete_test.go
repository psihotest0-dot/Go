package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
)

// deleteTestFile performs DELETE /files/{id} the way the chat UI does.
func deleteTestFile(t *testing.T, apiService *api.API, userID, fileID uuid.UUID) *http.Response {
	t.Helper()

	req := httptest.NewRequest(http.MethodDelete, "/files/"+fileID.String(), nil)
	req = withUser(req, userID)
	rec := httptest.NewRecorder()

	apiService.FileByIDHandler(rec, req)

	return rec.Result()
}

// TestFileDeleteIsSoft pins the behaviour that made the 2026-10-30 incident
// unrecoverable: a hard delete cascaded into document_chunks and destroyed the
// extracted text together with its embeddings. The row and the chunks must
// survive a delete request.
func TestFileDeleteIsSoft(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	ctx := context.Background()

	file := createTestFile(t, database, userID, "keep-me.txt", "completed")
	createTestChunk(t, database, file.ID, userID, 0)
	createTestChunk(t, database, file.ID, userID, 1)

	storedChunks, err := database.Queries.GetDocumentChunksByFile(ctx, db.GetDocumentChunksByFileParams{
		FileID: file.ID,
		UserID: userID,
	})
	require.NoError(t, err)
	require.Len(t, storedChunks, 2)

	res := deleteTestFile(t, apiService, userID, file.ID)
	defer res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	stored, err := database.Queries.GetIngestedFileByID(ctx, file.ID)
	require.NoError(t, err)
	assert.True(t, stored.DeletedAt.Valid, "the row must stay in the table, marked as deleted")
	assert.Equal(t, "deleted", stored.Status)

	chunkCount, err := database.Queries.GetIngestedFileChunkCount(ctx, db.GetIngestedFileChunkCountParams{
		FileID: file.ID,
		UserID: userID,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), chunkCount, "chunks must not be cascaded away by a delete request")

	// The file is gone for every client facing endpoint.
	assert.Empty(t, listTestFiles(t, apiService, userID), "a soft deleted file must not be listed")

	rec := httptest.NewRecorder()
	apiService.FileByIDHandler(rec, withUser(httptest.NewRequest(http.MethodGet, "/files/"+file.ID.String(), nil), userID))
	require.Equal(t, http.StatusNotFound, rec.Result().StatusCode)

	// Retrieval must not hand the chunks of a deleted file back to the agent.
	found, err := database.Queries.GetDocumentChunksWithFile(ctx, db.GetDocumentChunksWithFileParams{
		ChunkIds: []uuid.UUID{storedChunks[0].ID},
		UserID:   userID,
	})
	require.NoError(t, err)
	assert.Empty(t, found, "chunks of a deleted file must not be returned to retrieval")

	// The deduplication index only covers live rows, so the same document can be
	// uploaded again after a delete.
	reUploaded, err := database.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID:    userID,
		FileName:  file.FileName,
		FileHash:  file.FileHash,
		MimeType:  file.MimeType,
		FileSize:  file.FileSize,
		Status:    "pending",
		LocalPath: "test-data/keep-me.txt",
	})
	require.NoError(t, err, "re-uploading a deleted document must not hit the unique index")
	assert.NotEqual(t, file.ID, reUploaded.ID)
}

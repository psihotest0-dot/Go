package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
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

// storedOriginal writes a file into the upload directory and points the row at it,
// the way an upload leaves it.
func storedOriginal(t *testing.T, database *db.Database, fileID uuid.UUID) string {
	t.Helper()
	dir := api.UploadDir()
	createdDir := false
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		createdDir = true
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, uuid.NewString()+"_original.txt")
	require.NoError(t, os.WriteFile(path, []byte("the original"), 0o644))
	t.Cleanup(func() {
		_ = os.Remove(path)
		if createdDir {
			_ = os.Remove(dir) // only if this test made it and it is empty
		}
	})
	_, err := database.SqlDB.ExecContext(context.Background(), `UPDATE ingested_files SET local_path = $1 WHERE id = $2`, path, fileID)
	require.NoError(t, err)
	return path
}

func chunkCount(t *testing.T, database *db.Database, fileID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, database.SqlDB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM document_chunks WHERE file_id = $1`, fileID).Scan(&n))
	return n
}

// A delete takes the file's chunks and its stored original with it, and nothing
// of any other file. The row stays, marked deleted, as a record. (Until
// 2026-11-14 the chunks were kept so a delete could be undone; nothing ever undid
// one, and they filled the vector indexes - 17 853 chunks of 7 deleted files.)
func TestFileDeleteRemovesItsChunksAndOriginal(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	ctx := context.Background()

	file := createTestFile(t, database, userID, "delete-me.txt", "completed")
	createTestChunk(t, database, file.ID, userID, 0)
	createTestChunk(t, database, file.ID, userID, 1)
	original := storedOriginal(t, database, file.ID)

	other := createTestFile(t, database, userID, "keep-me.txt", "completed")
	createTestChunk(t, database, other.ID, userID, 0)
	otherOriginal := storedOriginal(t, database, other.ID)

	res := deleteTestFile(t, apiService, userID, file.ID)
	defer res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	stored, err := database.Queries.GetIngestedFileByID(ctx, file.ID)
	require.NoError(t, err)
	assert.True(t, stored.DeletedAt.Valid, "the row must stay in the table, marked as deleted")
	assert.Equal(t, "deleted", stored.Status)
	assert.Equal(t, 0, chunkCount(t, database, file.ID), "the chunks of a deleted file must go with it")
	assert.NoFileExists(t, original, "the stored original of a deleted file must go with it")

	// Nothing of another file is touched.
	assert.Equal(t, 1, chunkCount(t, database, other.ID), "a delete must not touch the chunks of another file")
	assert.FileExists(t, otherOriginal)

	// The file is gone for every client facing endpoint.
	listed := listTestFiles(t, apiService, userID)
	require.Len(t, listed, 1)
	assert.Equal(t, other.ID, listed[0].ID)

	rec := httptest.NewRecorder()
	apiService.FileByIDHandler(rec, withUser(httptest.NewRequest(http.MethodGet, "/files/"+file.ID.String(), nil), userID))
	require.Equal(t, http.StatusNotFound, rec.Result().StatusCode)

	// A second delete is a 404, not an error.
	again := deleteTestFile(t, apiService, userID, file.ID)
	defer again.Body.Close()
	assert.Equal(t, http.StatusNotFound, again.StatusCode)

	// The deduplication index only covers live rows, so the same document can be
	// uploaded again after a delete.
	reUploaded, err := database.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID:    userID,
		FileName:  file.FileName,
		FileHash:  file.FileHash,
		MimeType:  file.MimeType,
		FileSize:  file.FileSize,
		Status:    "pending",
		LocalPath: "test-data/delete-me.txt",
	})
	require.NoError(t, err, "re-uploading a deleted document must not hit the unique index")
	assert.NotEqual(t, file.ID, reUploaded.ID)
}

// Another user's delete request touches nothing.
func TestFileDeleteOfAnotherUsersFileTouchesNothing(t *testing.T) {
	apiService, database, owner := setupAPI(t)
	file := createTestFile(t, database, owner, "theirs.txt", "completed")
	createTestChunk(t, database, file.ID, owner, 0)

	stranger, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, stranger.ID) })

	res := deleteTestFile(t, apiService, stranger.ID, file.ID)
	defer res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	stored, err := database.Queries.GetIngestedFileByID(context.Background(), file.ID)
	require.NoError(t, err)
	assert.False(t, stored.DeletedAt.Valid)
	assert.Equal(t, 1, chunkCount(t, database, file.ID))
}

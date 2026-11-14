package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/dbtest"
)

type deleteEnv struct {
	ctx  context.Context
	d    *Database
	user uuid.UUID
}

func newDeleteEnv(t *testing.T) deleteEnv {
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	require.NoError(t, RunMigrations(connString))
	ctx := context.Background()
	d, err := Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(d.Close)
	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, d.Pool, user.ID) })
	return deleteEnv{ctx: ctx, d: d, user: user.ID}
}

func (e deleteEnv) file(t *testing.T, name string) uuid.UUID {
	f, err := e.d.Queries.CreateIngestedFile(e.ctx, CreateIngestedFileParams{
		UserID: e.user, FileName: name, FileHash: "h-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: 1, Status: "processing", LocalPath: "storage/none",
	})
	require.NoError(t, err)
	return f.ID
}

func (e deleteEnv) insertChunks(fileID uuid.UUID, first, n int) error {
	var arg CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	for i := first; i < first+n; i++ {
		arg.FileIds = append(arg.FileIds, fileID)
		arg.UserIds = append(arg.UserIds, e.user)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, "text")
		arg.Embeddings = append(arg.Embeddings, vec(map[int]float32{i % config.VectorDimension: 1}))
		arg.ChunkTypes = append(arg.ChunkTypes, "raw_text")
		arg.Metadatas = append(arg.Metadatas, "{}")
	}
	return e.d.Queries.CreateDocumentChunksBatch(e.ctx, arg)
}

func (e deleteEnv) chunks(t *testing.T, fileID uuid.UUID) int {
	var n int
	require.NoError(t, e.d.SqlDB.QueryRowContext(e.ctx, `SELECT count(*) FROM document_chunks WHERE file_id = $1`, fileID).Scan(&n))
	return n
}

func TestDeleteFileTakesOnlyItsOwnChunks(t *testing.T) {
	e := newDeleteEnv(t)
	gone, kept := e.file(t, "gone.txt"), e.file(t, "kept.txt")
	require.NoError(t, e.insertChunks(gone, 0, 3))
	require.NoError(t, e.insertChunks(kept, 0, 2))

	n, err := e.d.DeleteFile(e.ctx, e.user, gone)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	assert.Equal(t, 0, e.chunks(t, gone))
	assert.Equal(t, 2, e.chunks(t, kept))

	f, err := e.d.Queries.GetIngestedFileByID(e.ctx, gone)
	require.NoError(t, err)
	assert.True(t, f.DeletedAt.Valid)
	assert.Equal(t, "deleted", f.Status)

	// Another user's id deletes nothing.
	other, err := e.d.Queries.CreateUser(e.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.d.Pool, other.ID) })
	n, err = e.d.DeleteFile(e.ctx, other.ID, kept)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
	assert.Equal(t, 2, e.chunks(t, kept))
	k, err := e.d.Queries.GetIngestedFileByID(e.ctx, kept)
	require.NoError(t, err)
	assert.False(t, k.DeletedAt.Valid)
}

// An ingest that is still running when its file is deleted writes no chunks
// afterwards and leaves the "deleted" status alone.
func TestNothingIsWrittenIntoADeletedFile(t *testing.T) {
	e := newDeleteEnv(t)
	f := e.file(t, "deleted.txt")
	_, err := e.d.DeleteFile(e.ctx, e.user, f)
	require.NoError(t, err)

	require.NoError(t, e.insertChunks(f, 0, 5))
	assert.Equal(t, 0, e.chunks(t, f), "a batch for a deleted file must write nothing")

	require.NoError(t, e.d.Queries.UpdateIngestedFileStatus(e.ctx, UpdateIngestedFileStatusParams{ID: f, Status: "completed", UserID: e.user}))
	got, err := e.d.Queries.GetIngestedFileByID(e.ctx, f)
	require.NoError(t, err)
	assert.Equal(t, "deleted", got.Status, "a running ingest must not bring a deleted file back as completed")
}

// A batch that arrives while a delete is in progress (its chunks already gone,
// the transaction not yet committed) waits for it, and then writes nothing.
// Without the lock it would read the file as still there and leave its chunks
// behind the delete.
func TestABatchDuringADeleteWaitsAndWritesNothing(t *testing.T) {
	e := newDeleteEnv(t)
	f := e.file(t, "racing.txt")
	require.NoError(t, e.insertChunks(f, 0, 2))

	tx, err := e.d.SqlDB.BeginTx(e.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck
	q := e.d.Queries.WithTx(tx)
	require.NoError(t, q.SoftDeleteIngestedFile(e.ctx, SoftDeleteIngestedFileParams{ID: f, UserID: e.user}))
	_, err = q.DeleteFileChunks(e.ctx, DeleteFileChunksParams{FileID: f, UserID: e.user})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- e.insertChunks(f, 10, 3) }()
	select {
	case err := <-done:
		t.Fatalf("the batch did not wait for the delete in progress (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, tx.Commit())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the batch is still waiting after the delete committed")
	}
	assert.Equal(t, 0, e.chunks(t, f), "the batch must not leave chunks behind the delete")
}

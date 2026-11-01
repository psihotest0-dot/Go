package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

func TestReingestFiles(t *testing.T) {
	e := newInsertOnlyRiver(t)
	ctx := context.Background()

	user, err := e.database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.database.Pool, user.ID) })

	add := func(name, status string, deleted bool, path string) uuid.UUID {
		f, err := e.database.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
			UserID: user.ID, FileName: name, FileHash: "hash-" + uuid.NewString(),
			MimeType: "text/plain", FileSize: 1, Status: status, LocalPath: path,
		})
		require.NoError(t, err)
		if deleted {
			_, err = e.database.SqlDB.ExecContext(ctx, `UPDATE ingested_files SET deleted_at = now() WHERE id = $1`, f.ID)
			require.NoError(t, err)
		}
		return f.ID
	}
	// A file with its original in storage, as every upload has by default.
	addFile := func(name, status string, deleted bool) uuid.UUID {
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, []byte("text"), 0o600))
		return add(name, status, deleted, path)
	}
	completed := addFile("done.pdf", "completed", false)
	// Finished, but the original is gone (KEEP_ORIGINALS=false, or deleted by hand).
	lost := add("lost.pdf", "completed", false, filepath.Join(t.TempDir(), "gone.pdf"))
	failed := addFile("failed.pdf", "failed", false)
	processing := addFile("busy.pdf", "processing", false)
	pending := addFile("new.pdf", "pending", false)
	deleted := addFile("gone.pdf", "completed", true)

	ingestJobs := func(id uuid.UUID) int {
		var n int
		require.NoError(t, e.pool.QueryRow(ctx,
			`SELECT count(*) FROM river_job WHERE kind = 'ingest_file' AND args->>'file_id' = $1`, id.String()).Scan(&n))
		return n
	}
	// The nil case below queues every finished file of the test database, so remove
	// everything this test queued, not only its own files' jobs.
	var lastID int64
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT COALESCE(MAX(id), 0) FROM river_job`).Scan(&lastID))
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM river_job WHERE kind = 'ingest_file' AND id > $1`, lastID)
	})

	// Named files: the finished ones are queued; a file that is still being
	// processed, a deleted one and one that does not exist are skipped; one whose
	// original is gone is left alone (an ingest would wipe its chunks and fail).
	res, err := ReingestFiles(ctx, e.database, e.client, []uuid.UUID{completed, failed, processing, deleted, uuid.New(), lost})
	require.NoError(t, err)
	assert.Equal(t, ReingestResult{Queued: 2, Skipped: 3, NoOriginal: 1}, res)
	assert.Zero(t, ingestJobs(lost), "no original, so nothing to ingest again")
	assert.Equal(t, 1, ingestJobs(completed))
	assert.Equal(t, 1, ingestJobs(failed))
	assert.Zero(t, ingestJobs(processing), "a second run would delete the chunks the first is writing")
	assert.Zero(t, ingestJobs(deleted))
	assert.Zero(t, ingestJobs(pending))

	// Asking again while the jobs wait does not queue them twice.
	res, err = ReingestFiles(ctx, e.database, e.client, []uuid.UUID{completed, failed})
	require.NoError(t, err)
	assert.Equal(t, ReingestResult{AlreadyDue: 2}, res)
	assert.Equal(t, 1, ingestJobs(completed))

	// nil means every finished file of the database, which the test does not
	// count (other tests leave files behind); it must at least include ours.
	_, err = ReingestFiles(ctx, e.database, e.client, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, ingestJobs(completed))
	assert.Zero(t, ingestJobs(lost))
	assert.Zero(t, ingestJobs(pending), "a pending file is not finished")
	assert.Zero(t, ingestJobs(deleted))
}

// An upload retried, or a re-ingest started while one waits, must not run the
// same file twice.
func TestIngestInsertIsUniquePerFile(t *testing.T) {
	e := newInsertOnlyRiver(t)
	ctx := context.Background()
	id := uuid.New()
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM river_job WHERE kind = 'ingest_file' AND args->>'file_id' = $1`, id.String())
	})

	first, err := e.client.Insert(ctx, IngestFileArgs{FileID: id}, IngestInsertOpts())
	require.NoError(t, err)
	second, err := e.client.Insert(ctx, IngestFileArgs{FileID: id}, IngestInsertOpts())
	require.NoError(t, err)

	assert.False(t, first.UniqueSkippedAsDuplicate)
	assert.True(t, second.UniqueSkippedAsDuplicate)
	assert.Equal(t, "ingest", first.Job.Queue)

	// A finished job does not block a later run.
	_, err = e.pool.Exec(ctx, `UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, first.Job.ID)
	require.NoError(t, err)
	third, err := e.client.Insert(ctx, IngestFileArgs{FileID: id}, IngestInsertOpts())
	require.NoError(t, err)
	assert.False(t, third.UniqueSkippedAsDuplicate)
}

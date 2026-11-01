package jobs

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// The worker used to skip at "chunks/5 rounded up, times 0.8", the startup scan
// enqueued at "chunks/5 rounded down, times 0.8" (in SQL): for 216 chunks 35
// against 34, so a file could be too complete to enqueue and not complete
// enough to skip. Both now ask distillationNeeded.
func TestDistillationThresholdIsOneFunction(t *testing.T) {
	cases := []struct {
		raw, concepts int
		target        int
		needed        bool
	}{
		{raw: 0, concepts: 0, target: 0, needed: false},
		{raw: 5, concepts: 0, target: 0, needed: false}, // one batch: nothing worth summarising
		{raw: 10, concepts: 0, target: 1, needed: true},
		{raw: 10, concepts: 1, target: 1, needed: false},
		{raw: 216, concepts: 34, target: 35, needed: true}, // the old scan said "complete" here
		{raw: 216, concepts: 35, target: 35, needed: false},
		{raw: 2356, concepts: 376, target: 377, needed: true},
	}
	for _, c := range cases {
		assert.Equal(t, c.target, distillationTarget(c.raw), "target for %d chunks", c.raw)
		assert.Equal(t, c.needed, distillationNeeded(c.raw, c.concepts), "%d chunks, %d summaries", c.raw, c.concepts)
	}
}

type riverTestEnv struct {
	database *db.Database
	pool     *pgxpool.Pool
	client   *river.Client[pgx.Tx]
}

// newInsertOnlyRiver opens a River client that can enqueue but never works, so
// the test sees exactly the jobs it inserted.
func newInsertOnlyRiver(t *testing.T) *riverTestEnv {
	t.Helper()
	dbURL := dbtest.ConnString(t)
	ctx := context.Background()

	database, err := db.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	pool, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	driver := riverpgxv5.New(pool)
	migrator, err := rivermigrate.New(driver, nil)
	require.NoError(t, err)
	_, err = migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	require.NoError(t, err)

	client, err := river.NewClient(driver, &river.Config{})
	require.NoError(t, err)

	// Remove only what this test enqueues.
	var lastID int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(MAX(id), 0) FROM river_job`).Scan(&lastID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM river_job WHERE id > $1 AND kind = 'distillation_worker'`, lastID)
	})
	return &riverTestEnv{database: database, pool: pool, client: client}
}

func (e *riverTestEnv) jobsFor(t *testing.T, fileID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'distillation_worker' AND args->>'file_id' = $1`, fileID.String()).Scan(&n))
	return n
}

func TestDistillationInsertIsUniquePerFile(t *testing.T) {
	e := newInsertOnlyRiver(t)
	ctx := context.Background()
	fileID := uuid.New()

	first, err := e.client.Insert(ctx, DistillationWorkerArgs{FileID: fileID}, distillationInsertOpts())
	require.NoError(t, err)
	assert.False(t, first.UniqueSkippedAsDuplicate)

	// An ingest and a startup scan (or two scans) racing for one file.
	second, err := e.client.Insert(ctx, DistillationWorkerArgs{FileID: fileID}, distillationInsertOpts())
	require.NoError(t, err)
	assert.True(t, second.UniqueSkippedAsDuplicate, "the second insert must be dropped as a duplicate")
	assert.Equal(t, first.Job.ID, second.Job.ID)
	assert.Equal(t, 1, e.jobsFor(t, fileID))

	// Another file is not affected.
	other := uuid.New()
	third, err := e.client.Insert(ctx, DistillationWorkerArgs{FileID: other}, distillationInsertOpts())
	require.NoError(t, err)
	assert.False(t, third.UniqueSkippedAsDuplicate)

	// A finished job must not block a later re-ingest of the same file.
	_, err = e.pool.Exec(ctx, `UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, first.Job.ID)
	require.NoError(t, err)
	again, err := e.client.Insert(ctx, DistillationWorkerArgs{FileID: fileID}, distillationInsertOpts())
	require.NoError(t, err)
	assert.False(t, again.UniqueSkippedAsDuplicate, "a completed job must not block a new one")
	assert.NotEqual(t, first.Job.ID, again.Job.ID)
}

func TestEnqueueIncompleteDistillations(t *testing.T) {
	e := newInsertOnlyRiver(t)
	ctx := context.Background()

	user, err := e.database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.database.Pool, user.ID) })

	// addFile creates a completed file with the given numbers of chunks.
	addFile := func(name, status string, raw, concepts int) db.IngestedFile {
		file, err := e.database.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
			UserID: user.ID, FileName: name, FileHash: "hash-" + uuid.NewString(),
			MimeType: "text/plain", FileSize: 1, Status: status, LocalPath: "storage/none",
		})
		require.NoError(t, err)
		vec := pgvector.NewVector(make([]float32, config.VectorDimension))
		for _, kind := range []struct {
			chunkType string
			n         int
		}{{"raw_text", raw}, {"concept_summary", concepts}} {
			_, err = e.database.SqlDB.ExecContext(ctx,
				`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
				 SELECT $1, $2, g + $5, 'text ' || g, $3, $4, '{}' FROM generate_series(0, $6 - 1) g`,
				file.ID, user.ID, vec, kind.chunkType, map[string]int{"raw_text": 0, "concept_summary": 1000}[kind.chunkType], kind.n)
			require.NoError(t, err)
		}
		return file
	}

	incomplete := addFile("incomplete.txt", "completed", 20, 0) // 4 batches, target 3
	partial := addFile("partial.txt", "completed", 20, 2)       // still short of 3
	complete := addFile("complete.txt", "completed", 20, 3)
	single := addFile("single.txt", "completed", 5, 0) // one batch: not worth summarising
	pending := addFile("pending.txt", "pending", 20, 0)
	deleted := addFile("deleted.txt", "completed", 20, 0)
	_, err = e.database.SqlDB.ExecContext(ctx, `UPDATE ingested_files SET deleted_at = now() WHERE id = $1`, deleted.ID)
	require.NoError(t, err)

	EnqueueIncompleteDistillations(ctx, e.client, e.database)

	assert.Equal(t, 1, e.jobsFor(t, incomplete.ID))
	assert.Equal(t, 1, e.jobsFor(t, partial.ID))
	assert.Zero(t, e.jobsFor(t, complete.ID))
	assert.Zero(t, e.jobsFor(t, single.ID))
	assert.Zero(t, e.jobsFor(t, pending.ID), "a file that is still being ingested has nothing to summarise yet")
	assert.Zero(t, e.jobsFor(t, deleted.ID), "a deleted file is not worth LLM time")

	// A second scan (a quick restart) must not queue the same files again.
	EnqueueIncompleteDistillations(ctx, e.client, e.database)
	assert.Equal(t, 1, e.jobsFor(t, incomplete.ID))
	assert.Equal(t, 1, e.jobsFor(t, partial.ID))
}

package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// batchEmbedder returns one vector per text, as the real embedder does.
type batchEmbedder struct{}

func (batchEmbedder) EmbedTexts(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, config.VectorDimension)
	}
	return out, nil
}

type ingestEnv struct {
	database *db.Database
	user     db.User
	worker   *IngestFileWorker
}

func newIngestEnv(t *testing.T) *ingestEnv {
	t.Helper()
	dbURL := dbtest.ConnString(t)
	t.Setenv("ENABLE_DISTILLATION", "false") // no River client in a unit test
	ctx := context.Background()

	database, err := db.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })

	return &ingestEnv{
		database: database,
		user:     user,
		worker:   NewIngestFileWorker(database, batchEmbedder{}, nil),
	}
}

// addFile stores content in a temp file and registers it for ingest.
func (e *ingestEnv) addFile(t *testing.T, name string, content []byte) db.IngestedFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, content, 0o600))

	file, err := e.database.Queries.CreateIngestedFile(context.Background(), db.CreateIngestedFileParams{
		UserID: e.user.ID, FileName: name, FileHash: "hash-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: int64(len(content)), Status: "pending", LocalPath: path,
	})
	require.NoError(t, err)
	return file
}

func (e *ingestEnv) status(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var status string
	require.NoError(t, e.database.SqlDB.QueryRowContext(context.Background(),
		`SELECT status FROM ingested_files WHERE id = $1`, id).Scan(&status))
	return status
}

func (e *ingestEnv) chunks(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	rows, err := e.database.SqlDB.QueryContext(context.Background(),
		`SELECT text FROM document_chunks WHERE file_id = $1 ORDER BY chunk_index`, id)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func ingestJob(id uuid.UUID, attempt, maxAttempts int) *river.Job[IngestFileArgs] {
	return &river.Job[IngestFileArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: maxAttempts},
		Args:   IngestFileArgs{FileID: id},
	}
}

func isCancelled(err error) bool {
	var cancel *river.JobCancelError
	return errors.As(err, &cancel)
}

// A NUL byte in the extracted text makes the INSERT into PostgreSQL fail, so the
// file used to end as "failed" after 25 identical retries.
func TestIngestWorkerStoresTextWithNULBytes(t *testing.T) {
	e := newIngestEnv(t)
	file := e.addFile(t, "nul.txt", []byte("Първи ред\x00 с нулев байт.\nВтори ред на документа."))

	require.NoError(t, e.worker.Work(context.Background(), ingestJob(file.ID, 1, 25)))

	assert.Equal(t, "completed", e.status(t, file.ID))
	chunks := e.chunks(t, file.ID)
	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		assert.False(t, strings.ContainsRune(c, 0), "chunk still has a NUL byte: %q", c)
	}
	assert.Contains(t, strings.Join(chunks, ""), "Първи ред с нулев байт.")

	// New chunks say which model made their vectors, so that a change of model
	// can tell what has to be recomputed.
	var wrong int
	require.NoError(t, e.database.SqlDB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM document_chunks WHERE file_id = $1 AND embedding_model IS DISTINCT FROM $2`,
		file.ID, config.DefaultEmbedModel).Scan(&wrong))
	assert.Zero(t, wrong, "every chunk must be marked with the current embedding model")
}

// An empty result used to leave a "completed" file with zero chunks, which looks
// like a successful import and can never be found in a search.
func TestIngestWorkerFailsOnEmptyResult(t *testing.T) {
	cases := map[string][]byte{
		"empty.txt":      {},
		"whitespace.txt": []byte(" \n\t\r\n "),
		"dots.txt":       []byte(strings.Repeat(".", 1000)),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			e := newIngestEnv(t)
			file := e.addFile(t, name, content)

			err := e.worker.Work(context.Background(), ingestJob(file.ID, 1, 25))

			require.Error(t, err)
			assert.True(t, isCancelled(err), "an empty file can never succeed, so River must not retry it: %v", err)
			assert.Equal(t, "failed", e.status(t, file.ID))
			assert.Empty(t, e.chunks(t, file.ID))
		})
	}
}

func TestIngestWorkerCancelsUnsupportedFormat(t *testing.T) {
	e := newIngestEnv(t)
	file := e.addFile(t, "logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>`))

	err := e.worker.Work(context.Background(), ingestJob(file.ID, 1, 25))

	require.Error(t, err)
	assert.True(t, isCancelled(err), "unsupported format must not be retried: %v", err)
	assert.Equal(t, "failed", e.status(t, file.ID))
}

// A job whose row no longer exists was retried 10-22 times after the incident of
// 2026-10-30; nothing can make the row reappear.
func TestIngestWorkerCancelsWhenTheFileRowIsGone(t *testing.T) {
	e := newIngestEnv(t)

	err := e.worker.Work(context.Background(), ingestJob(uuid.New(), 1, 25))

	require.Error(t, err)
	assert.True(t, isCancelled(err), "got %v", err)
}

// On shutdown River cancels the context and runs the job again later. The file
// must not be shown as "failed" in between.
func TestIngestWorkerKeepsProcessingStateWhenInterrupted(t *testing.T) {
	e := newIngestEnv(t)
	file := e.addFile(t, "book.txt", []byte(strings.Repeat("Изречение за проверка. ", 100)))

	ctx, cancel := context.WithCancel(context.Background())
	e.worker.embedder = cancelingEmbedder{cancel: cancel}

	err := e.worker.Work(ctx, ingestJob(file.ID, 1, 25))

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, isCancelled(err))
	assert.Equal(t, "processing", e.status(t, file.ID))

	// On the last attempt nothing will retry, so the failure must be recorded.
	ctx2, cancel2 := context.WithCancel(context.Background())
	e.worker.embedder = cancelingEmbedder{cancel: cancel2}
	err = e.worker.Work(ctx2, ingestJob(file.ID, 25, 25))
	require.Error(t, err)
	assert.Equal(t, "failed", e.status(t, file.ID))
}

type cancelingEmbedder struct{ cancel context.CancelFunc }

func (c cancelingEmbedder) EmbedTexts(ctx context.Context, _ []string, _ bool) ([][]float32, error) {
	c.cancel()
	return nil, ctx.Err()
}

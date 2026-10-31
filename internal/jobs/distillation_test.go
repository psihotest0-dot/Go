package jobs

import (
	"context"
	"testing"

	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// cancelOnCallLLM behaves like the real client when the process is shutting
// down: the first call cancels the job context and fails with its error.
type cancelOnCallLLM struct {
	mockLLMClient
	cancel context.CancelFunc
	calls  int
}

func (c *cancelOnCallLLM) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error) {
	c.calls++
	c.cancel()
	return openai.ChatCompletionResponse{}, ctx.Err()
}

// On Ctrl+C River cancels the job context. The worker used to log one error per
// remaining batch and then return nil, so River recorded the job as completed
// although most of the file was never distilled. It must stop at once and
// return the context error, which River turns into "run again" without using up
// an attempt.
func TestDistillationWorkerStopsWhenContextIsCancelled(t *testing.T) {
	dbURL := dbtest.ConnString(t)
	t.Chdir("../..")

	base := context.Background()
	database, err := db.Connect(base, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	user, err := database.Queries.CreateUser(base)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	})

	file, err := database.Queries.CreateIngestedFile(base, db.CreateIngestedFileParams{
		UserID: user.ID, FileName: "book.txt", FileHash: "hash-" + user.ID.String(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)

	// 30 raw chunks are 6 batches of 5.
	_, err = database.SqlDB.ExecContext(base,
		`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
		 SELECT $1, $2, g, 'text ' || g, $3, 'raw_text', '{}' FROM generate_series(0, 29) g`,
		file.ID, user.ID, pgvector.NewVector(make([]float32, config.VectorDimension)))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	llmStub := &cancelOnCallLLM{cancel: cancel}
	worker := NewDistillationWorker(database, llmStub, &mockEmbedder{})

	err = worker.Work(ctx, &river.Job[DistillationWorkerArgs]{Args: DistillationWorkerArgs{FileID: file.ID}})

	require.ErrorIs(t, err, context.Canceled, "River only treats a context error as an interrupted job")
	assert.Equal(t, 1, llmStub.calls, "no further batch may be attempted after the cancellation")

	var concepts int
	require.NoError(t, database.SqlDB.QueryRowContext(base,
		`SELECT count(*) FROM document_chunks WHERE file_id = $1 AND chunk_type = 'concept_summary'`, file.ID).Scan(&concepts))
	assert.Zero(t, concepts)
}

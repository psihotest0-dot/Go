package jobs

import (
	"context"
	"errors"
	"testing"

	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// deleteOnCallLLM deletes the file while the model is "thinking", the way a user
// can delete a book during its distillation, and then answers normally.
type deleteOnCallLLM struct {
	mockLLMClient
	delete func()
	calls  int
}

func (c *deleteOnCallLLM) ChatWithJSONSchema(context.Context, openai.ChatCompletionRequest, interface{}) (openai.ChatCompletionResponse, error) {
	c.calls++
	if c.calls == 1 {
		c.delete()
	}
	return modelReply(goodSummaryReply, openai.FinishReasonStop), nil
}

// A file deleted during its distillation gets no summary after the delete (the
// delete took its chunks; a summary written now would be left behind), and the
// job stops instead of distilling the rest of a deleted book.
func TestDistillationWritesNothingIntoAFileDeletedMeanwhile(t *testing.T) {
	dbURL := dbtest.ConnString(t)
	t.Chdir("../..")
	ctx := context.Background()
	database, err := db.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })

	file, err := database.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: user.ID, FileName: "book.txt", FileHash: "hash-" + user.ID.String(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)
	_, err = database.SqlDB.ExecContext(ctx,
		`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
		 SELECT $1, $2, g, 'text ' || g, $3, 'raw_text', '{}' FROM generate_series(0, 29) g`,
		file.ID, user.ID, pgvector.NewVector(make([]float32, config.VectorDimension)))
	require.NoError(t, err)

	llmStub := &deleteOnCallLLM{delete: func() {
		_, err := database.DeleteFile(ctx, user.ID, file.ID)
		require.NoError(t, err)
	}}
	worker := NewDistillationWorker(database, llmStub, &mockEmbedder{})
	err = worker.Work(ctx, &river.Job[DistillationWorkerArgs]{Args: DistillationWorkerArgs{FileID: file.ID}})

	var cancelled *rivertype.JobCancelError
	require.True(t, errors.As(err, &cancelled), "the job must stop for good, got %v", err)
	assert.Equal(t, 1, llmStub.calls, "no further batch of a deleted book may be distilled")

	var left int
	require.NoError(t, database.SqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM document_chunks WHERE file_id = $1`, file.ID).Scan(&left))
	assert.Zero(t, left, "nothing of a deleted file may be left behind")
}

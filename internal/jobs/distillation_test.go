package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
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
		dbtest.DeleteUser(t, database.Pool, user.ID)
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

// scriptedLLM answers the n-th ChatWithJSONSchema call with the n-th of its
// replies; a call past the end gets an empty reply (no choices at all).
type scriptedLLM struct {
	mockLLMClient
	replies []openai.ChatCompletionResponse
	calls   int
}

func (s *scriptedLLM) ChatWithJSONSchema(_ context.Context, _ openai.ChatCompletionRequest, _ interface{}) (openai.ChatCompletionResponse, error) {
	s.calls++
	if s.calls > len(s.replies) {
		return openai.ChatCompletionResponse{}, nil
	}
	return s.replies[s.calls-1], nil
}

func modelReply(content string, finish openai.FinishReason) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{
		Message:      openai.ChatCompletionMessage{Content: content},
		FinishReason: finish,
	}}}
}

const goodSummaryReply = `{"summary": "A short summary of the batch.", "themes": ["one", "two", "three"]}`

// loopedReply is what the model sent on 2026-11-05: a normal start, then a loop
// until max_tokens cut it off in the middle of the string.
var loopedReply = "{\n  \"summary\": \"The text provides traditional Bulgarian culinary instructions for preparing three dishes: stuffed peppers, roasted lamb, and stuffed lamb " +
	strings.Repeat("and-", 60)

func TestParseDistillationReply(t *testing.T) {
	tests := []struct {
		name    string
		resp    openai.ChatCompletionResponse
		summary string
		errPart string
	}{
		{"a good reply", modelReply(goodSummaryReply, openai.FinishReasonStop), "A short summary of the batch.", ""},
		{"a reply in a markdown fence", modelReply("```json\n"+goodSummaryReply+"```", openai.FinishReasonStop), "A short summary of the batch.", ""},
		{"a reply cut off at max_tokens", modelReply(loopedReply, openai.FinishReasonLength), "", "unexpected end of JSON input"},
		{"an empty reply", modelReply("", openai.FinishReasonStop), "", "unexpected end of JSON input"},
		{"an empty summary", modelReply(`{"summary": "  ", "themes": []}`, openai.FinishReasonStop), "", "the summary is empty"},
		{"no choices", openai.ChatCompletionResponse{}, "", "no choices"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDistillationReply(tt.resp)
			if tt.errPart != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errPart)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.summary, got.Summary)
		})
	}
}

func TestReplyLogFieldsShowWhyTheReplyEnded(t *testing.T) {
	fields := replyLogFields(modelReply(loopedReply, openai.FinishReasonLength))
	assert.Contains(t, fields, "length", "finish_reason tells a reply cut off at max_tokens from one that ended on its own")
	for _, f := range fields {
		if s, ok := f.(string); ok {
			assert.LessOrEqual(t, len([]rune(s)), maxLoggedModelRunes+3, "the reply is cut for the log")
		}
	}
	assert.Nil(t, replyLogFields(openai.ChatCompletionResponse{}))
}

// distillationFixture is a user with one completed file of rawChunks raw chunks.
func distillationFixture(t *testing.T, rawChunks int) (*db.Database, uuid.UUID) {
	t.Helper()
	dbURL := dbtest.ConnString(t)
	t.Chdir("../..")

	base := context.Background()
	database, err := db.Connect(base, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	user, err := database.Queries.CreateUser(base)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })

	file, err := database.Queries.CreateIngestedFile(base, db.CreateIngestedFileParams{
		UserID: user.ID, FileName: "book.txt", FileHash: "hash-" + user.ID.String(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)

	_, err = database.SqlDB.ExecContext(base,
		`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
		 SELECT $1, $2, g, 'text ' || g, $3, 'raw_text', '{}' FROM generate_series(0, $4 - 1) g`,
		file.ID, user.ID, pgvector.NewVector(make([]float32, config.VectorDimension)), rawChunks)
	require.NoError(t, err)
	return database, file.ID
}

func conceptSummaries(t *testing.T, database *db.Database, fileID uuid.UUID) []string {
	t.Helper()
	rows, err := database.SqlDB.QueryContext(context.Background(),
		`SELECT text FROM document_chunks WHERE file_id = $1 AND chunk_type = 'concept_summary' ORDER BY chunk_index`, fileID)
	require.NoError(t, err)
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var text string
		require.NoError(t, rows.Scan(&text))
		texts = append(texts, text)
	}
	require.NoError(t, rows.Err())
	return texts
}

// A reply that loops until max_tokens leaves unfinished JSON. The same batch asked
// again gives a normal summary, so the batch must not be lost.
func TestDistillationAsksAgainWhenTheReplyIsCutOff(t *testing.T) {
	// Two batches: with a single one the target of summaries is 0 (80 % of 1,
	// rounded down) and the file would not be distilled at all.
	database, fileID := distillationFixture(t, 10)
	llmStub := &scriptedLLM{replies: []openai.ChatCompletionResponse{
		modelReply(loopedReply, openai.FinishReasonLength),
		modelReply(goodSummaryReply, openai.FinishReasonStop),
		modelReply(goodSummaryReply, openai.FinishReasonStop),
	}}
	worker := NewDistillationWorker(database, llmStub, &mockEmbedder{})

	err := worker.Work(context.Background(), &river.Job[DistillationWorkerArgs]{Args: DistillationWorkerArgs{FileID: fileID}})

	require.NoError(t, err)
	assert.Equal(t, 3, llmStub.calls, "the first batch was asked for again, the second one needed one call")
	assert.Equal(t,
		[]string{"A short summary of the batch.", "A short summary of the batch."},
		conceptSummaries(t, database, fileID), "no batch was lost")
}

// A batch that fails every attempt is counted as failed and the next batch is
// still done; the attempts are bounded.
func TestDistillationGoesOnAfterABatchThatFailsEveryAttempt(t *testing.T) {
	database, fileID := distillationFixture(t, 10) // two batches
	llmStub := &scriptedLLM{replies: []openai.ChatCompletionResponse{
		modelReply(loopedReply, openai.FinishReasonLength),
		modelReply(loopedReply, openai.FinishReasonLength),
		modelReply(goodSummaryReply, openai.FinishReasonStop),
	}}
	worker := NewDistillationWorker(database, llmStub, &mockEmbedder{})

	err := worker.Work(context.Background(), &river.Job[DistillationWorkerArgs]{Args: DistillationWorkerArgs{FileID: fileID}})

	require.NoError(t, err, "one good batch out of two is a partial success")
	assert.Equal(t, distillationAttempts+1, llmStub.calls, "the first batch used all its attempts, the second one needed one")
	assert.Len(t, conceptSummaries(t, database, fileID), 1)
}

// With nothing usable at all the job fails, so River records it and runs it again.
func TestDistillationFailsWhenNoBatchGivesAUsableReply(t *testing.T) {
	database, fileID := distillationFixture(t, 10) // two batches
	llmStub := &scriptedLLM{}                      // every call gets a reply without choices
	worker := NewDistillationWorker(database, llmStub, &mockEmbedder{})

	err := worker.Work(context.Background(), &river.Job[DistillationWorkerArgs]{Args: DistillationWorkerArgs{FileID: fileID}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 2 distillation batches failed")
	assert.Equal(t, 2*distillationAttempts, llmStub.calls)
	assert.Empty(t, conceptSummaries(t, database, fileID), "an empty reply is not stored as an empty concept")
}

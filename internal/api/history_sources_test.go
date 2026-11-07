package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
)

// The history shows a stored source as a distance. Answers stored before the
// distance have the score as (1 - distance) * 100 for a summary - which the history
// used to multiply by 100 once more, so it showed 8 200 % - and as a raw RRF score
// for a passage; those are turned into what the live stream shows now.
func TestGetMessagesShowsStoredSourcesAsDistances(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	ctx := context.Background()

	file, err := database.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: userID, FileName: "book.txt", FileHash: "hash-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)

	var arg db.CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	for i, kind := range []string{"concept_summary", "concept_summary", "raw_text", "raw_text"} {
		arg.FileIds = append(arg.FileIds, file.ID)
		arg.UserIds = append(arg.UserIds, userID)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, fmt.Sprintf("chunk %d", i))
		arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(make([]float32, config.VectorDimension)))
		arg.ChunkTypes = append(arg.ChunkTypes, kind)
		arg.Metadatas = append(arg.Metadatas, "{}")
	}
	require.NoError(t, database.Queries.CreateDocumentChunksBatch(ctx, arg))
	chunks, err := database.Queries.GetDocumentChunksByFile(ctx, db.GetDocumentChunksByFileParams{FileID: file.ID, UserID: userID})
	require.NoError(t, err)
	require.Len(t, chunks, 4)
	byText := map[string]uuid.UUID{}
	for _, c := range chunks {
		byText[c.Text] = c.ID
	}

	stored := []api.DBChunkInfo{
		{ChunkID: byText["chunk 0"], Score: 0.152, ScoreType: api.ScoreTypeDistance}, // since the distance
		{ChunkID: byText["chunk 1"], Score: 84.8},                                    // before: (1 - 0.152) * 100
		{ChunkID: byText["chunk 2"], Score: 0.0165},                                  // before: a raw RRF score
		{ChunkID: byText["chunk 3"]},                                                 // a passage the search_documents tool found: no measure
	}
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	_, err = database.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID: userID, SessionID: "history", Role: "assistant", Content: "answer",
		SourceChunkIds: pqtype.NullRawMessage{RawMessage: raw, Valid: true},
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/messages?session_id=history", nil)
	req = withUser(req, userID)
	w := httptest.NewRecorder()
	apiService.GetMessagesHandler(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var msgs []api.MessageDTO
	require.NoError(t, json.NewDecoder(w.Body).Decode(&msgs))
	require.Len(t, msgs, 1)

	got := map[string]api.SourceInfo{}
	for _, s := range msgs[0].Sources {
		got[s.ContentSnippet] = s
	}
	require.Len(t, got, 4)

	assert.Equal(t, 0.152, got["chunk 0"].Score)
	assert.Equal(t, api.ScoreTypeDistance, got["chunk 0"].ScoreType)
	assert.Equal(t, "concept", got["chunk 0"].SourceType)

	assert.InDelta(t, 0.152, got["chunk 1"].Score, 1e-9, "an old percentage turns back into the distance it was made from")
	assert.Equal(t, api.ScoreTypeDistance, got["chunk 1"].ScoreType)

	assert.InDelta(t, 50.0, got["chunk 2"].Score, 1e-9)
	assert.Equal(t, api.ScoreTypeRRF, got["chunk 2"].ScoreType)

	assert.Zero(t, got["chunk 3"].Score, "a passage stored with no measure has none, instead of turning into an RRF score of 0")
	assert.Empty(t, got["chunk 3"].ScoreType)
	assert.Equal(t, "text", got["chunk 3"].SourceType)
}

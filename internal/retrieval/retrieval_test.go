package retrieval_test

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

func TestRetrieval_CreateMemory_Mock(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	content := "Test memory content"

	mock.ExpectQuery(`(?i)INSERT INTO memories`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "content", "status", "embedding", "embedding_model", "source_message_id", "importance", "metadata", "content_hash", "created_at", "updated_at", "last_used_at"}).
			AddRow(uuid.New(), userID, content, "active", "[0,0]", "paraphrase-multilingual-MiniLM-L12-v2", nil, 5, nil, "hash", nil, nil, nil))

	mem, err := svc.CreateMemory(context.Background(), userID, content, 5, "active", pqtype.NullRawMessage{})
	require.NoError(t, err)
	assert.Equal(t, content, mem.Content)
	assert.Equal(t, userID, mem.UserID)
}

func TestRetrieval_SearchSimilar_Mock(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	query := "Find test fact"

    mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}).
			AddRow(uuid.New(), "This is a test fact.", 1))

    mock.ExpectQuery(`(?i).*WITH chunk_vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}).
			AddRow(uuid.New(), "Chunk fact.", "file.txt", 1))

	memories, err := svc.SearchMemories(context.Background(), userID, query, 10)
	require.NoError(t, err)
	assert.Len(t, memories, 1)
	assert.Equal(t, "This is a test fact.", memories[0].Content)
}

package retrieval_test

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
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
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	content := "This is a test fact."

	// Expectations
	emptyVec := pgvector.NewVector(make([]float32, 384))
	mock.ExpectQuery(`INSERT INTO memories`).
		WithArgs(userID, content, "active", sqlmock.AnyArg(), "paraphrase-multilingual-MiniLM-L12-v2", nil, 5, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "content", "status", "embedding", "embedding_model", "source_message_id", "importance", "metadata", "content_hash", "created_at", "updated_at", "last_used_at"}).
			AddRow(uuid.New(), userID, content, "active", emptyVec, "paraphrase-multilingual-MiniLM-L12-v2", nil, 5, nil, "hash", nil, nil, nil))

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
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "Find test fact"

	// Adjust mock to expect the new SearchHybridMemories query pattern
	mock.ExpectQuery(`(?i).*WITH\s+memory_vector_search AS.*`).
		WithArgs(10, sqlmock.AnyArg(), userID, query, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "source_type", "score"}).
			AddRow(uuid.New(), "This is a test fact.", "memory", 1))

	memories, err := svc.SearchSimilar(context.Background(), userID, query, 10)
	require.NoError(t, err)
	assert.Len(t, memories, 1)
	assert.Equal(t, "This is a test fact.", memories[0].Content)
}

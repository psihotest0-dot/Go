package retrieval_test

import "fmt"

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
			AddRow(uuid.New(), userID, content, "active", "[0,0]", "intfloat/multilingual-e5-small", nil, 5, nil, "hash", nil, nil, nil))

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

func TestRetrieval_SearchChunks_Mock(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	query := "Find test chunk"

    mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}).
			AddRow(uuid.New(), "This is a test fact.", 1))

    mock.ExpectQuery(`(?i).*WITH chunk_vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}).
			AddRow(uuid.New(), "Chunk fact.", "file.txt", 1))

	chunks, err := svc.SearchChunks(context.Background(), userID, query, 10)
	require.NoError(t, err)
	assert.Len(t, chunks, 1)
	assert.Equal(t, "Chunk fact.", chunks[0].Content)
}

func TestRetrieval_SearchAll_Fallback_EmptyResult(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	// Make embedder return empty slice
	embedder := &llm.MockEmbedderWithConfig{EmptyVectors: true}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	query := "Empty vectors test"

	mock.MatchExpectationsInOrder(false)

	// Fallback uses SearchTextMemories and SearchTextChunks
	mock.ExpectQuery(`(?i)SELECT m.id, m.content`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}).
			AddRow(uuid.New(), "Text fallback fact", 0.9))

	mock.ExpectQuery(`(?i)SELECT c.id, c.text as content, f.file_name`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}).
			AddRow(uuid.New(), "Text fallback chunk", "file2.txt", 0.8))

	memories, chunks, err := svc.SearchAll(context.Background(), userID, query, 10)
	require.NoError(t, err)
	assert.Len(t, memories, 1)
	assert.Equal(t, "Text fallback fact", memories[0].Content)
	assert.Len(t, chunks, 1)
	assert.Equal(t, "Text fallback chunk", chunks[0].Content)
}

func TestRetrieval_SearchAll_Fallback_Error(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedderWithConfig{Err: fmt.Errorf("mock error")}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	query := "Error vectors test"

	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`(?i)SELECT m.id, m.content`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}).
			AddRow(uuid.New(), "Text fallback fact", 0.9))

	mock.ExpectQuery(`(?i)SELECT c.id, c.text as content, f.file_name`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}).
			AddRow(uuid.New(), "Text fallback chunk", "file2.txt", 0.8))

	memories, chunks, err := svc.SearchAll(context.Background(), userID, query, 10)
	require.NoError(t, err)
	assert.Len(t, memories, 1)
	assert.Len(t, chunks, 1)
}

func TestRetrieval_SearchAll_InvalidDimension(t *testing.T) {
	mockDB, _, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedderWithConfig{Dim: 128} // Invalid dimension
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	_, _, err = svc.SearchAll(context.Background(), userID, "test", 10)
	require.ErrorContains(t, err, "invalid vector dimension")
}

func TestRetrieval_CreateMemory_EmptyContent(t *testing.T) {
	svc := retrieval.NewRetrievalService(nil, nil, nil)
	userID := uuid.New()

	_, err := svc.CreateMemory(context.Background(), userID, "   ", 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "cannot create memory with empty content")
}

func TestRetrieval_CreateMemory_EmbedderError(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{Err: fmt.Errorf("embed error")}
	svc := retrieval.NewRetrievalService(nil, embedder, nil)
	userID := uuid.New()

	_, err := svc.CreateMemory(context.Background(), userID, "valid content", 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "failed to generate embedding")
}

func TestRetrieval_CreateMemory_EmbedderEmpty(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{EmptyVectors: true}
	svc := retrieval.NewRetrievalService(nil, embedder, nil)
	userID := uuid.New()

	_, err := svc.CreateMemory(context.Background(), userID, "valid content", 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "no embedding returned")
}

func TestRetrieval_CreateMemory_InvalidDimension(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{Dim: 128}
	svc := retrieval.NewRetrievalService(nil, embedder, nil)
	userID := uuid.New()

	_, err := svc.CreateMemory(context.Background(), userID, "valid content", 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "invalid vector dimension")
}

func TestRetrieval_CreateMemory_DBError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	content := "Test db error"

	mock.ExpectQuery(`(?i)INSERT INTO memories`).WillReturnError(fmt.Errorf("db fail"))

	_, err = svc.CreateMemory(context.Background(), userID, content, 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "failed to create memory")
}

func TestRetrieval_SearchAll_SearchError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	query := "Test search error"

	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).WillReturnError(fmt.Errorf("db search fail"))
    mock.ExpectQuery(`(?i).*WITH chunk_vector_search AS.*`).WillReturnError(fmt.Errorf("db search fail"))

	_, _, err = svc.SearchAll(context.Background(), userID, query, 10)
	require.ErrorContains(t, err, "db search fail")
}

func TestRetrieval_SearchAll_WithBoost(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedder{}
    catMgr, err := retrieval.NewCategoryManager(context.Background(), embedder)
    require.NoError(t, err)

	svc := retrieval.NewRetrievalService(d, embedder, catMgr)

	userID := uuid.New()
	query := "The treatment of acute viral infections requires antiviral therapy and rest." // Matches medicine perfectly in mock

	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}).
			AddRow(uuid.New(), "This is a test fact.", 1))

    mock.ExpectQuery(`(?i).*WITH chunk_vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}).
			AddRow(uuid.New(), "Chunk fact.", "file.txt", 1))

	memories, chunks, err := svc.SearchAll(context.Background(), userID, query, 10)
	require.NoError(t, err)
	assert.Len(t, memories, 1)
	assert.Len(t, chunks, 1)
}

func TestRetrieval_CreateMemory_ConflictError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	content := "Test conflict db error"

	mock.ExpectQuery(`(?i)INSERT INTO memories`).WillReturnError(db.ErrMemoryExists)

	_, err = svc.CreateMemory(context.Background(), userID, content, 5, "active", pqtype.NullRawMessage{})
	require.ErrorIs(t, err, db.ErrMemoryExists)
}

func TestRetrieval_SearchAll_Fallback_TextSearchError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedderWithConfig{EmptyVectors: true}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	query := "Empty vectors test"

	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`(?i)SELECT m.id, m.content`).
		WillReturnError(fmt.Errorf("text fallback fail"))

	mock.ExpectQuery(`(?i)SELECT c.id, c.text as content, f.file_name`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}))

	_, _, err = svc.SearchAll(context.Background(), userID, query, 10)
	require.ErrorContains(t, err, "text fallback fail")
}

func TestRetrieval_SearchAll_Fallback_TextSearchChunksError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{Queries: q}

	embedder := &llm.MockEmbedderWithConfig{EmptyVectors: true}
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	userID := uuid.New()
	query := "Empty vectors chunk test"

	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`(?i)SELECT m.id, m.content`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}))

	mock.ExpectQuery(`(?i)SELECT c.id, c.text as content, f.file_name`).
		WillReturnError(fmt.Errorf("text fallback chunk fail"))

	_, _, err = svc.SearchAll(context.Background(), userID, query, 10)
	require.ErrorContains(t, err, "text fallback chunk fail")
}

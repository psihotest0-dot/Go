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

// expectSearchTx expects the transaction db.WithSearchSettings opens around a
// similarity search: begin, the set_config call, and the rollback at the end.
func expectSearchTx(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(`set_config`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
}

// expectEmptyConceptSearch expects the concept summary search that a vector
// search runs first, and answers it with no rows.
func expectEmptyConceptSearch(mock sqlmock.Sqlmock) {
	expectSearchTx(mock)
	mock.ExpectQuery(`(?i)SELECT v\.id, v\.text AS content`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "metadata", "distance"}))
}

func TestRetrieval_CreateMemory_Mock(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder)

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
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "Find test fact"

    mock.MatchExpectationsInOrder(false)

	expectEmptyConceptSearch(mock)
	expectSearchTx(mock) // the chunk search

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}).
			AddRow(uuid.New(), "This is a test fact.", 1))

    mock.ExpectQuery(`(?i)chunk_vector_search AS`).
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
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "Find test chunk"

    mock.MatchExpectationsInOrder(false)

	expectEmptyConceptSearch(mock)
	expectSearchTx(mock) // the chunk search

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}).
			AddRow(uuid.New(), "This is a test fact.", 1))

    mock.ExpectQuery(`(?i)chunk_vector_search AS`).
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
	d := &db.Database{SqlDB: mockDB, Queries: q}

	// Make embedder return empty slice
	embedder := &llm.MockEmbedderWithConfig{EmptyVectors: true}
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "Empty vectors test"

	mock.MatchExpectationsInOrder(false)

	expectSearchTx(mock) // the chunk fallback

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
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedderWithConfig{Err: fmt.Errorf("mock error")}
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "Error vectors test"

	mock.MatchExpectationsInOrder(false)

	expectSearchTx(mock) // the chunk fallback

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
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedderWithConfig{Dim: 128} // Invalid dimension
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	_, _, err = svc.SearchAll(context.Background(), userID, "test", 10)
	require.ErrorContains(t, err, "invalid vector dimension")
}

func TestRetrieval_CreateMemory_EmptyContent(t *testing.T) {
	svc := retrieval.NewRetrievalService(nil, nil)
	userID := uuid.New()

	_, err := svc.CreateMemory(context.Background(), userID, "   ", 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "cannot create memory with empty content")
}

func TestRetrieval_CreateMemory_EmbedderError(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{Err: fmt.Errorf("embed error")}
	svc := retrieval.NewRetrievalService(nil, embedder)
	userID := uuid.New()

	_, err := svc.CreateMemory(context.Background(), userID, "valid content", 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "failed to generate embedding")
}

func TestRetrieval_CreateMemory_EmbedderEmpty(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{EmptyVectors: true}
	svc := retrieval.NewRetrievalService(nil, embedder)
	userID := uuid.New()

	_, err := svc.CreateMemory(context.Background(), userID, "valid content", 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "no embedding returned")
}

func TestRetrieval_CreateMemory_InvalidDimension(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{Dim: 128}
	svc := retrieval.NewRetrievalService(nil, embedder)
	userID := uuid.New()

	_, err := svc.CreateMemory(context.Background(), userID, "valid content", 5, "active", pqtype.NullRawMessage{})
	require.ErrorContains(t, err, "invalid vector dimension")
}

func TestRetrieval_CreateMemory_DBError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder)

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
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "Test search error"

	mock.MatchExpectationsInOrder(false)

	expectEmptyConceptSearch(mock)
	expectSearchTx(mock) // the chunk search

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).WillReturnError(fmt.Errorf("db search fail"))
    mock.ExpectQuery(`(?i)chunk_vector_search AS`).WillReturnError(fmt.Errorf("db search fail"))

	_, _, err = svc.SearchAll(context.Background(), userID, query, 10)
	require.ErrorContains(t, err, "db search fail")
}

func TestRetrieval_SearchAll_MemoriesAndChunks(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "The treatment of acute viral infections requires antiviral therapy and rest."

	mock.MatchExpectationsInOrder(false)

	expectEmptyConceptSearch(mock)
	expectSearchTx(mock) // the chunk search

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}).
			AddRow(uuid.New(), "This is a test fact.", 1))

    mock.ExpectQuery(`(?i)chunk_vector_search AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}).
			AddRow(uuid.New(), "Chunk fact.", "file.txt", 1))

	memories, chunks, err := svc.SearchAll(context.Background(), userID, query, 10)
	require.NoError(t, err)
	assert.Len(t, memories, 1)
	assert.Len(t, chunks, 1)
}

func TestRetrieval_SearchAll_IncludesConceptChunks(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	d := &db.Database{SqlDB: mockDB, Queries: db.New(mockDB)}
	svc := retrieval.NewRetrievalService(d, &llm.MockEmbedder{})

	conceptID := uuid.New()
	mock.MatchExpectationsInOrder(false)

	expectSearchTx(mock) // the concept search
	expectSearchTx(mock) // the chunk search

	mock.ExpectQuery(`(?i)chunk_type = 'concept_summary'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "metadata", "distance"}).
			AddRow(conceptID, "Concept summary.", "book.txt", []byte(`{"themes":["alpha"]}`), 0.1))

	mock.ExpectQuery(`(?i).*WITH vector_search AS.*`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}))

	mock.ExpectQuery(`(?i)chunk_vector_search AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}).
			AddRow(uuid.New(), "Chunk fact.", "file.txt", 1).
			AddRow(conceptID, "Concept summary.", "book.txt", 1))

	_, chunks, err := svc.SearchAll(context.Background(), uuid.New(), "query", 10)
	require.NoError(t, err)
	require.Len(t, chunks, 2, "concept result must be kept and not duplicated by the hybrid chunk hit")
	assert.Equal(t, "concept", chunks[0].SourceType)
	assert.Equal(t, conceptID, chunks[0].ID)
	assert.Equal(t, "chunk", chunks[1].SourceType)
}

func TestRetrieval_CreateMemory_ConflictError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := db.New(mockDB)
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedder{}
	svc := retrieval.NewRetrievalService(d, embedder)

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
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedderWithConfig{EmptyVectors: true}
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "Empty vectors test"

	mock.MatchExpectationsInOrder(false)

	expectSearchTx(mock) // the chunk fallback

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
	d := &db.Database{SqlDB: mockDB, Queries: q}

	embedder := &llm.MockEmbedderWithConfig{EmptyVectors: true}
	svc := retrieval.NewRetrievalService(d, embedder)

	userID := uuid.New()
	query := "Empty vectors chunk test"

	mock.MatchExpectationsInOrder(false)

	expectSearchTx(mock) // the chunk fallback

	mock.ExpectQuery(`(?i)SELECT m.id, m.content`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "score"}))

	mock.ExpectQuery(`(?i)SELECT c.id, c.text as content, f.file_name`).
		WillReturnError(fmt.Errorf("text fallback chunk fail"))

	_, _, err = svc.SearchAll(context.Background(), userID, query, 10)
	require.ErrorContains(t, err, "text fallback chunk fail")
}

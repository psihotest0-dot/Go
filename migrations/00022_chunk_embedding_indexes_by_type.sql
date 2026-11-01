-- +goose NO TRANSACTION
-- +goose Up
-- One HNSW index per chunk type instead of one over the whole table.
--
-- The search queries take the nearest N vectors from the index and only then
-- filter by type (pgvector 0.5 filters after the scan). With one index over raw
-- chunks and concept summaries together, the summaries - written in another
-- language than the questions - sat hundreds of places behind the raw chunks and
-- were never among the nearest 200: the concept search returned nothing (a median
-- of 1 303 raw chunks were closer to the summary than the question's best match).
-- A partial index holds only its type, and the planner uses it when the query
-- repeats its WHERE clause (chunk_type = '...'), so each search sees only the
-- rows it is about.
--
-- CONCURRENTLY cannot run in a transaction, hence NO TRANSACTION above. An
-- interrupted build leaves an INVALID index, so any leftover is dropped first.
DROP INDEX CONCURRENTLY IF EXISTS idx_document_chunks_raw_embedding;
DROP INDEX CONCURRENTLY IF EXISTS idx_document_chunks_concept_embedding;
CREATE INDEX CONCURRENTLY idx_document_chunks_raw_embedding
    ON document_chunks USING hnsw (embedding vector_cosine_ops) WHERE chunk_type = 'raw_text';
CREATE INDEX CONCURRENTLY idx_document_chunks_concept_embedding
    ON document_chunks USING hnsw (embedding vector_cosine_ops) WHERE chunk_type = 'concept_summary';
DROP INDEX CONCURRENTLY IF EXISTS idx_document_chunks_embedding;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_document_chunks_embedding;
CREATE INDEX CONCURRENTLY idx_document_chunks_embedding
    ON document_chunks USING hnsw (embedding vector_cosine_ops);
DROP INDEX CONCURRENTLY IF EXISTS idx_document_chunks_raw_embedding;
DROP INDEX CONCURRENTLY IF EXISTS idx_document_chunks_concept_embedding;

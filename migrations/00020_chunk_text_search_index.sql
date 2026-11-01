-- +goose NO TRANSACTION
-- +goose Up
-- Full-text index on the chunk text, for the lexical half of the hybrid search
-- (SearchHybridChunks, SearchTextChunks). Without it every text search read and
-- scored every chunk of the library (6.6 s on 250 000 chunks). The expression
-- must be written exactly as in the queries - to_tsvector('simple', text) - or
-- the planner does not use the index.
--
-- CREATE INDEX CONCURRENTLY does not block writes while it builds, but it cannot
-- run inside a transaction, hence NO TRANSACTION above. If the build is
-- interrupted PostgreSQL leaves an INVALID index behind; IF NOT EXISTS would then
-- skip it, so drop such a leftover first.
DROP INDEX CONCURRENTLY IF EXISTS idx_document_chunks_text_fts;
CREATE INDEX CONCURRENTLY idx_document_chunks_text_fts
    ON document_chunks USING gin (to_tsvector('simple', text));

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_document_chunks_text_fts;

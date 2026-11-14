-- +goose Up
-- Two HNSW indexes for the raw chunks, one for Cyrillic text and one for the
-- rest, instead of one over both.
--
-- In one graph the passages of the English books formed an island that a
-- Bulgarian question did not reach: the index's nearest 400 held none of the
-- English book's chunks even when they were the nearest of the whole library
-- (an exact scan put them first). A denser graph (m=32, ef_construction=200)
-- did not help; one graph per script did. SearchHybridChunks takes its
-- candidates from each index separately and repeats the predicates below, which
-- is what lets the planner use them. "Cyrillic" is three Cyrillic letters in a
-- row, as in the query.
--
-- In a transaction, not CONCURRENTLY: pgvector 0.8 builds in parallel and puts
-- the whole maintenance_work_mem in one shared-memory segment; the server's
-- 1 GB takes ~1 022 MiB of the container's 1 GB /dev/shm, and anything else in
-- it at the time fails the build ("could not resize shared memory segment",
-- seen on a copy with 1.5 GB). SET LOCAL only holds inside a transaction; 512 MB keeps the graph
-- in memory (the two indexes are ~150 MB each). The build blocks writes to
-- document_chunks, not reads, for ~10 s on 155 000 chunks (measured on a copy,
-- 2026-11-14); at the backend's start, when this runs, nothing is writing yet.
SET LOCAL maintenance_work_mem = '512MB';
CREATE INDEX idx_document_chunks_raw_cyrillic_embedding
    ON document_chunks USING hnsw (embedding vector_cosine_ops)
    WHERE chunk_type = 'raw_text' AND text ~ '[А-Яа-я]{3}';
CREATE INDEX idx_document_chunks_raw_other_embedding
    ON document_chunks USING hnsw (embedding vector_cosine_ops)
    WHERE chunk_type = 'raw_text' AND NOT (text ~ '[А-Яа-я]{3}');
DROP INDEX IF EXISTS idx_document_chunks_raw_embedding;

-- +goose Down
SET LOCAL maintenance_work_mem = '512MB';
CREATE INDEX idx_document_chunks_raw_embedding
    ON document_chunks USING hnsw (embedding vector_cosine_ops) WHERE chunk_type = 'raw_text';
DROP INDEX IF EXISTS idx_document_chunks_raw_cyrillic_embedding;
DROP INDEX IF EXISTS idx_document_chunks_raw_other_embedding;

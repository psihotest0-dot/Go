-- name: CreateIngestedFile :one
INSERT INTO ingested_files (user_id, file_name, file_hash, mime_type, file_size, status, local_path)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetIngestedFileByHash :one
SELECT * FROM ingested_files WHERE user_id = $1 AND file_hash = $2 AND deleted_at IS NULL;

-- GetIngestedFileByID is deliberately unfiltered: the ingest worker and the
-- restore path must be able to see a soft deleted row. Callers that serve
-- clients filter on deleted_at.
-- name: GetIngestedFileByID :one
SELECT * FROM ingested_files WHERE id = $1;

-- name: GetIngestedFiles :many
SELECT * FROM ingested_files WHERE user_id = $1 AND deleted_at IS NULL ORDER BY created_at DESC;

-- name: GetUserChunkCountsByFile :many
SELECT file_id, CAST(COUNT(*) AS BIGINT) AS chunk_count
FROM document_chunks
WHERE user_id = $1
GROUP BY file_id;

-- name: GetIngestedFileChunkCount :one
SELECT CAST(COUNT(*) AS BIGINT) AS chunk_count
FROM document_chunks
WHERE file_id = $1 AND user_id = $2;

-- A deleted file keeps its "deleted" status: an ingest job that was running when
-- the file was deleted must not turn it into "completed" or "failed".
-- name: UpdateIngestedFileStatus :exec
UPDATE ingested_files SET status = $2, updated_at = NOW() WHERE id = $1 AND user_id = $3 AND deleted_at IS NULL;

-- DeleteIngestedFile is a hard delete. Only used to roll back an upload that
-- never produced chunks; anything a client asks to remove goes through
-- SoftDeleteIngestedFile.
-- name: DeleteIngestedFile :exec
DELETE FROM ingested_files WHERE id = $1 AND user_id = $2;

-- SoftDeleteIngestedFile marks the file as gone and keeps the row as a record.
-- A delete asked for by a client goes through Database.DeleteFile, which also
-- deletes the chunks in the same transaction. The partial unique index on
-- (user_id, file_hash) only covers live rows, which keeps re-uploads of the same
-- document working.
-- name: SoftDeleteIngestedFile :exec
UPDATE ingested_files
SET deleted_at = NOW(), status = 'deleted', updated_at = NOW()
WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- name: DeleteFileChunks :execrows
DELETE FROM document_chunks WHERE file_id = $1 AND user_id = $2;

-- name: UpdateIngestedFileCategory :exec
UPDATE ingested_files SET category = $2, manual_override = true, updated_at = NOW() WHERE id = $1 AND user_id = $3;

-- name: UpdateIngestedFileCategoryAuto :exec
UPDATE ingested_files SET category = $2, updated_at = NOW() WHERE id = $1 AND manual_override = false;

-- name: GetDocumentChunksByFile :many
SELECT c.* FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
WHERE c.file_id = $1 AND c.user_id = $2 AND f.deleted_at IS NULL
ORDER BY c.chunk_index ASC;

-- Chunks of a file that is deleted are not written: an ingest still running when
-- the file was deleted would otherwise leave them behind. FOR SHARE waits for a
-- delete in progress (Database.DeleteFile) and then sees the file as deleted, so
-- no chunk can slip in between that delete's two statements.
-- name: CreateDocumentChunksBatch :exec
INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata, embedding_model)
SELECT u.file_id, u.user_id, u.chunk_index, u.text, u.embedding, u.chunk_type,
       CAST(u.metadata AS jsonb), sqlc.arg('embedding_model')::text
FROM (
    SELECT
        unnest(@file_ids::uuid[]) AS file_id,
        unnest(@user_ids::uuid[]) AS user_id,
        unnest(@chunk_indices::int[]) AS chunk_index,
        unnest(@texts::text[]) AS text,
        unnest(@embeddings::vector[]) AS embedding,
        unnest(@chunk_types::text[]) AS chunk_type,
        unnest(@metadatas::text[]) AS metadata
) u
JOIN ingested_files f ON f.id = u.file_id AND f.deleted_at IS NULL
FOR SHARE OF f;

-- The chunks whose vector was not made by the given model (or not known to be),
-- after last_id, with what is needed to rebuild the text that was embedded.
-- Soft deleted files are included: they can be restored. force = true returns
-- every chunk (for vectors whose recorded model cannot be trusted).
-- name: GetChunksForReembedding :many
SELECT c.id, c.chunk_type, c.chunk_index, c.text, f.file_name
FROM document_chunks c
JOIN ingested_files f ON f.id = c.file_id
WHERE c.id > sqlc.arg('last_id')::uuid
  AND (sqlc.arg('force')::boolean OR c.embedding_model IS DISTINCT FROM sqlc.arg('embedding_model')::text)
ORDER BY c.id ASC
LIMIT sqlc.arg('limit');

-- name: UpdateChunkEmbeddingsBatch :exec
UPDATE document_chunks
SET embedding = v.embedding,
    embedding_model = sqlc.arg('embedding_model')::text
FROM (
    SELECT unnest(sqlc.arg('ids')::uuid[]) AS id,
           unnest(sqlc.arg('embeddings')::vector[]) AS embedding
) v
WHERE document_chunks.id = v.id;

-- name: GetDocumentChunksWithFile :many
SELECT c.*, f.file_name
FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
WHERE c.id = ANY(sqlc.arg('chunk_ids')::uuid[]) AND c.user_id = sqlc.arg('user_id')
  AND f.deleted_at IS NULL;

-- The search queries below are shaped for the indexes. They are meant to run in
-- a transaction that has set hnsw.ef_search (db.Database.WithSearchSettings);
-- without it the HNSW scan returns 40 rows, not the candidates asked for.
--
-- Vector side: "WHERE chunk_type = '...' ORDER BY embedding <=> $v LIMIT n" is the
-- form the HNSW indexes serve (5 ms on 250 000 chunks, against 20 s for a scan
-- that filtered and ranked every row). There is one partial index per chunk type
-- (migration 00022; the raw chunks have two, one per script, since migration
-- 00028), and the WHERE clause must repeat its predicate for the
-- planner to use it. pgvector 0.5 applies every other filter (user, file status)
-- after the index scan, to the n nearest rows of that type; n is large enough
-- that this does not matter for one user's library.
--
-- Text side: PostgreSQL full-text search over the raw chunks, to_tsvector('simple',
-- text), served by a GIN index (migration 00020). Concept summaries are left out,
-- as on the vector side: they have their own search, and their text (English, and
-- matching the English themes that widen the query) is not the book's. ts_query is built by the caller
-- (retrieval.lexicalQuery): the significant words of the question as prefixes
-- ("столиц:* & българ:*"), which stands in for Bulgarian stemming. An empty
-- ts_query matches nothing. Trigram matching (word_similarity with "<%") was
-- tried first and measured: on 250 000 chunks the GIN index hands back a
-- hundred thousand candidates that each need a recheck, 0.5-17 s a query.

-- name: SearchConceptChunks :many
WITH nearest AS (
    SELECT c.id, c.file_id, c.user_id, c.chunk_type, c.text, c.metadata,
           (c.embedding <=> sqlc.arg('embedding')) AS distance
    FROM document_chunks c
    WHERE c.chunk_type = 'concept_summary'
    ORDER BY c.embedding <=> sqlc.arg('embedding')
    LIMIT sqlc.arg('vector_limit')
)
SELECT v.id, v.text AS content, f.file_name, v.metadata,
       CAST(v.distance AS FLOAT8) AS distance
FROM nearest v
JOIN ingested_files f ON v.file_id = f.id
WHERE f.user_id = sqlc.arg('user_id')
  AND f.status = 'completed'
  AND f.deleted_at IS NULL
  AND v.chunk_type = 'concept_summary'
  AND v.distance < sqlc.arg('max_distance')::float8
ORDER BY v.distance ASC
LIMIT sqlc.arg('match_count');

-- A question in one language is farther from the passages of a book in another
-- one than from passages in its own (Bulgarian question, English book: 0.17-0.21,
-- against 0.10-0.15 within Bulgarian, for the same quality of match), so a chunk
-- whose language differs from the question's gets cross_language_margin more
-- distance before it is dropped. "Cyrillic" is decided per chunk by three letters
-- in a row.
--
-- The vector candidates are taken separately from the Cyrillic chunks and from
-- the others, vector_limit from each, through one partial HNSW index per script
-- (migration 00028). With one index over both, the passages of an English book
-- formed an island in the graph that a Bulgarian question did not reach: for
-- "Питър Пан къде живее" none of the 32 nearest chunks of the whole library (all
-- from the English book, distance 0.18) were among the index's 400 candidates,
-- and m=32/ef_construction=200 did not help. The split also keeps a book in the
-- other script from being crowded out of the candidates by the nearer chunks of
-- the question's own. Measured on 98 questions (2026-11-14): the right book in
-- the first 8 results 77 -> 84, 16 -> 22 ms.
-- name: SearchHybridChunks :many
WITH nearest AS (
    (SELECT c.id, c.file_id, c.user_id, c.chunk_type,
           (c.embedding <=> sqlc.arg('embedding')) AS distance,
           true AS cyrillic
    FROM document_chunks c
    WHERE c.chunk_type = 'raw_text' AND c.text ~ '[А-Яа-я]{3}'
    ORDER BY c.embedding <=> sqlc.arg('embedding')
    LIMIT sqlc.arg('vector_limit'))
    UNION ALL
    (SELECT c.id, c.file_id, c.user_id, c.chunk_type,
           (c.embedding <=> sqlc.arg('embedding')) AS distance,
           false AS cyrillic
    FROM document_chunks c
    WHERE c.chunk_type = 'raw_text' AND NOT (c.text ~ '[А-Яа-я]{3}')
    ORDER BY c.embedding <=> sqlc.arg('embedding')
    LIMIT sqlc.arg('vector_limit'))
),
-- At most this many chunks per file, so one long book cannot fill the whole
-- result: 2 when the library has many files, more when it has few (a library of
-- one book would otherwise return two chunks in total).
per_file AS (
    SELECT GREATEST(2, CEIL(sqlc.arg('match_count')::float8 / GREATEST(COUNT(*), 1)))::int AS max_per_file
    FROM ingested_files
    WHERE user_id = sqlc.arg('user_id') AND status = 'completed' AND deleted_at IS NULL
),
chunk_vector_search AS (
    SELECT id, distance, rank FROM (
        SELECT v.id, v.distance,
               RANK() OVER (ORDER BY v.distance) AS rank,
               ROW_NUMBER() OVER (PARTITION BY v.file_id ORDER BY v.distance) AS file_rank
        FROM nearest v
        JOIN ingested_files f ON v.file_id = f.id
        WHERE v.user_id = sqlc.arg('user_id')
          AND f.status = 'completed'
          AND f.deleted_at IS NULL
          AND v.chunk_type = 'raw_text'
          AND v.distance < sqlc.arg('max_distance')::float8
              + (CASE WHEN v.cyrillic <> sqlc.arg('query_cyrillic')::boolean
                      THEN sqlc.arg('cross_language_margin')::float8 ELSE 0.0 END)
    ) sub
    WHERE file_rank <= (SELECT max_per_file FROM per_file)
    ORDER BY rank
    LIMIT 100
),
text_matches AS (
    SELECT c.id, c.file_id,
           ts_rank_cd(to_tsvector('simple', c.text), to_tsquery('simple', NULLIF(sqlc.arg('ts_query')::text, '')), 32) AS ts_score
    FROM document_chunks c
    JOIN ingested_files f ON c.file_id = f.id
    WHERE c.user_id = sqlc.arg('user_id')
      AND f.status = 'completed'
      AND f.deleted_at IS NULL
      AND c.chunk_type = 'raw_text'
      AND to_tsvector('simple', c.text) @@ to_tsquery('simple', NULLIF(sqlc.arg('ts_query')::text, ''))
),
-- A chunk that holds all the significant words of the question is a strong text
-- match whatever its density, so the text score starts at 0.5 and the proximity
-- of the words (ts_rank_cd, normalised to 0..1) adds up to 0.5.
chunk_text_search AS (
    SELECT id, sim_score, rank FROM (
        SELECT t.id, 0.5 + 0.5 * t.ts_score AS sim_score,
               RANK() OVER (ORDER BY t.ts_score DESC) AS rank,
               ROW_NUMBER() OVER (PARTITION BY t.file_id ORDER BY t.ts_score DESC) AS file_rank
        FROM text_matches t
    ) sub
    WHERE file_rank <= (SELECT max_per_file FROM per_file)
    ORDER BY rank
    LIMIT 100
)
SELECT
    c.id,
    c.text as content,
    f.file_name,
    CAST(
        GREATEST(
            COALESCE(1.0 - cvs.distance, 0.0),
            COALESCE(cts.sim_score, 0.0)
        )
    AS FLOAT8) as score,
    -- Whether the vector half found the chunk, and its cosine distance then (0 for
    -- one only the text half found); the nearest one is a measure of the search.
    CAST(cvs.id IS NOT NULL AS BOOLEAN) AS vector_match,
    CAST(COALESCE(cvs.distance, 0.0) AS FLOAT8) AS distance
FROM chunk_vector_search cvs
FULL OUTER JOIN chunk_text_search cts ON cvs.id = cts.id
JOIN document_chunks c ON c.id = COALESCE(cvs.id, cts.id)
JOIN ingested_files f ON c.file_id = f.id
WHERE c.user_id = sqlc.arg('user_id')
ORDER BY (COALESCE(1.0 / (60 + cvs.rank), 0.0) + COALESCE(1.0 / (60 + cts.rank), 0.0)) DESC
LIMIT sqlc.arg('match_count');

-- name: SearchTextChunks :many
SELECT c.id, c.text as content, f.file_name,
       CAST(0.5 + 0.5 * ts_rank_cd(to_tsvector('simple', c.text), to_tsquery('simple', NULLIF(sqlc.arg('ts_query')::text, '')), 32) AS FLOAT8) as score
FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
WHERE c.user_id = sqlc.arg('user_id')
  AND f.status = 'completed'
  AND f.deleted_at IS NULL
  AND c.chunk_type = 'raw_text'
  AND to_tsvector('simple', c.text) @@ to_tsquery('simple', NULLIF(sqlc.arg('ts_query')::text, ''))
ORDER BY score DESC
LIMIT sqlc.arg('match_count');

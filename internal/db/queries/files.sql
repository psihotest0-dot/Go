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

-- name: UpdateIngestedFileStatus :exec
UPDATE ingested_files SET status = $2, updated_at = NOW() WHERE id = $1 AND user_id = $3;

-- DeleteIngestedFile is a hard delete. Only used to roll back an upload that
-- never produced chunks; anything a client asks to remove goes through
-- SoftDeleteIngestedFile.
-- name: DeleteIngestedFile :exec
DELETE FROM ingested_files WHERE id = $1 AND user_id = $2;

-- SoftDeleteIngestedFile marks the file as gone without destroying its chunks,
-- so a mistaken delete can be undone. The partial unique index on
-- (user_id, file_hash) only covers live rows, which keeps re-uploads of the same
-- document working.
-- name: SoftDeleteIngestedFile :exec
UPDATE ingested_files
SET deleted_at = NOW(), status = 'deleted', updated_at = NOW()
WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- name: UpdateIngestedFileCategory :exec
UPDATE ingested_files SET category = $2, manual_override = true, updated_at = NOW() WHERE id = $1 AND user_id = $3;

-- name: UpdateIngestedFileCategoryAuto :exec
UPDATE ingested_files SET category = $2, updated_at = NOW() WHERE id = $1 AND manual_override = false;

-- name: CreateDocumentChunk :one
INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetDocumentChunksByFile :many
SELECT c.* FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
WHERE c.file_id = $1 AND c.user_id = $2 AND f.deleted_at IS NULL
ORDER BY c.chunk_index ASC;

-- name: CreateDocumentChunksBatch :exec
INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
SELECT
    unnest(@file_ids::uuid[]),
    unnest(@user_ids::uuid[]),
    unnest(@chunk_indices::int[]),
    unnest(@texts::text[]),
    unnest(@embeddings::vector[]),
    unnest(@chunk_types::text[]),
    CAST(unnest(@metadatas::text[]) AS jsonb);

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
-- Vector side: "ORDER BY embedding <=> $v LIMIT n" on the bare table is the one
-- form the HNSW index can serve (5 ms on 250 000 chunks, against 20 s for a scan
-- that filtered and ranked every row). pgvector 0.5 applies WHERE clauses after
-- the index scan, so the user, status and type filters are applied to the n
-- nearest rows, not before them; n is large enough that this does not matter for
-- one user's library.
--
-- Text side: PostgreSQL full-text search over to_tsvector('simple', text), served
-- by a GIN index (migration 00020). ts_query is built by the caller
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

-- name: SearchHybridChunks :many
WITH nearest AS (
    SELECT c.id, c.file_id, c.user_id, c.chunk_type,
           (c.embedding <=> sqlc.arg('embedding')) AS distance
    FROM document_chunks c
    ORDER BY c.embedding <=> sqlc.arg('embedding')
    LIMIT sqlc.arg('vector_limit')
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
    SELECT id, category, distance, rank FROM (
        SELECT v.id, f.category, v.distance,
               RANK() OVER (ORDER BY v.distance) AS rank,
               ROW_NUMBER() OVER (PARTITION BY v.file_id ORDER BY v.distance) AS file_rank
        FROM nearest v
        JOIN ingested_files f ON v.file_id = f.id
        WHERE v.user_id = sqlc.arg('user_id')
          AND f.status = 'completed'
          AND f.deleted_at IS NULL
          AND v.chunk_type = 'raw_text'
          AND v.distance < sqlc.arg('max_distance')::float8
    ) sub
    WHERE file_rank <= (SELECT max_per_file FROM per_file)
    ORDER BY rank
    LIMIT 100
),
text_matches AS (
    SELECT c.id, c.file_id, f.category,
           ts_rank_cd(to_tsvector('simple', c.text), to_tsquery('simple', NULLIF(sqlc.arg('ts_query')::text, '')), 32) AS ts_score
    FROM document_chunks c
    JOIN ingested_files f ON c.file_id = f.id
    WHERE c.user_id = sqlc.arg('user_id')
      AND f.status = 'completed'
      AND f.deleted_at IS NULL
      AND to_tsvector('simple', c.text) @@ to_tsquery('simple', NULLIF(sqlc.arg('ts_query')::text, ''))
),
-- A chunk that holds all the significant words of the question is a strong text
-- match whatever its density, so the text score starts at 0.5 and the proximity
-- of the words (ts_rank_cd, normalised to 0..1) adds up to 0.5.
chunk_text_search AS (
    SELECT id, category, sim_score, rank FROM (
        SELECT t.id, t.category, 0.5 + 0.5 * t.ts_score AS sim_score,
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
        ) +
        (CASE WHEN COALESCE(cvs.category, cts.category) = sqlc.arg('boost_category')::text THEN sqlc.arg('boost_amount')::FLOAT8 ELSE 0.0 END)
    AS FLOAT8) as score
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
  AND to_tsvector('simple', c.text) @@ to_tsquery('simple', NULLIF(sqlc.arg('ts_query')::text, ''))
ORDER BY score DESC
LIMIT sqlc.arg('match_count');

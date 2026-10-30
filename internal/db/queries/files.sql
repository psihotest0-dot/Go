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

-- name: SearchConceptChunks :many
SELECT c.id, c.text as content, f.file_name, c.metadata,
       CAST(c.embedding <=> sqlc.arg('embedding') AS FLOAT8) as distance
FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
WHERE c.user_id = sqlc.arg('user_id')
  AND f.status = 'completed'
  AND f.deleted_at IS NULL
  AND c.chunk_type = 'concept_summary'
  AND (c.embedding <=> sqlc.arg('embedding')) < 0.40
ORDER BY distance ASC
LIMIT 3;

-- name: SearchHybridChunks :many
WITH chunk_vector_search AS (
    SELECT source_type, id, content, rank, category, distance FROM (
        SELECT 'chunk' as source_type, c.id, c.text as content, f.category,
               (c.embedding <=> sqlc.arg('embedding')) as distance,
               RANK() OVER (ORDER BY c.embedding <=> sqlc.arg('embedding')) as rank,
               ROW_NUMBER() OVER (PARTITION BY c.file_id ORDER BY c.embedding <=> sqlc.arg('embedding')) as file_rank
        FROM document_chunks c
        JOIN ingested_files f ON c.file_id = f.id
        WHERE c.user_id = sqlc.arg('user_id')
          AND f.status = 'completed'
          AND f.deleted_at IS NULL
          AND c.chunk_type = 'raw_text'
          AND (c.embedding <=> sqlc.arg('embedding')) < 0.45
    ) sub
    WHERE file_rank <= 2
    ORDER BY rank
    LIMIT 100
),
chunk_text_search AS (
    SELECT source_type, id, content, rank, category, sim_score FROM (
        SELECT 'chunk' as source_type, c.id, c.text as content, f.category,
               similarity(c.text, sqlc.arg('query_text')) as sim_score,
               RANK() OVER (ORDER BY similarity(c.text, sqlc.arg('query_text')) DESC) as rank,
               ROW_NUMBER() OVER (PARTITION BY c.file_id ORDER BY similarity(c.text, sqlc.arg('query_text')) DESC) as file_rank
        FROM document_chunks c
        JOIN ingested_files f ON c.file_id = f.id
        WHERE c.user_id = sqlc.arg('user_id')
          AND f.status = 'completed'
          AND f.deleted_at IS NULL
          AND similarity(c.text, sqlc.arg('query_text')) > 0.15
    ) sub
    WHERE file_rank <= 2
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
FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
LEFT JOIN chunk_vector_search cvs ON c.id = cvs.id
LEFT JOIN chunk_text_search cts ON c.id = cts.id
WHERE (cvs.id IS NOT NULL OR cts.id IS NOT NULL)
  AND c.user_id = sqlc.arg('user_id')
ORDER BY (COALESCE(1.0 / (60 + cvs.rank), 0.0) + COALESCE(1.0 / (60 + cts.rank), 0.0)) DESC
LIMIT sqlc.arg('match_count');

-- name: SearchTextChunks :many
SELECT c.id, c.text as content, f.file_name, CAST(similarity(c.text, sqlc.arg('query_text')) AS FLOAT8) as score
FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
WHERE c.user_id = sqlc.arg('user_id')
  AND f.status = 'completed'
  AND f.deleted_at IS NULL
ORDER BY score DESC
LIMIT sqlc.arg('match_count');

-- name: CreateIngestedFile :one
INSERT INTO ingested_files (user_id, file_name, file_hash, mime_type, file_size, status, local_path)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetIngestedFileByHash :one
SELECT * FROM ingested_files WHERE user_id = $1 AND file_hash = $2;

-- name: GetIngestedFileByID :one
SELECT * FROM ingested_files WHERE id = $1;

-- name: GetIngestedFiles :many
SELECT * FROM ingested_files WHERE user_id = $1 ORDER BY created_at DESC;

-- name: UpdateIngestedFileStatus :exec
UPDATE ingested_files SET status = $2, updated_at = NOW() WHERE id = $1 AND user_id = $3;

-- name: DeleteIngestedFile :exec
DELETE FROM ingested_files WHERE id = $1 AND user_id = $2;

-- name: UpdateIngestedFileCategory :exec
UPDATE ingested_files SET category = $2, manual_override = true, updated_at = NOW() WHERE id = $1 AND user_id = $3;

-- name: UpdateIngestedFileCategoryAuto :exec
UPDATE ingested_files SET category = $2, updated_at = NOW() WHERE id = $1 AND manual_override = false;

-- name: CreateDocumentChunk :one
INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetDocumentChunksByFile :many
SELECT * FROM document_chunks WHERE file_id = $1 AND user_id = $2 ORDER BY chunk_index ASC;

-- name: CreateDocumentChunksBatch :exec
INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding)
SELECT
    unnest(@file_ids::uuid[]),
    unnest(@user_ids::uuid[]),
    unnest(@chunk_indices::int[]),
    unnest(@texts::text[]),
    unnest(@embeddings::vector[]);

-- name: GetDocumentChunksWithFile :many
SELECT c.*, f.file_name
FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
WHERE c.id = ANY(sqlc.arg('chunk_ids')::uuid[]) AND c.user_id = sqlc.arg('user_id');

-- name: SearchHybridChunks :many
WITH chunk_vector_search AS (
    SELECT source_type, id, content, rank, category FROM (
        SELECT 'chunk' as source_type, c.id, c.text as content, f.category,
               RANK() OVER (ORDER BY c.embedding <=> sqlc.arg('embedding')) as rank,
               ROW_NUMBER() OVER (PARTITION BY c.file_id ORDER BY c.embedding <=> sqlc.arg('embedding')) as file_rank
        FROM document_chunks c
        JOIN ingested_files f ON c.file_id = f.id
        WHERE c.user_id = sqlc.arg('user_id')
          AND f.status = 'completed'
          AND (c.embedding <=> sqlc.arg('embedding')) < 0.45
    ) sub
    WHERE file_rank <= 2
    ORDER BY rank
    LIMIT 100
),
chunk_text_search AS (
    SELECT source_type, id, content, rank, category FROM (
        SELECT 'chunk' as source_type, c.id, c.text as content, f.category,
               RANK() OVER (ORDER BY similarity(c.text, sqlc.arg('query_text')) DESC) as rank,
               ROW_NUMBER() OVER (PARTITION BY c.file_id ORDER BY similarity(c.text, sqlc.arg('query_text')) DESC) as file_rank
        FROM document_chunks c
        JOIN ingested_files f ON c.file_id = f.id
        WHERE c.user_id = sqlc.arg('user_id')
          AND f.status = 'completed'
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
        COALESCE(1.0 / (60 + cvs.rank), 0.0) +
        COALESCE(1.0 / (60 + cts.rank), 0.0) +
        (CASE WHEN COALESCE(cvs.category, cts.category) = sqlc.arg('boost_category')::text THEN sqlc.arg('boost_amount')::FLOAT8 ELSE 0.0 END)
    AS FLOAT8) as score
FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
LEFT JOIN chunk_vector_search cvs ON c.id = cvs.id
LEFT JOIN chunk_text_search cts ON c.id = cts.id
WHERE (cvs.id IS NOT NULL OR cts.id IS NOT NULL)
  AND c.user_id = sqlc.arg('user_id')
ORDER BY score DESC
LIMIT sqlc.arg('match_count');

-- name: SearchTextChunks :many
SELECT c.id, c.text as content, f.file_name, CAST(similarity(c.text, sqlc.arg('query_text')) AS FLOAT8) as score
FROM document_chunks c
JOIN ingested_files f ON c.file_id = f.id
WHERE c.user_id = sqlc.arg('user_id')
  AND f.status = 'completed'
ORDER BY score DESC
LIMIT sqlc.arg('match_count');

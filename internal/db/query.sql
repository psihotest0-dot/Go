-- name: GetTopMemories :many
SELECT * FROM memories
WHERE user_id = sqlc.arg('user_id') AND status IN ('active', 'candidate')
ORDER BY importance DESC, last_used_at DESC
LIMIT sqlc.arg('limit');
-- name: CreateUser :one
INSERT INTO users DEFAULT VALUES RETURNING *;

-- name: CreateMessage :one
INSERT INTO messages (user_id, session_id, role, content, source_chunk_ids) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: GetMessagesBySession :many
SELECT * FROM messages WHERE user_id = $1 AND session_id = $2 ORDER BY created_at ASC;

-- name: CreateMemory :one
INSERT INTO memories (user_id, content, status, embedding, embedding_model, source_message_id, importance, metadata, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetMemories :many
SELECT * FROM memories
WHERE user_id = sqlc.arg('user_id')
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
ORDER BY created_at DESC;

-- name: GetMemoryByHash :one
SELECT * FROM memories WHERE user_id = $1 AND content_hash = $2;

-- name: GetMemoryByID :one
SELECT * FROM memories WHERE id = $1 AND user_id = $2;

-- name: UpdateMemory :one
UPDATE memories
SET content = COALESCE(sqlc.narg('content'), content),
    status = COALESCE(sqlc.narg('status'), status),
    embedding = COALESCE(sqlc.narg('embedding')::vector, embedding),
    embedding_model = COALESCE(sqlc.narg('embedding_model'), embedding_model),
    importance = COALESCE(sqlc.narg('importance'), importance),
    metadata = COALESCE(sqlc.narg('metadata'), metadata),
    content_hash = COALESCE(sqlc.narg('content_hash'), content_hash),
    updated_at = NOW()
WHERE id = $1 AND user_id = $2
RETURNING *;


-- name: SearchHybridMemories :many
WITH vector_search AS (
    SELECT id, RANK() OVER (ORDER BY memories.embedding <=> sqlc.arg('embedding')) as rank
    FROM memories
    WHERE memories.user_id = sqlc.arg('user_id')
      AND memories.status IN ('active', 'candidate')
      AND (memories.embedding <=> sqlc.arg('embedding')) < 0.45
    ORDER BY rank
    LIMIT 100
),
text_search AS (
    SELECT id, RANK() OVER (ORDER BY similarity(content, sqlc.arg('query_text')) DESC) as rank
    FROM memories
    WHERE memories.user_id = sqlc.arg('user_id')
      AND memories.status IN ('active', 'candidate')
      AND similarity(content, sqlc.arg('query_text')) > 0.15
    ORDER BY rank
    LIMIT 100
)
SELECT m.id, m.content, CAST(COALESCE(1.0 / (60 + vs.rank), 0.0) + COALESCE(1.0 / (60 + ts.rank), 0.0) + 0.01 * (COALESCE(m.importance, 5)::FLOAT8 / 10.0) AS FLOAT8) as score
FROM memories m
LEFT JOIN vector_search vs ON m.id = vs.id
LEFT JOIN text_search ts ON m.id = ts.id
WHERE (vs.id IS NOT NULL OR ts.id IS NOT NULL)
  AND m.user_id = sqlc.arg('user_id') AND m.status != 'superseded'
ORDER BY score DESC
LIMIT sqlc.arg('match_count');

-- name: SearchTextMemories :many
SELECT m.id, m.content, CAST(similarity(m.content, sqlc.arg('query_text')) AS FLOAT8) as score
FROM memories m
WHERE m.user_id = sqlc.arg('user_id') AND m.status IN ('active', 'candidate')
ORDER BY score DESC
LIMIT sqlc.arg('match_count');

-- name: DeleteMemory :exec
DELETE FROM memories WHERE id = $1 AND user_id = $2;

-- name: GetEmbeddingCacheByHash :one
SELECT * FROM embedding_cache WHERE content_hash = $1;

-- name: CreateEmbeddingCache :one
INSERT INTO embedding_cache (content_hash, embedding)
VALUES ($1, $2)
RETURNING *;

-- name: CreateAgentTrace :one
INSERT INTO agent_traces (user_id, session_id, step, details)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUnanalyzedMessages :many
SELECT *
FROM messages
WHERE is_analyzed = FALSE AND user_id = $1
ORDER BY created_at ASC;

-- name: GetUsersWithUnanalyzedMessages :many
SELECT DISTINCT user_id
FROM messages
WHERE is_analyzed = FALSE;

-- name: MarkMessageAnalyzed :exec
UPDATE messages SET is_analyzed = TRUE WHERE id = $1;

-- name: DeleteOldEmbeddingCache :execrows
WITH deleted AS (
    SELECT id FROM embedding_cache
    WHERE created_at < NOW() - INTERVAL '30 days'
    LIMIT 1000
    FOR UPDATE SKIP LOCKED
)
DELETE FROM embedding_cache
WHERE id IN (SELECT id FROM deleted);

-- name: SupercedeMemory :exec
UPDATE memories
SET status = 'superseded', updated_at = NOW()
WHERE id = $1 AND user_id = $2;
-- name: GetAllMemoriesForReembedding :many
SELECT * FROM memories ORDER BY created_at ASC LIMIT $1 OFFSET $2;

-- name: UpdateMemoryEmbedding :exec
UPDATE memories SET embedding = $1, embedding_model = $2 WHERE id = $3;

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
UPDATE ingested_files SET status = $2, updated_at = NOW() WHERE id = $1;

-- name: CreateDocumentChunk :one
INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetDocumentChunksByFile :many
SELECT * FROM document_chunks WHERE file_id = $1 ORDER BY chunk_index ASC;


-- name: DeleteIngestedFile :exec
DELETE FROM ingested_files WHERE id = $1 AND user_id = $2;


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
WHERE c.id = ANY(sqlc.arg('chunk_ids')::uuid[]);

-- name: UpdateIngestedFileCategory :exec
UPDATE ingested_files SET category = $2, manual_override = true, updated_at = NOW() WHERE id = $1;

-- name: UpdateIngestedFileCategoryAuto :exec
UPDATE ingested_files SET category = $2, updated_at = NOW() WHERE id = $1 AND manual_override = false;

-- name: GetGlobalRules :many
SELECT value FROM global_rules WHERE user_id = $1 AND enabled = true;

-- name: SaveGlobalRule :exec
INSERT INTO global_rules (user_id, key, value, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (user_id, key) DO UPDATE
SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at, enabled = true;

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


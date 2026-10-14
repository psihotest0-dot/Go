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

-- name: UpdateMemory :one
UPDATE memories
SET content = $2, status = $3, embedding = $4, embedding_model = $5, importance = $6, metadata = $7, content_hash = $8, updated_at = NOW()
WHERE id = $1 AND user_id = $9
RETURNING *;


-- name: SearchHybridMemories :many
WITH vector_search AS (
    SELECT id, RANK() OVER (ORDER BY memories.embedding <=> sqlc.arg('embedding')) as rank
    FROM memories
    WHERE memories.user_id = sqlc.arg('user_id') AND memories.status = 'active'
    ORDER BY rank
    LIMIT 100
),
text_search AS (
    SELECT id, RANK() OVER (ORDER BY similarity(content, sqlc.arg('query_text')) DESC) as rank
    FROM memories
    WHERE memories.user_id = sqlc.arg('user_id') AND memories.status = 'active'
    ORDER BY rank
    LIMIT 100
)
SELECT m.*
FROM memories m
LEFT JOIN vector_search vs ON m.id = vs.id
LEFT JOIN text_search ts ON m.id = ts.id
WHERE (vs.id IS NOT NULL OR ts.id IS NOT NULL)
  AND m.user_id = sqlc.arg('user_id')
ORDER BY
    COALESCE(1.0 / (60 + vs.rank), 0.0) +
    COALESCE(1.0 / (60 + ts.rank), 0.0) DESC
LIMIT sqlc.arg('match_count');

-- name: SearchTextMemories :many
SELECT m.*
FROM memories m
WHERE m.user_id = sqlc.arg('user_id') AND m.status = 'active'
ORDER BY similarity(m.content, sqlc.arg('query_text')) DESC
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
SELECT m.*
FROM messages m
LEFT JOIN memories mem ON m.id = mem.source_message_id
WHERE mem.id IS NULL AND m.user_id = $1
ORDER BY m.created_at ASC;

-- name: GetUsersWithUnanalyzedMessages :many
SELECT DISTINCT m.user_id
FROM messages m
LEFT JOIN memories mem ON m.id = mem.source_message_id
WHERE mem.id IS NULL;

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


-- name: SearchHybridAll :many
WITH
memory_vector_search AS (
    SELECT 'memory' as source_type, id, content, RANK() OVER (ORDER BY memories.embedding <=> sqlc.arg('embedding')) as rank
    FROM memories
    WHERE memories.user_id = sqlc.arg('user_id') AND memories.status = 'active'
    ORDER BY rank
    LIMIT 100
),
memory_text_search AS (
    SELECT 'memory' as source_type, id, content, RANK() OVER (ORDER BY similarity(content, sqlc.arg('query_text')) DESC) as rank
    FROM memories
    WHERE memories.user_id = sqlc.arg('user_id') AND memories.status = 'active'
    ORDER BY rank
    LIMIT 100
),
chunk_vector_search AS (
    SELECT 'chunk' as source_type, id, text as content, RANK() OVER (ORDER BY document_chunks.embedding <=> sqlc.arg('embedding')) as rank
    FROM document_chunks
    WHERE document_chunks.user_id = sqlc.arg('user_id')
    ORDER BY rank
    LIMIT 100
),
chunk_text_search AS (
    SELECT 'chunk' as source_type, id, text as content, RANK() OVER (ORDER BY similarity(text, sqlc.arg('query_text')) DESC) as rank
    FROM document_chunks
    WHERE document_chunks.user_id = sqlc.arg('user_id')
    ORDER BY rank
    LIMIT 100
),
combined_results AS (
    SELECT
        m.id,
        m.content,
        'memory' as source_type,
        CAST(COALESCE(1.0 / (60 + mvs.rank), 0.0) + COALESCE(1.0 / (60 + mts.rank), 0.0) AS FLOAT8) as score
    FROM memories m
    LEFT JOIN memory_vector_search mvs ON m.id = mvs.id
    LEFT JOIN memory_text_search mts ON m.id = mts.id
    WHERE (mvs.id IS NOT NULL OR mts.id IS NOT NULL)
      AND m.user_id = sqlc.arg('user_id')
      AND m.status = 'active'

    UNION ALL

    SELECT
        c.id,
        c.text as content,
        'chunk' as source_type,
        CAST(COALESCE(1.0 / (60 + cvs.rank), 0.0) + COALESCE(1.0 / (60 + cts.rank), 0.0) AS FLOAT8) as score
    FROM document_chunks c
    LEFT JOIN chunk_vector_search cvs ON c.id = cvs.id
    LEFT JOIN chunk_text_search cts ON c.id = cts.id
    WHERE (cvs.id IS NOT NULL OR cts.id IS NOT NULL)
      AND c.user_id = sqlc.arg('user_id')
)
SELECT id, content, source_type, score
FROM combined_results
ORDER BY score DESC
LIMIT sqlc.arg('match_count');


-- name: DeleteIngestedFile :exec
DELETE FROM ingested_files WHERE id = $1 AND user_id = $2;


-- name: SearchTextAll :many
WITH combined AS (
    SELECT m.id, m.content, 'memory' as source_type, similarity(m.content, sqlc.arg('query_text')) as sim
    FROM memories m
    WHERE m.user_id = sqlc.arg('user_id') AND m.status = 'active'

    UNION ALL

    SELECT c.id, c.text as content, 'chunk' as source_type, similarity(c.text, sqlc.arg('query_text')) as sim
    FROM document_chunks c
    WHERE c.user_id = sqlc.arg('user_id')
)
SELECT id, content, source_type
FROM combined
ORDER BY sim DESC
LIMIT sqlc.arg('match_count');

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

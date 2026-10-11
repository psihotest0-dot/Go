-- name: CreateUser :one
INSERT INTO users DEFAULT VALUES RETURNING *;

-- name: CreateMessage :one
INSERT INTO messages (user_id, session_id, role, content) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetMessagesBySession :many
SELECT * FROM messages WHERE user_id = $1 AND session_id = $2 ORDER BY created_at ASC;

-- name: CreateMemory :one
INSERT INTO memories (user_id, content, status, embedding, embedding_model, source_message_id, importance, metadata, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetMemories :many
SELECT * FROM memories WHERE user_id = $1 ORDER BY created_at DESC;

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
    WHERE user_id = sqlc.arg('user_id') AND status = 'active'
    ORDER BY rank
    LIMIT 100
),
text_search AS (
    SELECT id, RANK() OVER (ORDER BY similarity(content, sqlc.arg('query_text')) DESC) as rank
    FROM memories
    WHERE user_id = sqlc.arg('user_id') AND status = 'active'
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

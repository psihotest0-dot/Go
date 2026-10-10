-- name: CreateUser :one
INSERT INTO users DEFAULT VALUES RETURNING *;

-- name: CreateMessage :one
INSERT INTO messages (user_id, session_id, role, content) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetMessagesBySession :many
SELECT * FROM messages WHERE user_id = $1 AND session_id = $2 ORDER BY created_at ASC;

-- name: CreateMemory :one
INSERT INTO memories (user_id, content, status, source_message_id, importance, metadata, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetMemories :many
SELECT * FROM memories WHERE user_id = $1 ORDER BY created_at DESC;

-- name: GetMemoryByHash :one
SELECT * FROM memories WHERE user_id = $1 AND content_hash = $2;

-- name: UpdateMemory :one
UPDATE memories
SET content = $2, status = $3, importance = $4, metadata = $5, content_hash = $6, updated_at = NOW()
WHERE id = $1 AND user_id = $7
RETURNING *;

-- name: DeleteMemory :exec
DELETE FROM memories WHERE id = $1 AND user_id = $2;

-- name: GetEmbeddingCacheByHash :one
SELECT * FROM embedding_cache WHERE content_hash = $1;

-- name: CreateEmbeddingCache :one
INSERT INTO embedding_cache (content_hash, embedding)
VALUES ($1, $2)
RETURNING *;

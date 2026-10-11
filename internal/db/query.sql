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

-- name: SearchSimilarMemories :many
SELECT * FROM memories
WHERE user_id = $1
ORDER BY embedding <=> $2
LIMIT $3;

-- name: DeleteMemory :exec
DELETE FROM memories WHERE id = $1 AND user_id = $2;

-- name: GetEmbeddingCacheByHash :one
SELECT * FROM embedding_cache WHERE content_hash = $1;

-- name: CreateEmbeddingCache :one
INSERT INTO embedding_cache (content_hash, embedding)
VALUES ($1, $2)
RETURNING *;

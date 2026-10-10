-- name: CreateUser :one
INSERT INTO users DEFAULT VALUES RETURNING *;

-- name: CreateConversation :one
INSERT INTO conversations (user_id) VALUES ($1) RETURNING *;

-- name: CreateMessage :one
INSERT INTO messages (user_id, conversation_id, content) VALUES ($1, $2, $3) RETURNING *;

-- name: GetMessages :many
SELECT * FROM messages WHERE user_id = $1 AND conversation_id = $2 ORDER BY created_at ASC;

-- name: CreateMemory :one
INSERT INTO memories (user_id, content, embedding, embedding_model, content_hash, importance)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetMemories :many
SELECT * FROM memories WHERE user_id = $1 ORDER BY created_at DESC;

-- name: CacheEmbedding :one
INSERT INTO embedding_cache (content_hash, embedding) VALUES ($1, $2) RETURNING *;

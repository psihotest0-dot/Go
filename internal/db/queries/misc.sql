-- name: GetEmbeddingCacheByHash :one
SELECT * FROM embedding_cache WHERE content_hash = $1;

-- name: CreateEmbeddingCache :one
INSERT INTO embedding_cache (content_hash, embedding)
VALUES ($1, $2)
RETURNING *;

-- name: DeleteOldEmbeddingCache :execrows
WITH deleted AS (
    SELECT id FROM embedding_cache
    WHERE created_at < NOW() - INTERVAL '30 days'
    LIMIT 1000
    FOR UPDATE SKIP LOCKED
)
DELETE FROM embedding_cache
WHERE id IN (SELECT id FROM deleted);

-- name: CreateAgentTrace :one
INSERT INTO agent_traces (user_id, session_id, step, details)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetGlobalRules :many
SELECT value FROM global_rules WHERE user_id = $1 AND enabled = true;

-- name: SaveGlobalRule :exec
INSERT INTO global_rules (user_id, key, value, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (user_id, key) DO UPDATE
SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at, enabled = true;

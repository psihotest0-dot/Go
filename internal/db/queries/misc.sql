-- name: GetEmbeddingCacheByHash :one
SELECT * FROM embedding_cache WHERE content_hash = $1;

-- name: CreateEmbeddingCache :one
INSERT INTO embedding_cache (content_hash, embedding)
VALUES ($1, $2)
RETURNING *;

-- name: GetEmbeddingCacheByHashes :many
-- One read for a whole batch of texts.
SELECT content_hash, embedding FROM embedding_cache WHERE content_hash = ANY(sqlc.arg('hashes')::text[]);

-- name: CreateEmbeddingCacheBatch :exec
-- One write for a whole batch; an entry that exists already (a concurrent
-- writer, or the same text twice) is left as it is. The vectors come as their
-- text form ("[0.1,0.2,...]").
INSERT INTO embedding_cache (content_hash, embedding)
SELECT h.content_hash, e.embedding::vector(384)
FROM unnest(sqlc.arg('hashes')::text[]) WITH ORDINALITY AS h(content_hash, n)
JOIN unnest(sqlc.arg('embeddings')::text[]) WITH ORDINALITY AS e(embedding, n) ON e.n = h.n
ON CONFLICT (content_hash) DO NOTHING;

-- name: TouchEmbeddingCache :exec
-- Renews the last use of entries that were used again. Entries touched within the
-- last day are skipped, so a hot entry costs one write a day, not one per use.
UPDATE embedding_cache
SET last_accessed_at = now()
WHERE content_hash = ANY(sqlc.arg('hashes')::text[])
  AND last_accessed_at < now() - INTERVAL '1 day';

-- name: DeleteOldEmbeddingCache :execrows
WITH deleted AS (
    SELECT id FROM embedding_cache
    WHERE last_accessed_at < NOW() - INTERVAL '7 days'
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

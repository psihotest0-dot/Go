-- name: GetTopMemories :many
SELECT * FROM memories
WHERE user_id = sqlc.arg('user_id') AND status IN ('active', 'candidate')
ORDER BY importance DESC, last_used_at DESC NULLS LAST
LIMIT sqlc.arg('limit');

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

-- name: SupercedeMemory :exec
UPDATE memories
SET status = 'superseded', updated_at = NOW()
WHERE id = $1 AND user_id = $2;

-- name: GetAllMemoriesForReembedding :many
SELECT * FROM memories
WHERE (created_at, id) > (sqlc.arg('last_created_at')::TIMESTAMP WITH TIME ZONE, sqlc.arg('last_id')::UUID)
  AND content != ''
  AND (embedding_model != sqlc.arg('embedding_model') OR embedding_model IS NULL)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('limit');

-- name: UpdateMemoryEmbedding :exec
UPDATE memories SET embedding = $1, embedding_model = $2 WHERE id = $3 AND user_id = $4;

-- name: UpdateMemoryEmbeddingsBatch :exec
UPDATE memories
SET embedding = v.embedding,
    embedding_model = sqlc.arg('embedding_model'),
    updated_at = NOW()
FROM (
    SELECT unnest(sqlc.arg('ids')::uuid[]) AS id,
           unnest(sqlc.arg('embeddings')::vector[]) AS embedding
) v
WHERE memories.id = v.id;

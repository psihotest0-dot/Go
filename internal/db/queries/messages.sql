-- name: CreateMessage :one
INSERT INTO messages (user_id, session_id, role, content, source_chunk_ids) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: GetMessagesBySession :many
SELECT * FROM messages WHERE user_id = $1 AND session_id = $2 ORDER BY created_at DESC LIMIT $3;

-- name: GetUnanalyzedMessages :many
SELECT *
FROM messages
WHERE is_analyzed = FALSE AND user_id = $1
ORDER BY created_at ASC
LIMIT 100;

-- name: GetUsersWithUnanalyzedMessages :many
SELECT DISTINCT user_id
FROM messages
WHERE is_analyzed = FALSE;

-- name: MarkMessageAnalyzed :exec
UPDATE messages SET is_analyzed = TRUE WHERE id = $1 AND user_id = $2;

-- name: DeleteLastAssistantMessage :exec
DELETE FROM messages
WHERE id = (
    SELECT m.id FROM messages m
    WHERE m.session_id = $1 AND m.user_id = $2 AND m.role = 'assistant'
    ORDER BY m.created_at DESC
    LIMIT 1
);

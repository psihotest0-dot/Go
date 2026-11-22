-- name: CreateMessage :one
INSERT INTO messages (user_id, session_id, role, content, source_chunk_ids, tool_log, answer_plan) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- Whether each of the last answers of a conversation before a moment had a plan (П55), the newest
-- first: a plan comes at most once in answerplan.Every replies.
-- name: RecentAnswerPlanned :many
SELECT (answer_plan IS NOT NULL)::boolean AS planned FROM messages
WHERE user_id = $1 AND session_id = $2 AND role = 'assistant' AND created_at < $3
ORDER BY created_at DESC, id DESC LIMIT $4;

-- The plans of the person's last answers that had one, in any conversation, the newest first: a
-- memory they used is not used again while there are others.
-- name: RecentAnswerPlans :many
SELECT answer_plan FROM messages
WHERE user_id = $1 AND role = 'assistant' AND answer_plan IS NOT NULL
ORDER BY created_at DESC, id DESC LIMIT $2;

-- name: GetMessagesBySession :many
SELECT * FROM messages WHERE user_id = $1 AND session_id = $2 ORDER BY created_at DESC, id DESC LIMIT $3;

-- Where a message stands in its conversation: the time that, with its id, is the cursor of
-- GetMessagesBySessionBefore. A message of another person or session is "no rows".
-- name: GetMessageCursor :one
SELECT created_at::timestamptz AS created_at
FROM messages
WHERE id = $1 AND user_id = $2 AND session_id = $3 AND created_at IS NOT NULL;

-- The page of a conversation older than the message the cursor names (its time and id),
-- newest first like GetMessagesBySession. The id breaks the tie of messages stored in one
-- instant, so no page repeats or skips one; "created_at <=" is the same bound for the index.
-- name: GetMessagesBySessionBefore :many
SELECT * FROM messages
WHERE user_id = sqlc.arg('user_id') AND session_id = sqlc.arg('session_id')
  AND created_at <= sqlc.arg('before_at')::timestamptz
  AND (created_at, id) < (sqlc.arg('before_at')::timestamptz, sqlc.arg('before_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit');

-- name: GetRecentMessagesByUser :many
SELECT * FROM messages WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2;

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

-- The answers a regeneration replaces, by id: the ones read when it started, not
-- whatever is newest when it ends (a turn sent meanwhile would lose its answer).
-- name: DeleteAssistantMessages :execrows
DELETE FROM messages
WHERE user_id = sqlc.arg('user_id') AND session_id = sqlc.arg('session_id')
  AND role = 'assistant' AND id = ANY(sqlc.arg('ids')::uuid[]);

-- name: IncrementAnalysisAttempts :exec
UPDATE messages
SET analysis_attempts = analysis_attempts + 1
WHERE id = $1;

-- How many answers Lili had given in the conversation before the moment: the count that
-- decides which turns end with a question (plan item P49). Empty answers are not counted,
-- the same as in the history the prompt is built from.
-- name: CountAssistantRepliesBefore :one
SELECT count(*)::bigint
FROM messages
WHERE user_id = sqlc.arg('user_id') AND session_id = sqlc.arg('session_id')
  AND role = 'assistant' AND content <> ''
  AND created_at < sqlc.arg('before');

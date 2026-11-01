-- name: GetSessionSummary :one
SELECT * FROM session_summaries WHERE user_id = $1 AND session_id = $2;

-- Stores the summary of a session. The position only moves forward: a pass that
-- finishes late (an older one overtaken by a newer one) must not move it back
-- and make the same messages be summarised again.
-- name: UpsertSessionSummary :exec
INSERT INTO session_summaries (user_id, session_id, summary, covered_until_message_id, covered_until_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, session_id) DO UPDATE
SET summary = EXCLUDED.summary,
    covered_until_message_id = EXCLUDED.covered_until_message_id,
    covered_until_at = EXCLUDED.covered_until_at,
    updated_at = now()
WHERE session_summaries.covered_until_at < EXCLUDED.covered_until_at;

-- The messages of a session that are neither in the summary yet (created after
-- the covered position) nor in the prompt (created before its first message),
-- oldest first. Independent of how many messages the prompt reads.
-- name: GetMessagesToSummarize :many
SELECT * FROM messages
WHERE user_id = $1
  AND session_id = $2
  AND created_at > sqlc.arg(after)::timestamptz
  AND created_at < sqlc.arg(before)::timestamptz
ORDER BY created_at ASC
LIMIT $3;

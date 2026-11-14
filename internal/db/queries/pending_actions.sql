-- name: CreatePendingAction :one
-- The same open proposal made again returns the existing row.
INSERT INTO pending_actions (user_id, session_id, kind, payload, payload_hash, external_sources)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, payload_hash) WHERE status = 'pending'
DO UPDATE SET session_id = pending_actions.session_id
RETURNING *;

-- name: ListPendingActions :many
SELECT * FROM pending_actions
WHERE user_id = $1 AND status = 'pending'
ORDER BY created_at, id;

-- name: CountPendingActions :one
SELECT count(*) FROM pending_actions WHERE user_id = $1 AND status = 'pending';

-- name: DeleteStalePendingActions :execrows
DELETE FROM pending_actions
WHERE user_id = $1 AND status = 'pending' AND created_at < $2;

-- name: GetPendingAction :one
SELECT * FROM pending_actions WHERE id = $1 AND user_id = $2;

-- name: DecidePendingAction :one
-- Claims an open proposal. Only one caller can win, so a double tap cannot
-- apply it twice.
UPDATE pending_actions
SET status = $3, decided_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'pending'
RETURNING *;

-- name: ReopenPendingAction :exec
UPDATE pending_actions
SET status = 'pending', decided_at = NULL
WHERE id = $1 AND user_id = $2 AND status = $3;

-- name: ListGlobalRules :many
SELECT * FROM global_rules WHERE user_id = $1 ORDER BY updated_at DESC, id;

-- name: CountGlobalRules :one
SELECT count(*) FROM global_rules WHERE user_id = $1 AND kind = $2 AND enabled = true;

-- name: GlobalRuleExists :one
SELECT EXISTS (SELECT 1 FROM global_rules WHERE user_id = $1 AND kind = $2 AND key = $3);

-- name: DeleteGlobalRule :execrows
DELETE FROM global_rules WHERE id = $1 AND user_id = $2;

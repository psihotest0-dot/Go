-- name: CreateOracleEvaluation :one
INSERT INTO oracle_evaluations (
    request_id, signal_id, symbol, mode, status, verdict, reason, model,
    attempts, finish_reason, prompt_tokens, completion_tokens, latency_ms, error, request,
    prompt_version
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13, $14, $15,
    $16
)
RETURNING id, created_at;

-- The newest first, a page at a time: before_id 0 starts at the newest, and the
-- id of the last row of a page is the before_id of the next one. The request
-- body is left out (up to 64 KB a row); the list is for reading the verdicts.
-- name: ListOracleEvaluations :many
SELECT id, created_at, request_id, signal_id, symbol, mode, status, verdict, reason,
       model, attempts, finish_reason, prompt_tokens, completion_tokens, latency_ms, error,
       prompt_version, signal_version, served_from
FROM oracle_evaluations
WHERE (sqlc.arg(symbol)::text = '' OR symbol = sqlc.arg(symbol))
  AND (sqlc.arg(before_id)::bigint = 0 OR id < sqlc.arg(before_id))
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;

-- The light row of a call that did not reach the model (migration 33): the verdict of an
-- earlier call, repeated to another caller. It is written only when the signal has no row
-- in the mode of this call yet; the unique index on (signal_id, mode) of the served rows
-- settles two calls that arrive at once. The number of rows written is 0 or 1.
-- name: CreateServedOracleEvaluation :execrows
INSERT INTO oracle_evaluations (
    request_id, signal_id, symbol, mode, status, verdict, reason, model,
    attempts, finish_reason, prompt_tokens, completion_tokens, latency_ms, error, request,
    prompt_version, served_from
)
SELECT
    sqlc.arg(request_id)::text, sqlc.arg(signal_id)::text, sqlc.arg(symbol)::text, sqlc.arg(mode)::text,
    'answered', sqlc.arg(verdict)::text, sqlc.arg(reason)::text, sqlc.arg(model)::text,
    0, '', 0, 0, 0, '', sqlc.arg(request)::jsonb,
    sqlc.arg(prompt_version)::text, sqlc.arg(served_from)::text
WHERE NOT EXISTS (
    SELECT 1 FROM oracle_evaluations
    WHERE signal_id = sqlc.arg(signal_id)::text AND mode = sqlc.arg(mode)::text
)
ON CONFLICT (signal_id, mode) WHERE served_from IS NOT NULL DO NOTHING;

-- name: CreateOracleEvaluation :one
INSERT INTO oracle_evaluations (
    request_id, signal_id, symbol, mode, status, verdict, reason, model,
    attempts, finish_reason, prompt_tokens, completion_tokens, latency_ms, error, request
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13, $14, $15
)
RETURNING id, created_at;

-- The newest first, a page at a time: before_id 0 starts at the newest, and the
-- id of the last row of a page is the before_id of the next one. The request
-- body is left out (up to 64 KB a row); the list is for reading the verdicts.
-- name: ListOracleEvaluations :many
SELECT id, created_at, request_id, signal_id, symbol, mode, status, verdict, reason,
       model, attempts, finish_reason, prompt_tokens, completion_tokens, latency_ms, error
FROM oracle_evaluations
WHERE (sqlc.arg(symbol)::text = '' OR symbol = sqlc.arg(symbol))
  AND (sqlc.arg(before_id)::bigint = 0 OR id < sqlc.arg(before_id))
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;

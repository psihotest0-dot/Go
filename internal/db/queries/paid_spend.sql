-- name: AddPaidLLMSpend :exec
-- Adds what one request of the paid model cost to its day. Adding (not storing a total)
-- keeps the count right when two writers meet.
INSERT INTO paid_llm_spend (day, usd) VALUES (sqlc.arg('day')::text, sqlc.arg('usd')::float8)
ON CONFLICT (day) DO UPDATE SET usd = paid_llm_spend.usd + EXCLUDED.usd;

-- name: GetPaidLLMSpend :one
SELECT usd FROM paid_llm_spend WHERE day = $1;

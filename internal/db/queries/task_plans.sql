-- name: GetActiveTaskPlan :one
SELECT id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at
FROM task_plans
WHERE user_id = $1 AND session_id = $2 AND status = 'active';

-- The newest plan of the conversation, whatever became of it.
-- name: GetLatestTaskPlan :one
SELECT id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at
FROM task_plans
WHERE user_id = $1 AND session_id = $2
ORDER BY id DESC
LIMIT 1;

-- name: CancelActiveTaskPlan :exec
UPDATE task_plans SET status = 'cancelled', updated_at = now()
WHERE user_id = $1 AND session_id = $2 AND status = 'active';

-- name: CreateTaskPlan :one
INSERT INTO task_plans (user_id, session_id, goal, steps, ledger)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at;

-- name: UpdateTaskPlan :exec
UPDATE task_plans SET steps = $3, ledger = $4, status = $5, updated_at = now()
WHERE id = $1 AND user_id = $2;

-- Only the newest keep_count plans of the conversation stay.
-- name: TrimTaskPlans :exec
DELETE FROM task_plans t
WHERE t.user_id = $1 AND t.session_id = $2
  AND t.id NOT IN (
      SELECT p.id FROM task_plans p
      WHERE p.user_id = $1 AND p.session_id = $2
      ORDER BY p.id DESC
      LIMIT sqlc.arg(keep_count)::int
  );

-- name: DeleteSessionTaskPlans :execrows
DELETE FROM task_plans WHERE user_id = $1 AND session_id = $2;

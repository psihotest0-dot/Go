-- The plan of the model (kind ''): the plans the server walks have their own queries below.
-- name: GetActiveTaskPlan :one
SELECT id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at, kind
FROM task_plans
WHERE user_id = $1 AND session_id = $2 AND status = 'active' AND kind = '';

-- The newest plan of the model in the conversation, whatever became of it.
-- name: GetLatestTaskPlan :one
SELECT id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at, kind
FROM task_plans
WHERE user_id = $1 AND session_id = $2 AND kind = '' AND status <> 'proposed'
ORDER BY id DESC
LIMIT 1;

-- name: CancelActiveTaskPlan :exec
UPDATE task_plans SET status = 'cancelled', updated_at = now()
WHERE user_id = $1 AND session_id = $2 AND status = 'active' AND kind = '';

-- name: CreateTaskPlan :one
INSERT INTO task_plans (user_id, session_id, goal, steps, ledger)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at, kind;

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

-- The plans the server walks (kind <> '').

-- A new proposal takes the place of one that waits in the conversation.
-- name: DropProposedTaskPlan :exec
DELETE FROM task_plans WHERE user_id = $1 AND session_id = $2 AND status = 'proposed';

-- name: CreateProposedTaskPlan :one
INSERT INTO task_plans (user_id, session_id, goal, steps, ledger, status, kind)
VALUES ($1, $2, $3, $4, '[]', 'proposed', $5)
RETURNING id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at, kind;

-- name: GetProposedTaskPlan :one
SELECT id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at, kind
FROM task_plans
WHERE user_id = $1 AND session_id = $2 AND status = 'proposed';

-- The yes of the person: the proposal becomes the plan the server walks. No row: it was not
-- waiting any more (answered, replaced), or a plan of the same kind is being walked already.
-- name: StartProposedTaskPlan :one
UPDATE task_plans p SET status = 'active', updated_at = now()
WHERE p.id = $1 AND p.user_id = $2 AND p.status = 'proposed'
  AND NOT EXISTS (
      SELECT 1 FROM task_plans a
      WHERE a.user_id = p.user_id AND a.session_id = p.session_id AND a.kind = p.kind AND a.status = 'active'
  )
RETURNING p.id, p.user_id, p.session_id, p.goal, p.steps, p.ledger, p.status, p.created_at, p.updated_at, p.kind;

-- The plans the server is walking, the oldest first.
-- name: ListRunningTaskPlans :many
SELECT id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at, kind
FROM task_plans
WHERE status = 'active' AND kind <> ''
ORDER BY id;

-- name: GetRunningTaskPlan :one
SELECT id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at, kind
FROM task_plans
WHERE user_id = $1 AND session_id = $2 AND status = 'active' AND kind <> ''
ORDER BY id
LIMIT 1;

-- The newest plan the server walked or walks in the conversation (not a proposal).
-- name: GetLatestRunTaskPlan :one
SELECT id, user_id, session_id, goal, steps, ledger, status, created_at, updated_at, kind
FROM task_plans
WHERE user_id = $1 AND session_id = $2 AND kind <> '' AND status <> 'proposed'
ORDER BY id DESC
LIMIT 1;

-- The steps of a plan the server walks, written after each of them, and its end. Only while the
-- plan is active: a plan the person stopped stays stopped (no row).
-- name: SaveRunningTaskPlan :execrows
UPDATE task_plans SET steps = $3, status = $4, updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'active' AND kind <> '';

-- name: CancelRunningTaskPlans :execrows
UPDATE task_plans SET status = 'cancelled', updated_at = now()
WHERE user_id = $1 AND session_id = $2 AND status = 'active' AND kind <> '';

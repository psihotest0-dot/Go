-- name: DropProposedBill :execrows
DELETE FROM bills WHERE user_id = $1 AND session_id = $2 AND status = 'proposed';

-- name: ProposeBill :one
INSERT INTO bills (user_id, session_id, issuer, kind, amount_cents, currency, invoice_date, due_date, paid_date,
                   payment, repeat, summary, file_name, file_hash, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 'proposed')
RETURNING *;

-- name: GetProposedBill :one
SELECT * FROM bills WHERE user_id = $1 AND session_id = $2 AND status = 'proposed';

-- name: KeepBill :one
-- The person's yes: only a proposal that is still there is kept, once.
UPDATE bills SET status = 'kept', reminder_id = $3, updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'proposed'
RETURNING *;

-- name: KeptBillWithFile :one
SELECT * FROM bills WHERE user_id = $1 AND file_hash = $2 AND status = 'kept';

-- name: SimilarKeptBills :many
-- The same bill from another file (a copy, another page): the same amount and one of its dates.
SELECT * FROM bills
WHERE user_id = $1 AND status = 'kept' AND amount_cents = $2
  AND (invoice_date IS NOT DISTINCT FROM sqlc.narg(invoice_date)::date
       OR (sqlc.narg(due_date)::date IS NOT NULL AND due_date = sqlc.narg(due_date)::date)
       OR (sqlc.narg(paid_date)::date IS NOT NULL AND paid_date = sqlc.narg(paid_date)::date)
       -- A page without a date of its own (the second page of a bill): the amount is enough.
       OR (sqlc.narg(invoice_date)::date IS NULL AND sqlc.narg(due_date)::date IS NULL AND sqlc.narg(paid_date)::date IS NULL))
ORDER BY id
LIMIT 3;

-- name: ListKeptBills :many
SELECT * FROM bills WHERE user_id = $1 AND status = 'kept'
ORDER BY COALESCE(due_date, invoice_date, paid_date) DESC NULLS LAST, id DESC
LIMIT $2;

-- name: MarkBillPaid :execrows
UPDATE bills SET paid_date = $3, updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'kept' AND paid_date IS NULL;

-- name: DeleteStaleProposedBills :execrows
DELETE FROM bills WHERE status = 'proposed' AND created_at < $1;

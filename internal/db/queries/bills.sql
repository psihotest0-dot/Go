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

-- name: KeptBillsBetween :many
-- The bills of a period, by their day: the due day, else the day of the bill, else the day paid.
SELECT * FROM bills
WHERE user_id = $1 AND status = 'kept'
  AND COALESCE(due_date, invoice_date, paid_date) BETWEEN sqlc.arg(from_day)::date AND sqlc.arg(to_day)::date
ORDER BY COALESCE(due_date, invoice_date, paid_date), id;

-- name: CountKeptBills :one
SELECT count(*) FROM bills WHERE user_id = $1 AND status = 'kept';

-- name: UsersWithKeptBills :many
SELECT DISTINCT user_id FROM bills WHERE status = 'kept';

-- name: LatestBillSession :one
SELECT session_id FROM bills WHERE user_id = $1 AND status = 'kept' ORDER BY created_at DESC LIMIT 1;

-- name: ClaimBillReport :execrows
-- The report of a month is claimed once; a second claim changes nothing.
INSERT INTO bill_reports (user_id, month, session_id) VALUES ($1, $2, $3)
ON CONFLICT (user_id, month) DO NOTHING;

-- name: UnclaimBillReport :exec
DELETE FROM bill_reports WHERE user_id = $1 AND month = $2;

-- name: DropProposedStanding :execrows
DELETE FROM standing_bills WHERE user_id = $1 AND session_id = $2 AND status = 'proposed';

-- name: ProposeStanding :one
INSERT INTO standing_bills (user_id, session_id, issuer, kind, amount_cents, currency, day, month, payment, summary, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'proposed')
RETURNING *;

-- name: GetProposedStanding :one
SELECT * FROM standing_bills WHERE user_id = $1 AND session_id = $2 AND status = 'proposed';

-- name: KeepStanding :one
-- The person's yes: only a proposal that is still there is kept, once.
UPDATE standing_bills SET status = 'kept', updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'proposed'
RETURNING *;

-- name: ListKeptStanding :many
SELECT * FROM standing_bills WHERE user_id = $1 AND status = 'kept' ORDER BY id;

-- name: AllKeptStanding :many
SELECT * FROM standing_bills WHERE status = 'kept' ORDER BY id;

-- name: DeleteStanding :execrows
DELETE FROM standing_bills WHERE id = $1 AND user_id = $2 AND status = 'kept';

-- name: InsertStandingPayment :one
-- The payment of a standing bill on one of its days, written once (the file_hash of the day).
INSERT INTO bills (user_id, session_id, issuer, kind, amount_cents, currency, due_date, payment, repeat, summary,
                   file_name, file_hash, status, standing_id, remind_days_before)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'kept', $13, $14)
ON CONFLICT (user_id, file_hash) WHERE status = 'kept' DO NOTHING
RETURNING *;

-- name: SetBillReminder :exec
UPDATE bills SET reminder_id = $3, updated_at = now() WHERE id = $1 AND user_id = $2;

-- name: UpcomingStandingPayments :many
-- The payments of a standing bill still to come and not paid: they go with it when it is removed.
SELECT * FROM bills
WHERE user_id = $1 AND standing_id = $2 AND status = 'kept' AND paid_date IS NULL AND due_date >= sqlc.arg(from_day)::date;

-- name: StandingPaymentsOfKindBetween :many
-- The payments of standing bills of a kind in a span of days: a document of the same kind and month
-- takes their place.
SELECT * FROM bills
WHERE user_id = $1 AND status = 'kept' AND standing_id IS NOT NULL AND kind = $2
  AND due_date BETWEEN sqlc.arg(from_day)::date AND sqlc.arg(to_day)::date;

-- name: DeleteBill :execrows
DELETE FROM bills WHERE id = $1 AND user_id = $2;

-- name: UpdateProposedBill :one
-- The person's answer to the proposal changes it: the due day, the amount, the way it is paid, the
-- days of its reminder.
UPDATE bills SET due_date = $3, amount_cents = $4, payment = $5, remind_days_before = $6, updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'proposed'
RETURNING *;

-- name: UpdateProposedStanding :one
UPDATE standing_bills SET amount_cents = $3, payment = $4, remind_days_before = $5, day = $6, month = $7, until = sqlc.narg(until)::date, updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'proposed'
RETURNING *;
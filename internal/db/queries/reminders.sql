-- name: ProposeReminder :one
-- The proposal of a conversation; an earlier one of the same conversation is dropped first
-- (DropProposedReminder), in the same transaction.
INSERT INTO reminders (user_id, session_id, text, repeat, first_date, at_minute, day_before, status, next_at, next_early)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'proposed', $8, $9)
RETURNING *;

-- name: DropProposedReminder :execrows
DELETE FROM reminders WHERE user_id = $1 AND session_id = $2 AND status = 'proposed';

-- name: GetProposedReminder :one
SELECT * FROM reminders WHERE user_id = $1 AND session_id = $2 AND status = 'proposed';

-- name: ActivateReminder :one
-- The person's yes: only a proposal that is still there becomes active, once.
UPDATE reminders
SET status = 'active', next_at = $3, next_early = $4, updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'proposed'
RETURNING *;

-- name: ListActiveReminders :many
SELECT * FROM reminders WHERE user_id = $1 AND status = 'active' ORDER BY next_at, id;

-- name: CancelReminder :execrows
UPDATE reminders SET status = 'cancelled', updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'active';

-- name: DueReminders :many
-- The reminders to send now. The row stays locked until the transaction that sends it ends, so two
-- senders never send one reminder twice.
SELECT * FROM reminders
WHERE status = 'active' AND next_at <= $1
ORDER BY next_at, id
LIMIT $2
FOR UPDATE SKIP LOCKED;

-- name: MarkReminderSent :exec
-- After a sending: the next time, or 'done' for a reminder that is not repeated.
UPDATE reminders
SET status = $2, next_at = $3, next_early = $4, sent_count = sent_count + 1, last_sent_at = now(), updated_at = now()
WHERE id = $1;

-- name: DeleteOldReminders :execrows
-- The proposals nobody answered and the reminders that are over are kept a while, then removed.
DELETE FROM reminders
WHERE (status = 'proposed' AND created_at < $1) OR (status IN ('done', 'cancelled') AND updated_at < $2);

-- name: TelegramChatOfUser :one
-- The chat a reminder goes to when it was made in a conversation of the app only.
SELECT telegram_chat_id FROM telegram_users WHERE user_id = $1 ORDER BY telegram_chat_id LIMIT 1;

-- name: CreateActiveReminder :one
-- A reminder the person already said yes to (the due day of a bill they kept).
INSERT INTO reminders (user_id, session_id, text, repeat, first_date, at_minute, day_before, status, next_at, next_early)
VALUES ($1, $2, $3, '', $4, $5, false, 'active', $6, false)
RETURNING *;

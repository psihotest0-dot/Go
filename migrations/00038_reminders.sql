-- +goose Up
-- +goose StatementBegin
-- Reminders (plan item И8, 2026-11-24): "напомни ми утре в 9 да платя тока", "всяка сутрин в 8 …",
-- "рожденият ден на мама е на 12 март" (every year, the day before and on the day).
--
-- The model only turns the person's words into the fields of a reminder; the code checks the date,
-- says it back ("в сряда, 25.11.2026, 09:00 — така ли?") and stores the reminder as 'active' only
-- after the person's yes. Until then it is 'proposed': one proposal per conversation at a time.
--
-- The time is the person's wall clock (Europe/Berlin): first_date and at_minute are local, so a
-- reminder "every day at 08:00" stays at 08:00 across the change of summer time. next_at is the
-- next time to send, computed by the code from them; next_early says that this sending is the one
-- of the day before (day_before). A reminder that is not repeated is 'done' once it is sent.
CREATE TABLE reminders (
    id           BIGSERIAL PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id   TEXT        NOT NULL CHECK (session_id <> ''),
    text         TEXT        NOT NULL CHECK (length(text) BETWEEN 1 AND 300),
    repeat       TEXT        NOT NULL DEFAULT '' CHECK (repeat IN ('', 'daily', 'weekdays', 'weekly', 'monthly', 'yearly')),
    first_date   DATE        NOT NULL,
    at_minute    INTEGER     NOT NULL CHECK (at_minute BETWEEN 0 AND 1439),
    day_before   BOOLEAN     NOT NULL DEFAULT false,
    status       TEXT        NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'active', 'done', 'cancelled')),
    next_at      TIMESTAMPTZ NOT NULL,
    next_early   BOOLEAN     NOT NULL DEFAULT false,
    sent_count   INTEGER     NOT NULL DEFAULT 0,
    last_sent_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_reminders_proposed ON reminders (user_id, session_id) WHERE status = 'proposed';
-- The sender reads the active reminders that are due, the oldest first.
CREATE INDEX idx_reminders_due ON reminders (next_at) WHERE status = 'active';
-- "какви напомняния имам?"
CREATE INDEX idx_reminders_user_active ON reminders (user_id, next_at) WHERE status = 'active';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS reminders;
-- +goose StatementEnd

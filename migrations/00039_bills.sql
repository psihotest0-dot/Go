-- +goose Up
-- +goose StatementBegin
-- Bills (the owner, 2026-11-24): a PDF from an e-mail or a scanned paper bill is read into a record,
-- said back to the person and kept after "да", with a reminder of its due day when one is needed;
-- the sums of the bills come from these rows (internal/bills).
--
-- The model reads the text into fields; the code keeps the amount and a date only when the text of
-- the document holds them. amount_cents is the total to pay (or paid). payment says what the person
-- has to do: 'transfer' - pay it; 'direct_debit' - the issuer takes it (have the money there);
-- 'paid' - nothing. paid_date is when it was paid (from the document or "платих …").
--
-- 'proposed' is a bill read and not yet confirmed: one per conversation. file_hash is the SHA-256
-- of the uploaded file: the same file is never kept twice.
CREATE TABLE bills (
    id           BIGSERIAL PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id   TEXT        NOT NULL CHECK (session_id <> ''),
    issuer       TEXT        NOT NULL CHECK (length(issuer) <= 80),
    kind         TEXT        NOT NULL CHECK (kind IN ('phone_internet', 'electricity', 'gas', 'water', 'heating', 'rent',
                                   'insurance', 'doctor', 'dentist', 'pharmacy', 'car', 'tax', 'subscription', 'shopping', 'other')),
    amount_cents BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency     TEXT        NOT NULL DEFAULT 'EUR' CHECK (currency <> ''),
    invoice_date DATE,
    due_date     DATE,
    paid_date    DATE,
    payment      TEXT        NOT NULL CHECK (payment IN ('direct_debit', 'transfer', 'paid', 'unknown')),
    repeat       TEXT        NOT NULL CHECK (repeat IN ('monthly', 'quarterly', 'yearly', 'once', 'unknown')),
    summary      TEXT        NOT NULL DEFAULT '' CHECK (length(summary) <= 120),
    file_name    TEXT        NOT NULL,
    file_hash    TEXT        NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'kept')),
    reminder_id  BIGINT      REFERENCES reminders(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_bills_proposed ON bills (user_id, session_id) WHERE status = 'proposed';
CREATE UNIQUE INDEX idx_bills_file ON bills (user_id, file_hash) WHERE status = 'kept';
-- The sums by month and the list of the unpaid ones.
CREATE INDEX idx_bills_user_dates ON bills (user_id, COALESCE(due_date, invoice_date, paid_date)) WHERE status = 'kept';
CREATE INDEX idx_bills_reminder ON bills (reminder_id) WHERE reminder_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS bills;
-- +goose StatementEnd

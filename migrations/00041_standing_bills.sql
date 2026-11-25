-- +goose Up
-- +goose StatementBegin
-- Standing bills (the owner, 2026-11-25): "запиши, че на всяко 28-мо число имам сметка от … към …"
-- is a bill with no document that comes every month (or every year) on the same day. Asked in the
-- chat it became a note in a text file of the assistant, which the code never reads; now it is a row
-- here, kept after "да".
--
-- A standing bill is not summed itself: a few days before each of its days the code writes the
-- payment of that day into bills (standing_id, file_hash "standing:<id>:<yyyy-mm>", so it is written
-- once), with the reminder of the day before, and the sums, the lists, the report and "платих …" see
-- it as any other bill. day is the day of the month (31 is the last day of a shorter month); month is
-- 0 for a monthly bill and the month of a yearly one. The amount and the day come from the person's
-- words, checked by the code; the model only names the issuer and the kind.
CREATE TABLE standing_bills (
    id           BIGSERIAL PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id   TEXT        NOT NULL CHECK (session_id <> ''),
    issuer       TEXT        NOT NULL CHECK (length(issuer) <= 80),
    kind         TEXT        NOT NULL CHECK (kind IN ('phone_internet', 'electricity', 'gas', 'water', 'heating', 'rent',
                                   'insurance', 'doctor', 'dentist', 'pharmacy', 'car', 'tax', 'subscription', 'shopping', 'other')),
    amount_cents BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency     TEXT        NOT NULL DEFAULT 'EUR' CHECK (currency <> ''),
    day          INT         NOT NULL CHECK (day BETWEEN 1 AND 31),
    month        INT         NOT NULL DEFAULT 0 CHECK (month BETWEEN 0 AND 12),
    payment      TEXT        NOT NULL CHECK (payment IN ('direct_debit', 'transfer', 'unknown')),
    summary      TEXT        NOT NULL DEFAULT '' CHECK (length(summary) <= 120),
    status       TEXT        NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'kept')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_standing_bills_proposed ON standing_bills (user_id, session_id) WHERE status = 'proposed';
CREATE INDEX idx_standing_bills_kept ON standing_bills (user_id) WHERE status = 'kept';

-- The payment of a standing bill on one of its days. A standing bill that is removed leaves its
-- payments of the days gone in the sums (SET NULL); the ones still to come are deleted by the code.
ALTER TABLE bills ADD COLUMN standing_id BIGINT REFERENCES standing_bills(id) ON DELETE SET NULL;
CREATE INDEX idx_bills_standing ON bills (standing_id) WHERE standing_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_bills_standing;
ALTER TABLE bills DROP COLUMN IF EXISTS standing_id;
DROP TABLE IF EXISTS standing_bills;
-- +goose StatementEnd

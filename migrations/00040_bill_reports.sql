-- +goose Up
-- +goose StatementBegin
-- The monthly report of the bills (phase 2, 2026-11-24): one row per person and month it was sent
-- for, so that the report goes out once even when the backend starts many times that month.
CREATE TABLE bill_reports (
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    month      DATE        NOT NULL CHECK (EXTRACT(DAY FROM month) = 1),
    session_id TEXT        NOT NULL,
    sent_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, month)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS bill_reports;
-- +goose StatementEnd

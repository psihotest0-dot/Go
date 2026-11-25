-- +goose Up
-- +goose StatementBegin
-- The last day of a standing bill (2026-11-25): "всеки месец на 1-во … до 01.05.2028 включително"
-- (a lease) - no payment is written after it. NULL: it goes on until it is removed.
ALTER TABLE standing_bills ADD COLUMN until DATE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE standing_bills DROP COLUMN IF EXISTS until;
-- +goose StatementEnd

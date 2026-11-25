-- +goose Up
-- +goose StatementBegin
-- The bills, 2026-11-25 evening (the owner: "всичките"):
--
-- A kind of its own for a loan or a lease: a credit statement and a car finance plan were read as
-- "insurance".
--
-- remind_days_before: how many days before its due day the reminder of a bill comes, at 09:00; -1
-- for none. The default is the day before, as it was. The person sets it in the answer to the
-- proposal ("напомни ми седмица по-рано", "без напомняне"), which before this was dropped with the
-- proposal while the model said it had done it.
ALTER TABLE bills DROP CONSTRAINT bills_kind_check;
ALTER TABLE bills ADD CONSTRAINT bills_kind_check CHECK (kind IN ('phone_internet', 'electricity', 'gas', 'water', 'heating', 'rent',
    'insurance', 'doctor', 'dentist', 'pharmacy', 'car', 'tax', 'subscription', 'shopping', 'loan', 'other'));
ALTER TABLE standing_bills DROP CONSTRAINT standing_bills_kind_check;
ALTER TABLE standing_bills ADD CONSTRAINT standing_bills_kind_check CHECK (kind IN ('phone_internet', 'electricity', 'gas', 'water', 'heating', 'rent',
    'insurance', 'doctor', 'dentist', 'pharmacy', 'car', 'tax', 'subscription', 'shopping', 'loan', 'other'));

ALTER TABLE bills ADD COLUMN remind_days_before INT NOT NULL DEFAULT 1 CHECK (remind_days_before BETWEEN -1 AND 60);
ALTER TABLE standing_bills ADD COLUMN remind_days_before INT NOT NULL DEFAULT 1 CHECK (remind_days_before BETWEEN -1 AND 60);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE standing_bills DROP COLUMN IF EXISTS remind_days_before;
ALTER TABLE bills DROP COLUMN IF EXISTS remind_days_before;
UPDATE bills SET kind = 'other' WHERE kind = 'loan';
UPDATE standing_bills SET kind = 'other' WHERE kind = 'loan';
ALTER TABLE bills DROP CONSTRAINT bills_kind_check;
ALTER TABLE bills ADD CONSTRAINT bills_kind_check CHECK (kind IN ('phone_internet', 'electricity', 'gas', 'water', 'heating', 'rent',
    'insurance', 'doctor', 'dentist', 'pharmacy', 'car', 'tax', 'subscription', 'shopping', 'other'));
ALTER TABLE standing_bills DROP CONSTRAINT standing_bills_kind_check;
ALTER TABLE standing_bills ADD CONSTRAINT standing_bills_kind_check CHECK (kind IN ('phone_internet', 'electricity', 'gas', 'water', 'heating', 'rent',
    'insurance', 'doctor', 'dentist', 'pharmacy', 'car', 'tax', 'subscription', 'shopping', 'other'));
-- +goose StatementEnd

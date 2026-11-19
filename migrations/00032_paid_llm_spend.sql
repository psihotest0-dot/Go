-- +goose Up
-- What the paid model has cost on each local calendar day, at the peak prices (an upper
-- bound, which is what the daily limit wants). The limit used to be seeded at start from
-- the Oracle's journal only, so a restart forgot what the chat had spent that day. One row
-- per day, a handful of rows a year; `day` is the date as the process counts it
-- (YYYY-MM-DD, local), as text so that no time zone can move it.
CREATE TABLE paid_llm_spend (
    day text PRIMARY KEY CHECK (day ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'),
    usd double precision NOT NULL CHECK (usd >= 0)
);

-- +goose Down
DROP TABLE paid_llm_spend;

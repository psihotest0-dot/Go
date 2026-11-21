-- +goose Up
-- +goose StatementBegin
-- A light row of the Oracle's journal for a call that did not reach the model.
--
-- A verdict is kept for a signal (30 minutes, 3 for a HOLD) and given again to whoever asks
-- about the same signal_id, whatever its mode. Since the trading bot asks the same signal in
-- two modes (a shadow ask for the evaluation, an active ask from its paper container), the
-- one that comes second used to leave no row at all: the journal then said that the signal
-- was asked in one mode only, and the counters of the other mode missed it.
--
-- served_from says where the verdict of such a row came from: 'cache' (it was given a
-- moment ago) or 'joined' (it was still being asked, and the call waited for it). NULL, as
-- in every row before this migration, is a call the model was asked about. A served row
-- copies the verdict, the reason, the model and the prompt label of the answer it repeats;
-- it has no attempts, tokens or latency of its own, and 'mode' is the mode of the CALL, not
-- of the call that asked the model.
--
-- There is at most one served row for a (signal_id, mode): the repeats of a HOLD, asked
-- again every minute, must not fill the journal. The unique index is what makes that hold
-- when two calls arrive at once; the insert also skips the signal that already has a row of
-- any kind in that mode.
ALTER TABLE oracle_evaluations
    ADD COLUMN served_from TEXT
        CONSTRAINT oracle_evaluations_served_from_values CHECK (served_from IN ('cache', 'joined')),
    ADD CONSTRAINT oracle_evaluations_served_shape
        CHECK (served_from IS NULL OR (status = 'answered' AND attempts = 0));

CREATE UNIQUE INDEX idx_oracle_evaluations_served ON oracle_evaluations (signal_id, mode)
    WHERE served_from IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_oracle_evaluations_served;
ALTER TABLE oracle_evaluations
    DROP CONSTRAINT IF EXISTS oracle_evaluations_served_shape,
    DROP COLUMN IF EXISTS served_from;
-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin
-- The journal of the Oracle: every question the trading bot asked and every
-- answer the model gave (or why it gave none). In shadow mode this table is the
-- whole result: the bot ignores the verdicts, and the owner reads them here
-- later and compares them with what the signals went on to do.
--
-- It has no user_id on purpose. The caller is the bot, identified by the Oracle
-- token, not a user of the chat, and the system has one owner. The rows are read
-- through the ordinary user-scoped API; nothing in them is secret (market data
-- and cleaned news headlines).
CREATE TABLE oracle_evaluations (
    id                BIGSERIAL PRIMARY KEY,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The X-Request-ID of the call, to find the log lines of it.
    request_id        TEXT        NOT NULL DEFAULT '',
    -- The bot's own id of the signal; one signal can be asked about many times
    -- (a HOLD is asked again).
    signal_id         TEXT        NOT NULL CHECK (signal_id <> ''),
    symbol            TEXT        NOT NULL CHECK (symbol <> ''),
    -- What the bot does with the verdict, as it said it: '' when it did not say.
    mode              TEXT        NOT NULL DEFAULT '' CHECK (mode IN ('', 'shadow', 'active')),
    -- 'answered': the model gave a verdict. 'failed': it did not (unreachable,
    -- cut off, unreadable twice); the bot treats that as a HOLD.
    status            TEXT        NOT NULL CHECK (status IN ('answered', 'failed')),
    verdict           TEXT        CHECK (verdict IN ('APPROVE', 'VETO', 'HOLD')),
    reason            TEXT        NOT NULL DEFAULT '',
    model             TEXT        NOT NULL DEFAULT '',
    -- How many times the model was called for this answer (1 or 2).
    attempts          SMALLINT    NOT NULL DEFAULT 1 CHECK (attempts >= 0),
    finish_reason     TEXT        NOT NULL DEFAULT '',
    prompt_tokens     INTEGER     NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens INTEGER     NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    latency_ms        INTEGER     NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
    error             TEXT        NOT NULL DEFAULT '',
    -- The body the bot sent, as received (at most 64 KB).
    request           JSONB       NOT NULL,
    -- A verdict exists exactly when the model answered.
    CONSTRAINT oracle_evaluations_status_verdict CHECK ((status = 'answered') = (verdict IS NOT NULL))
);

CREATE INDEX idx_oracle_evaluations_created ON oracle_evaluations (created_at DESC, id DESC);
CREATE INDEX idx_oracle_evaluations_signal ON oracle_evaluations (signal_id, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS oracle_evaluations;
-- +goose StatementEnd
